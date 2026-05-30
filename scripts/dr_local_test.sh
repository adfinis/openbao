#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TOPOLOGY="${DR_TOPOLOGY:-single}"
COMPOSE_FILE="${DR_COMPOSE_FILE:-}"
ENV_FILE="${DR_LOCAL_ENV_FILE:-${ROOT_DIR}/.dr-test.env}"
RESULTS_DIR="${DR_RESULTS_DIR:-${ROOT_DIR}/dr-stress-results}"
DR_STRESS_BIN="${DR_STRESS_BIN:-${ROOT_DIR}/bin/dr-stress}"

PRIMARY_ADDR="${DR_PRIMARY_ADDR:-http://localhost:8800}"
SECONDARY1_ADDR="${DR_SECONDARY1_ADDR:-http://localhost:8900}"
SECONDARY2_ADDR="${DR_SECONDARY2_ADDR:-http://localhost:9000}"

PRIMARY_NODE_ADDRS=()
SECONDARY1_NODE_ADDRS=()
SECONDARY2_NODE_ADDRS=()
PRIMARY_SERVICES=()
EXPECTED_RAFT_PEERS=1

usage() {
  cat <<'USAGE'
Manage the local DR replication test environment.

Usage:
  scripts/dr_local_test.sh [--topology single|ha] up [--build]
  scripts/dr_local_test.sh [--topology single|ha] bootstrap
  scripts/dr_local_test.sh [--topology single|ha] configure
  scripts/dr_local_test.sh [--topology single|ha] reset [--build]
  scripts/dr_local_test.sh [--topology single|ha] status
  scripts/dr_local_test.sh [--topology single|ha] smoke [dr-stress flags...]
  scripts/dr_local_test.sh [--topology single|ha] verify [RUN_DIR] [--sample N]
  scripts/dr_local_test.sh [--topology single|ha] failover-smoke
  scripts/dr_local_test.sh --topology ha promoted-durability-smoke
  scripts/dr_local_test.sh --topology ha reseed-secondary-smoke
  scripts/dr_local_test.sh --topology ha failover-load-lifecycle [--duration N] [--concurrency N] [--hard-stop-after N] [--no-reset]
  scripts/dr_local_test.sh [--topology single|ha] down
  scripts/dr_local_test.sh [--topology single|ha] logs [service]

Defaults:
  topology:     single
  compose file: docker-compose.dr-test.yml or docker-compose.dr-ha-test.yml
  env file:     .dr-test.env
  results:      dr-stress-results

Typical flow:
  make docker-dev
  scripts/dr_local_test.sh reset
  scripts/dr_local_test.sh smoke --duration 120 --concurrency 24
  scripts/dr_local_test.sh verify

HA topology:
  scripts/dr_local_test.sh --topology ha reset
  scripts/dr_local_test.sh --topology ha smoke --duration 900 --concurrency 48 --stepdown-interval 300
  scripts/dr_local_test.sh --topology ha failover-load-lifecycle
USAGE
}

die() {
  echo "ERROR: $*" >&2
  exit 1
}

need_bin() {
  command -v "$1" >/dev/null 2>&1 || die "missing required binary: $1"
}

configure_topology() {
  case "$TOPOLOGY" in
    single)
      COMPOSE_FILE="${COMPOSE_FILE:-${ROOT_DIR}/docker-compose.dr-test.yml}"
      PRIMARY_NODE_ADDRS=("$PRIMARY_ADDR")
      SECONDARY1_NODE_ADDRS=("$SECONDARY1_ADDR")
      SECONDARY2_NODE_ADDRS=("$SECONDARY2_ADDR")
      PRIMARY_SERVICES=(primary)
      EXPECTED_RAFT_PEERS=1
      ;;
    ha)
      COMPOSE_FILE="${COMPOSE_FILE:-${ROOT_DIR}/docker-compose.dr-ha-test.yml}"
      PRIMARY_NODE_ADDRS=(
        "${DR_PRIMARY_NODE1_ADDR:-$PRIMARY_ADDR}"
        "${DR_PRIMARY_NODE2_ADDR:-http://localhost:8802}"
        "${DR_PRIMARY_NODE3_ADDR:-http://localhost:8804}"
      )
      SECONDARY1_NODE_ADDRS=(
        "${DR_SECONDARY1_NODE1_ADDR:-$SECONDARY1_ADDR}"
        "${DR_SECONDARY1_NODE2_ADDR:-http://localhost:8902}"
        "${DR_SECONDARY1_NODE3_ADDR:-http://localhost:8904}"
      )
      SECONDARY2_NODE_ADDRS=(
        "${DR_SECONDARY2_NODE1_ADDR:-$SECONDARY2_ADDR}"
        "${DR_SECONDARY2_NODE2_ADDR:-http://localhost:9002}"
        "${DR_SECONDARY2_NODE3_ADDR:-http://localhost:9004}"
      )
      PRIMARY_SERVICES=(primary-1 primary-2 primary-3)
      EXPECTED_RAFT_PEERS=3
      ;;
    *)
      die "unknown topology: $TOPOLOGY"
      ;;
  esac
}

bao_bin() {
  if [[ -x "${BAO_BIN:-}" ]]; then
    printf "%s" "$BAO_BIN"
    return
  fi
  if [[ -x "${ROOT_DIR}/bin/bao" ]]; then
    printf "%s" "${ROOT_DIR}/bin/bao"
    return
  fi
  command -v bao >/dev/null 2>&1 || die "missing bao binary; run make dev or set BAO_BIN"
  command -v bao
}

compose() {
  need_bin docker
  docker compose -f "$COMPOSE_FILE" "$@"
}

bao() {
  "$(bao_bin)" "$@"
}

bao_for() {
  local addr="$1"
  local token="${2:-}"
  shift 2 || true
  if [[ -n "$token" ]]; then
    env BAO_ADDR="$addr" BAO_TOKEN="$token" "$(bao_bin)" "$@"
  else
    env BAO_ADDR="$addr" "$(bao_bin)" "$@"
  fi
}

status_json() {
  local addr="$1"
  local out
  out="$(bao_for "$addr" "" status -format=json 2>&1)" || true
  if jq -e '.initialized != null and .sealed != null' >/dev/null 2>&1 <<<"$out"; then
    printf "%s\n" "$out"
    return 0
  fi
  return 1
}

wait_status() {
  local name="$1"
  local addr="$2"
  local timeout="${3:-120}"
  local deadline=$(( $(date +%s) + timeout ))
  while true; do
    if status_json "$addr" >/dev/null; then
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      die "timed out waiting for ${name} status endpoint at ${addr}"
    fi
    sleep 2
  done
}

init_and_unseal() {
  local name="$1"
  local addr="$2"
  local init_file="$3"
  local key_env_name="${4:-}"
  local status initialized sealed
  local key=""

  wait_status "$name" "$addr" 180
  status="$(status_json "$addr")"
  initialized="$(jq -r '.initialized' <<<"$status")"
  sealed="$(jq -r '.sealed' <<<"$status")"

  if [[ "$initialized" != "true" ]]; then
    echo "Initializing ${name}..."
    bao_for "$addr" "" operator init -key-shares=1 -key-threshold=1 -format=json >"$init_file"
    key="$(jq -r '.unseal_keys_b64[0]' "$init_file")"
    sealed="true"
  else
    echo "${name} already initialized."
    if [[ -s "$init_file" ]]; then
      key="$(jq -r '.unseal_keys_b64[0]' "$init_file")"
    elif [[ -n "$key_env_name" ]]; then
      key="${!key_env_name:-}"
    fi
    if [[ "$sealed" == "true" && -z "$key" ]]; then
      die "${name} is already initialized but no unseal key is available; run reset for a fresh local environment"
    fi
  fi

  if [[ "$sealed" == "true" ]]; then
    echo "Unsealing ${name}..."
    bao_for "$addr" "" operator unseal "$key" >/dev/null
  fi
}

unseal_with_key() {
  local name="$1"
  local addr="$2"
  local key="$3"
  local status sealed

  wait_status "$name" "$addr" 180
  status="$(status_json "$addr")"
  sealed="$(jq -r '.sealed' <<<"$status")"
  if [[ "$sealed" == "true" ]]; then
    echo "Unsealing ${name}..."
    bao_for "$addr" "" operator unseal "$key" >/dev/null
  fi
}

wait_raft_peers() {
  local name="$1"
  local addr="$2"
  local token="$3"
  local expected="$4"
  local timeout="${5:-240}"
  local deadline=$(( $(date +%s) + timeout ))
  local out servers leaders voters

  while true; do
    out="$(bao_for "$addr" "$token" operator raft list-peers -format=json 2>/dev/null || true)"
    servers="$(jq '[.data.config.servers[]?] | length' <<<"$out" 2>/dev/null || echo 0)"
    leaders="$(jq '[.data.config.servers[]? | select(.leader == true)] | length' <<<"$out" 2>/dev/null || echo 0)"
    voters="$(jq '[.data.config.servers[]? | select((.voter == true) or (.suffrage == "voter") or (.suffrage == "Voter"))] | length' <<<"$out" 2>/dev/null || echo 0)"
    if [[ "$servers" -ge "$expected" && "$leaders" -eq 1 && "$voters" -ge "$expected" ]]; then
      echo "${name} Raft ready: servers=${servers} voters=${voters}"
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      echo "$out" >&2
      die "timed out waiting for ${name} Raft peers (servers=${servers} leaders=${leaders} voters=${voters})"
    fi
    sleep 2
  done
}

write_env_file() {
  local primary_init="$1"
  local secondary1_init="$2"
  local secondary2_init="$3"
  local primary_token secondary1_token secondary2_token
  local primary_unseal secondary1_unseal secondary2_unseal

  primary_token="$(jq -r '.root_token' "$primary_init")"
  secondary1_token="$(jq -r '.root_token' "$secondary1_init")"
  secondary2_token="$(jq -r '.root_token' "$secondary2_init")"
  primary_unseal="$(jq -r '.unseal_keys_b64[0]' "$primary_init")"
  secondary1_unseal="$(jq -r '.unseal_keys_b64[0]' "$secondary1_init")"
  secondary2_unseal="$(jq -r '.unseal_keys_b64[0]' "$secondary2_init")"

  cat >"$ENV_FILE" <<EOF
# Generated by scripts/dr_local_test.sh. Do not commit.
export DR_TOPOLOGY="${TOPOLOGY}"
export DR_PRIMARY_ADDR="${PRIMARY_ADDR}"
export DR_PRIMARY_TOKEN="${primary_token}"
export DR_PRIMARY_UNSEAL_KEY="${primary_unseal}"
export DR_SECONDARY1_ADDR="${SECONDARY1_ADDR}"
export DR_SECONDARY1_TOKEN="${secondary1_token}"
export DR_SECONDARY1_UNSEAL_KEY="${secondary1_unseal}"
export DR_SECONDARY2_ADDR="${SECONDARY2_ADDR}"
export DR_SECONDARY2_TOKEN="${secondary2_token}"
export DR_SECONDARY2_UNSEAL_KEY="${secondary2_unseal}"
export BAO_ADDR="${PRIMARY_ADDR}"
export BAO_TOKEN="${primary_token}"
EOF
  chmod 0600 "$ENV_FILE"
  echo "Wrote ${ENV_FILE}"
}

load_env() {
  [[ -f "$ENV_FILE" ]] || die "missing ${ENV_FILE}; run scripts/dr_local_test.sh bootstrap first"
  local requested_topology="$TOPOLOGY"
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  TOPOLOGY="$requested_topology"
}

call_idempotent() {
  local ok_pattern="$1"
  shift
  local out
  if out="$("$@" 2>&1)"; then
    printf "%s\n" "$out"
    return 0
  fi
  printf "%s\n" "$out" >&2
  if grep -qi "$ok_pattern" <<<"$out"; then
    return 0
  fi
  return 1
}

dr_status() {
  local name="$1"
  local addr="$2"
  local token="$3"
  echo "== ${name} =="
  bao_for "$addr" "$token" read -format=json sys/replication/dr/status | jq '.data'
}

wait_secondary_ready() {
  local name="$1"
  local addr="$2"
  local timeout="${3:-240}"
  local deadline=$(( $(date +%s) + timeout ))
  local out mode state lag

  while true; do
    out="$(bao_for "$addr" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status 2>/dev/null || true)"
    mode="$(jq -r '.data.mode // ""' <<<"$out" 2>/dev/null || true)"
    state="$(jq -r '.data.secondary_state // ""' <<<"$out" 2>/dev/null || true)"
    lag="$(jq -r '.data.lag_entries // 0' <<<"$out" 2>/dev/null || echo 0)"
    if [[ "$mode" == "secondary" && "$state" == "streaming" && "$lag" == "0" ]]; then
      echo "${name} ready: state=${state} lag=${lag}"
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      echo "$out" | jq '.data // {}' >&2 || true
      die "timed out waiting for ${name} to reach streaming lag=0"
    fi
    sleep 2
  done
}

cmd_up() {
  if [[ "${1:-}" == "--build" ]]; then
    (cd "$ROOT_DIR" && make docker-dev)
  fi
  compose up -d
}

cmd_down() {
  compose down -v --remove-orphans
}

cmd_bootstrap() {
  need_bin jq
  local tmpdir primary_init secondary1_init secondary2_init
  tmpdir="$(mktemp -d)"
  primary_init="${tmpdir}/primary.json"
  secondary1_init="${tmpdir}/secondary1.json"
  secondary2_init="${tmpdir}/secondary2.json"

  if [[ -f "$ENV_FILE" ]]; then
    # shellcheck disable=SC1090
    source "$ENV_FILE"
  fi

  init_and_unseal "primary" "$PRIMARY_ADDR" "$primary_init" "DR_PRIMARY_UNSEAL_KEY"
  init_and_unseal "secondary1" "$SECONDARY1_ADDR" "$secondary1_init" "DR_SECONDARY1_UNSEAL_KEY"
  init_and_unseal "secondary2" "$SECONDARY2_ADDR" "$secondary2_init" "DR_SECONDARY2_UNSEAL_KEY"

  if [[ "$TOPOLOGY" == "ha" ]]; then
    local primary_key secondary1_key secondary2_key
    local primary_token secondary1_token secondary2_token
    primary_key="$(jq -r '.unseal_keys_b64[0] // empty' "$primary_init" 2>/dev/null || true)"
    secondary1_key="$(jq -r '.unseal_keys_b64[0] // empty' "$secondary1_init" 2>/dev/null || true)"
    secondary2_key="$(jq -r '.unseal_keys_b64[0] // empty' "$secondary2_init" 2>/dev/null || true)"
    primary_key="${primary_key:-${DR_PRIMARY_UNSEAL_KEY:-}}"
    secondary1_key="${secondary1_key:-${DR_SECONDARY1_UNSEAL_KEY:-}}"
    secondary2_key="${secondary2_key:-${DR_SECONDARY2_UNSEAL_KEY:-}}"

    primary_token="$(jq -r '.root_token // empty' "$primary_init" 2>/dev/null || true)"
    secondary1_token="$(jq -r '.root_token // empty' "$secondary1_init" 2>/dev/null || true)"
    secondary2_token="$(jq -r '.root_token // empty' "$secondary2_init" 2>/dev/null || true)"
    primary_token="${primary_token:-${DR_PRIMARY_TOKEN:-}}"
    secondary1_token="${secondary1_token:-${DR_SECONDARY1_TOKEN:-}}"
    secondary2_token="${secondary2_token:-${DR_SECONDARY2_TOKEN:-}}"

    [[ -n "$primary_key" && -n "$secondary1_key" && -n "$secondary2_key" ]] || die "missing HA unseal keys"
    [[ -n "$primary_token" && -n "$secondary1_token" && -n "$secondary2_token" ]] || die "missing HA root tokens"

    unseal_with_key "primary-2" "${PRIMARY_NODE_ADDRS[1]}" "$primary_key"
    unseal_with_key "primary-3" "${PRIMARY_NODE_ADDRS[2]}" "$primary_key"
    unseal_with_key "secondary1-2" "${SECONDARY1_NODE_ADDRS[1]}" "$secondary1_key"
    unseal_with_key "secondary1-3" "${SECONDARY1_NODE_ADDRS[2]}" "$secondary1_key"
    unseal_with_key "secondary2-2" "${SECONDARY2_NODE_ADDRS[1]}" "$secondary2_key"
    unseal_with_key "secondary2-3" "${SECONDARY2_NODE_ADDRS[2]}" "$secondary2_key"

    wait_raft_peers "primary" "$PRIMARY_ADDR" "$primary_token" "$EXPECTED_RAFT_PEERS" 300
    wait_raft_peers "secondary1" "$SECONDARY1_ADDR" "$secondary1_token" "$EXPECTED_RAFT_PEERS" 300
    wait_raft_peers "secondary2" "$SECONDARY2_ADDR" "$secondary2_token" "$EXPECTED_RAFT_PEERS" 300
  fi

  if [[ -s "$primary_init" && -s "$secondary1_init" && -s "$secondary2_init" ]]; then
    write_env_file "$primary_init" "$secondary1_init" "$secondary2_init"
  else
    echo "Existing initialized clusters detected; keeping ${ENV_FILE}"
  fi
  rm -rf "$tmpdir"
}

cmd_configure() {
  need_bin jq
  load_env

  echo "Enabling DR primary..."
  call_idempotent "already enabled as primary" \
    bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write -f sys/replication/dr/primary/enable

  echo "Enabling DR secondary #1..."
  local token1
  token1="$(bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write -f -format=json sys/replication/dr/primary/secondary-token | jq -r '.data.token')"
  call_idempotent "already enabled as secondary" \
    bao_for "$DR_SECONDARY1_ADDR" "$DR_SECONDARY1_TOKEN" write sys/replication/dr/secondary/enable token="$token1"

  echo "Enabling DR secondary #2..."
  local token2
  token2="$(bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write -f -format=json sys/replication/dr/primary/secondary-token | jq -r '.data.token')"
  call_idempotent "already enabled as secondary" \
    bao_for "$DR_SECONDARY2_ADDR" "$DR_SECONDARY2_TOKEN" write sys/replication/dr/secondary/enable token="$token2"

  wait_secondary_ready "secondary1" "$DR_SECONDARY1_ADDR"
  wait_secondary_ready "secondary2" "$DR_SECONDARY2_ADDR"
  cmd_status
}

cmd_reset() {
  if [[ "${1:-}" == "--build" ]]; then
    (cd "$ROOT_DIR" && make docker-dev)
  fi
  cmd_down
  compose up -d
  cmd_bootstrap
  cmd_configure
}

cmd_status() {
  load_env
  dr_status "primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN"
  dr_status "secondary1" "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN"
  dr_status "secondary2" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN"
}

ensure_dr_stress() {
  if [[ ! -x "$DR_STRESS_BIN" ]] || find "${ROOT_DIR}/scripts/dr-stress" -name '*.go' -newer "$DR_STRESS_BIN" -print -quit | grep -q .; then
    echo "Building dr-stress..."
    mkdir -p "$(dirname "$DR_STRESS_BIN")"
    (cd "${ROOT_DIR}/scripts/dr-stress" && go build -o "$DR_STRESS_BIN" .)
  fi
}

cmd_smoke() {
  load_env
  ensure_dr_stress
  local args=(
    -primary-addr "$DR_PRIMARY_ADDR"
    -primary-token "$DR_PRIMARY_TOKEN"
    -secondary1-addr "$DR_SECONDARY1_ADDR"
    -secondary1-token "$DR_PRIMARY_TOKEN"
    -secondary2-addr "$DR_SECONDARY2_ADDR"
    -secondary2-token "$DR_PRIMARY_TOKEN"
    -ensure-kv
    -output-dir "$RESULTS_DIR"
    -duration 120
    -concurrency 24
    -max-wait-seconds 180
  )
  "$DR_STRESS_BIN" run "${args[@]}" "$@"
}

latest_run_dir() {
  local latest=""
  local dir
  while IFS= read -r -d '' dir; do
    if [[ -z "$latest" || "$dir" -nt "$latest" ]]; then
      latest="$dir"
    fi
  done < <(find "$RESULTS_DIR" -mindepth 1 -maxdepth 1 -type d -name 'drmixed-*' -print0 2>/dev/null)
  printf "%s\n" "$latest"
}

verify_one() {
  local label="$1"
  local addr="$2"
  local run_dir="$3"
  local sample="$4"
  local out="${run_dir}/verify-${label}.json"

  echo "Verifying ${label}..."
  "$DR_STRESS_BIN" verify \
    -addr "$addr" \
    -token "$DR_PRIMARY_TOKEN" \
    -run-dir "$run_dir" \
    -sample "$sample" \
    -json >"$out"
  jq '.summary // .' "$out"
}

cmd_verify() {
  load_env
  ensure_dr_stress
  local run_dir=""
  local sample=0

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --sample)
        sample="${2:?missing value for --sample}"
        shift 2
        ;;
      -*)
        die "unknown verify option: $1"
        ;;
      *)
        if [[ -n "$run_dir" ]]; then
          die "verify accepts at most one run directory"
        fi
        run_dir="$1"
        shift
        ;;
    esac
  done

  if [[ -z "$run_dir" ]]; then
    run_dir="$(latest_run_dir)"
  fi
  [[ -n "$run_dir" && -d "$run_dir" ]] || die "no run directory found"

  verify_one "primary" "$DR_PRIMARY_ADDR" "$run_dir" "$sample"
  verify_one "secondary1" "$DR_SECONDARY1_ADDR" "$run_dir" "$sample"
  verify_one "secondary2" "$DR_SECONDARY2_ADDR" "$run_dir" "$sample"
}

ensure_kv_mount_on() {
  local addr="$1"
  local token="$2"
  local out
  if out="$(bao_for "$addr" "$token" secrets enable -path=kv kv-v2 2>&1)"; then
    return 0
  fi
  if grep -qi "path is already in use" <<<"$out"; then
    return 0
  fi
  printf "%s\n" "$out" >&2
  return 1
}

ensure_kv_mount() {
  ensure_kv_mount_on "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN"
}

kv_put() {
  local addr="$1"
  local token="$2"
  local key_name="$3"
  local phase="$4"
  local run_id="$5"

  bao_for "$addr" "$token" kv put "kv/${key_name}" \
    phase="$phase" \
    run_id="$run_id" \
    ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)" >/dev/null
}

kv_present() {
  local addr="$1"
  local token="$2"
  local key_name="$3"

  bao_for "$addr" "$token" kv get -format=json "kv/${key_name}" >/dev/null 2>&1
}

assert_kv_present() {
  local label="$1"
  local addr="$2"
  local token="$3"
  local key_name="$4"

  kv_present "$addr" "$token" "$key_name" || die "expected ${label} to contain kv/${key_name}"
}

assert_kv_absent() {
  local label="$1"
  local addr="$2"
  local token="$3"
  local key_name="$4"

  if kv_present "$addr" "$token" "$key_name"; then
    die "expected ${label} not to contain kv/${key_name}"
  fi
}

print_key_presence() {
  local label="$1"
  local addr="$2"
  local token="$3"
  local key_name="$4"

  if kv_present "$addr" "$token" "$key_name"; then
    printf "%-24s kv/%-56s present\n" "$label" "$key_name"
  else
    printf "%-24s kv/%-56s absent\n" "$label" "$key_name"
  fi
}

secondary1_active_addr() {
  local addr status
  for addr in "${SECONDARY1_NODE_ADDRS[@]}"; do
    if status="$(status_json "$addr" 2>/dev/null)"; then
      if jq -e '.sealed == false and (.is_self == true)' >/dev/null <<<"$status"; then
        printf "%s\n" "$addr"
        return 0
      fi
    fi
  done
  return 1
}

wait_secondary1_active_addr() {
  local timeout="${1:-120}"
  local deadline=$(( $(date +%s) + timeout ))
  local active=""
  while true; do
    active="$(secondary1_active_addr || true)"
    if [[ -n "$active" ]]; then
      printf "%s\n" "$active"
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      die "timed out waiting for promoted secondary1 active node"
    fi
    sleep 2
  done
}

wait_secondary1_active_change() {
  local old_active="$1"
  local timeout="${2:-120}"
  local deadline=$(( $(date +%s) + timeout ))
  local active=""
  while true; do
    active="$(secondary1_active_addr || true)"
    if [[ -n "$active" && "$active" != "$old_active" ]]; then
      printf "%s\n" "$active"
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      die "timed out waiting for promoted secondary1 active handoff away from ${old_active}"
    fi
    sleep 2
  done
}

promoted_secondary1_promotion_id() {
  bao_for "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status | jq -r '.data.last_promotion_id // empty'
}

wait_promoted_secondary1_ready() {
  local expected_promotion_id="${1:-}"
  local timeout="${2:-180}"
  local deadline=$(( $(date +%s) + timeout ))
  local addr status dr_status mode promotion_id sealed all_ready

  wait_raft_peers "promoted secondary1" "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" "$EXPECTED_RAFT_PEERS" "$timeout"

  while true; do
    all_ready=true
    for addr in "${SECONDARY1_NODE_ADDRS[@]}"; do
      if ! status="$(status_json "$addr" 2>/dev/null)"; then
        all_ready=false
        break
      fi
      sealed="$(jq -r '.sealed' <<<"$status")"
      if [[ "$sealed" != "false" ]]; then
        all_ready=false
        break
      fi
      if ! dr_status="$(bao_for "$addr" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status 2>/dev/null)"; then
        all_ready=false
        break
      fi
      mode="$(jq -r '.data.mode // ""' <<<"$dr_status")"
      promotion_id="$(jq -r '.data.last_promotion_id // ""' <<<"$dr_status")"
      if [[ "$mode" != "disabled" || -z "$promotion_id" ]]; then
        all_ready=false
        break
      fi
      if [[ -n "$expected_promotion_id" && "$promotion_id" != "$expected_promotion_id" ]]; then
        die "promotion ID changed on ${addr}: got ${promotion_id}, expected ${expected_promotion_id}"
      fi
    done
    if [[ "$all_ready" == "true" ]]; then
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      die "timed out waiting for promoted secondary1 to be ready"
    fi
    sleep 2
  done
}

stop_old_primary_services() {
  echo "Stopping old primary services: ${PRIMARY_SERVICES[*]}"
  compose stop "${PRIMARY_SERVICES[@]}"
  sleep 3
}

restart_old_primary_services() {
  echo "Restarting old primary services: ${PRIMARY_SERVICES[*]}"
  compose start "${PRIMARY_SERVICES[@]}"

  local i
  for i in "${!PRIMARY_NODE_ADDRS[@]}"; do
    unseal_with_key "primary-$((i + 1))" "${PRIMARY_NODE_ADDRS[$i]}" "$DR_PRIMARY_UNSEAL_KEY"
  done
  wait_raft_peers "old primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$EXPECTED_RAFT_PEERS" 300
}

assert_promoted_secondary1_key_present_all() {
  local key_name="$1"
  local addr i
  for i in "${!SECONDARY1_NODE_ADDRS[@]}"; do
    addr="${SECONDARY1_NODE_ADDRS[$i]}"
    assert_kv_present "promoted secondary1-$((i + 1))" "$addr" "$DR_PRIMARY_TOKEN" "$key_name"
  done
}

promote_secondary1_after_primary_stop() {
  local tmp_out tmp_err err promotion_json
  tmp_out="$(mktemp)"
  tmp_err="$(mktemp)"

  echo "Checking that hard-stop promotion requires explicit data-loss acknowledgement..."
  if bao_for "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" write -format=json \
    sys/replication/dr/secondary/promote \
    confirm_primary_unreachable=true >"$tmp_out" 2>"$tmp_err"; then
    promotion_json="$(cat "$tmp_out")"
    rm -f "$tmp_out" "$tmp_err"
    printf "%s\n" "$promotion_json" | jq '.data'
    die "hard-stop promotion unexpectedly succeeded without accept_data_loss=true"
  fi

  err="$(cat "$tmp_err" "$tmp_out")"
  if ! grep -qi "accept_data_loss" <<<"$err"; then
    rm -f "$tmp_out" "$tmp_err"
    printf "%s\n" "$err" >&2
    die "promotion failed, but not with the expected accept_data_loss guard"
  fi

  echo "Promoting secondary1 with accept_data_loss=true..."
  if ! bao_for "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" write -format=json \
    sys/replication/dr/secondary/promote \
    confirm_primary_unreachable=true \
    accept_data_loss=true >"$tmp_out" 2>"$tmp_err"; then
    err="$(cat "$tmp_err" "$tmp_out")"
    rm -f "$tmp_out" "$tmp_err"
    printf "%s\n" "$err" >&2
    die "forced promotion failed"
  fi

  promotion_json="$(cat "$tmp_out")"
  rm -f "$tmp_out" "$tmp_err"
  printf "%s\n" "$promotion_json" | jq '.data | {
    promotion_id,
    promotion_class,
    data_loss_accepted,
    estimated_data_loss_entries,
    old_primary_cluster_id,
    old_relationship_id
  }'

  [[ "$(jq -r '.data.promotion_class // ""' <<<"$promotion_json")" == "forced" ]] || die "expected forced promotion class"
  [[ "$(jq -r '.data.data_loss_accepted // false' <<<"$promotion_json")" == "true" ]] || die "expected data_loss_accepted=true"
}

cmd_failover_smoke() {
  need_bin jq
  load_env

  local run_id before_key promoted_key old_primary_key stale_old_primary_token promoted_addr
  run_id="failover-smoke-$(date -u +%Y%m%dT%H%M%SZ)"
  before_key="${run_id}/before"
  promoted_key="${run_id}/promoted-after"
  old_primary_key="${run_id}/old-primary-after-restart"

  echo "Preparing failover smoke run: ${run_id}"
  ensure_kv_mount
  wait_secondary_ready "secondary1" "$DR_SECONDARY1_ADDR"
  wait_secondary_ready "secondary2" "$DR_SECONDARY2_ADDR"

  echo "Writing pre-failover key on primary..."
  kv_put "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$before_key" "before" "$run_id"
  wait_secondary_ready "secondary1" "$DR_SECONDARY1_ADDR"
  wait_secondary_ready "secondary2" "$DR_SECONDARY2_ADDR"
  assert_kv_present "primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$before_key"
  assert_kv_present "secondary1" "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" "$before_key"
  assert_kv_present "secondary2" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" "$before_key"

  echo "Minting an old-primary activation token that must become stale after promotion..."
  stale_old_primary_token="$(bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write -f -format=json sys/replication/dr/primary/secondary-token | jq -r '.data.token')"

  stop_old_primary_services

  promote_secondary1_after_primary_stop
  promoted_addr="$(wait_secondary1_active_addr 120)"

  echo "Writing post-promotion key on promoted secondary1..."
  kv_put "$promoted_addr" "$DR_PRIMARY_TOKEN" "$promoted_key" "promoted-after" "$run_id"
  assert_kv_present "promoted secondary1" "$promoted_addr" "$DR_PRIMARY_TOKEN" "$promoted_key"

  echo "Verifying promoted secondary1 rejects stale old-primary activation token..."
  local tmp_out tmp_err stale_err
  tmp_out="$(mktemp)"
  tmp_err="$(mktemp)"
  if bao_for "$promoted_addr" "$DR_PRIMARY_TOKEN" write \
    sys/replication/dr/secondary/enable \
    token="$stale_old_primary_token" >"$tmp_out" 2>"$tmp_err"; then
    rm -f "$tmp_out" "$tmp_err"
    die "promoted secondary1 accepted a stale old-primary activation token"
  fi
  stale_err="$(cat "$tmp_err" "$tmp_out")"
  rm -f "$tmp_out" "$tmp_err"
  if ! grep -qi "stale pre-promotion" <<<"$stale_err"; then
    printf "%s\n" "$stale_err" >&2
    die "stale activation token failed, but not with the expected lineage fence"
  fi

  restart_old_primary_services

  echo "Writing divergent key on resurrected old primary..."
  wait_secondary_ready "secondary2" "$DR_SECONDARY2_ADDR"
  kv_put "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$old_primary_key" "old-primary-after-restart" "$run_id"
  wait_secondary_ready "secondary2" "$DR_SECONDARY2_ADDR"

  assert_kv_absent "old primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$promoted_key"
  assert_kv_absent "secondary2" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" "$promoted_key"
  assert_kv_absent "promoted secondary1" "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" "$old_primary_key"
  assert_kv_present "old primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$old_primary_key"
  assert_kv_present "secondary2" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" "$old_primary_key"

  echo "Key visibility:"
  print_key_presence "old primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$before_key"
  print_key_presence "promoted secondary1" "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" "$before_key"
  print_key_presence "secondary2" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" "$before_key"
  print_key_presence "old primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$promoted_key"
  print_key_presence "promoted secondary1" "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" "$promoted_key"
  print_key_presence "secondary2" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" "$promoted_key"
  print_key_presence "old primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$old_primary_key"
  print_key_presence "promoted secondary1" "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" "$old_primary_key"
  print_key_presence "secondary2" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" "$old_primary_key"

  echo "Failover smoke passed. The environment is intentionally left in a divergent post-failover state; run reset before the next test."
}

cmd_promoted_durability_smoke() {
  [[ "$TOPOLOGY" == "ha" ]] || die "promoted-durability-smoke requires --topology ha"
  need_bin jq
  load_env

  local run_id promotion_id before_restart_key after_restart_key after_handoff_key
  local active_before_restart active_after_restart active_before active_after stale_old_primary_token tmp_out tmp_err stale_err
  run_id="promoted-durability-$(date -u +%Y%m%dT%H%M%SZ)"
  before_restart_key="${run_id}/before-restart"
  after_restart_key="${run_id}/after-restart"
  after_handoff_key="${run_id}/after-handoff"

  promotion_id="$(promoted_secondary1_promotion_id)"
  [[ -n "$promotion_id" && "$promotion_id" != "null" ]] || die "secondary1 is not a promoted cluster with a promotion record"

  echo "Checking promoted secondary1 durability for promotion ${promotion_id}..."
  wait_promoted_secondary1_ready "$promotion_id" 180
  active_before_restart="$(wait_secondary1_active_addr 120)"

  echo "Writing pre-restart key on promoted secondary1..."
  kv_put "$active_before_restart" "$DR_PRIMARY_TOKEN" "$before_restart_key" "before-restart" "$run_id"
  assert_promoted_secondary1_key_present_all "$before_restart_key"

  echo "Restarting all promoted secondary1 services..."
  compose restart secondary1-1 secondary1-2 secondary1-3
  local i
  for i in "${!SECONDARY1_NODE_ADDRS[@]}"; do
    unseal_with_key "secondary1-$((i + 1))" "${SECONDARY1_NODE_ADDRS[$i]}" "$DR_SECONDARY1_UNSEAL_KEY"
  done
  wait_promoted_secondary1_ready "$promotion_id" 300
  active_after_restart="$(wait_secondary1_active_addr 120)"
  assert_promoted_secondary1_key_present_all "$before_restart_key"

  echo "Writing post-restart key on promoted secondary1..."
  kv_put "$active_after_restart" "$DR_PRIMARY_TOKEN" "$after_restart_key" "after-restart" "$run_id"
  assert_promoted_secondary1_key_present_all "$after_restart_key"

  echo "Verifying stale old-primary activation token is still rejected after promoted restart..."
  stale_old_primary_token="$(bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write -f -format=json sys/replication/dr/primary/secondary-token | jq -r '.data.token')"
  tmp_out="$(mktemp)"
  tmp_err="$(mktemp)"
  if bao_for "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" write \
    sys/replication/dr/secondary/enable \
    token="$stale_old_primary_token" >"$tmp_out" 2>"$tmp_err"; then
    rm -f "$tmp_out" "$tmp_err"
    die "promoted secondary1 accepted a stale old-primary activation token after restart"
  fi
  stale_err="$(cat "$tmp_err" "$tmp_out")"
  rm -f "$tmp_out" "$tmp_err"
  if ! grep -qi "stale pre-promotion" <<<"$stale_err"; then
    printf "%s\n" "$stale_err" >&2
    die "stale activation token failed after restart, but not with the expected lineage fence"
  fi

  active_before="$(wait_secondary1_active_addr 120)"
  echo "Stepping down promoted secondary1 active node: ${active_before}"
  bao_for "$active_before" "$DR_PRIMARY_TOKEN" write -f sys/step-down >/dev/null
  active_after="$(wait_secondary1_active_change "$active_before" 120)"
  echo "Promoted secondary1 active moved to: ${active_after}"

  wait_promoted_secondary1_ready "$promotion_id" 180
  echo "Writing post-handoff key on promoted secondary1..."
  kv_put "$active_after" "$DR_PRIMARY_TOKEN" "$after_handoff_key" "after-handoff" "$run_id"
  assert_promoted_secondary1_key_present_all "$after_handoff_key"

  echo "Promotion durability smoke passed. Promoted secondary1 remained writable across full restart, stale-token rejection, and HA active handoff."
}

cmd_reseed_secondary_smoke() {
  [[ "$TOPOLOGY" == "ha" ]] || die "reseed-secondary-smoke requires --topology ha"
  need_bin jq
  load_env

  local run_id promotion_id promoted_old_key old_primary_key promoted_after_reseed_key
  local promoted_addr
  local token promoted_cluster_id secondary2_cluster_id
  run_id="reseed-secondary-$(date -u +%Y%m%dT%H%M%SZ)"
  promoted_old_key="${run_id}/promoted-before-reseed"
  old_primary_key="${run_id}/old-primary-before-reseed"
  promoted_after_reseed_key="${run_id}/promoted-after-reseed"

  promotion_id="$(promoted_secondary1_promotion_id)"
  [[ -n "$promotion_id" && "$promotion_id" != "null" ]] || die "secondary1 is not a promoted cluster with a promotion record"

  echo "Checking explicit secondary reseed from promoted authority for promotion ${promotion_id}..."
  wait_promoted_secondary1_ready "$promotion_id" 180
  promoted_addr="$(wait_secondary1_active_addr 120)"
  wait_secondary_ready "secondary2-old-lineage" "$DR_SECONDARY2_ADDR" 180

  ensure_kv_mount_on "$promoted_addr" "$DR_PRIMARY_TOKEN"
  ensure_kv_mount_on "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN"

  echo "Creating divergent keys before reseed..."
  kv_put "$promoted_addr" "$DR_PRIMARY_TOKEN" "$promoted_old_key" "promoted-before-reseed" "$run_id"
  kv_put "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$old_primary_key" "old-primary-before-reseed" "$run_id"
  wait_secondary_ready "secondary2-old-lineage" "$DR_SECONDARY2_ADDR" 180

  assert_kv_present "promoted secondary1" "$promoted_addr" "$DR_PRIMARY_TOKEN" "$promoted_old_key"
  assert_kv_absent "old primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$promoted_old_key"
  assert_kv_absent "secondary2 old lineage" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" "$promoted_old_key"
  assert_kv_present "old primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$old_primary_key"
  assert_kv_present "secondary2 old lineage" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" "$old_primary_key"
  assert_kv_absent "promoted secondary1" "$promoted_addr" "$DR_PRIMARY_TOKEN" "$old_primary_key"

  echo "Enabling DR primary mode on promoted secondary1..."
  call_idempotent "already enabled as primary" \
    bao_for "$promoted_addr" "$DR_PRIMARY_TOKEN" write -f sys/replication/dr/primary/enable
  promoted_addr="$(wait_secondary1_active_addr 120)"
  promoted_cluster_id="$(bao_for "$promoted_addr" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status | jq -r '.data.cluster_id')"
  [[ -n "$promoted_cluster_id" && "$promoted_cluster_id" != "null" ]] || die "promoted secondary1 did not become a DR primary"

  echo "Disabling secondary2 from old primary lineage..."
  call_idempotent "not in DR secondary mode" \
    bao_for "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" write -f sys/replication/dr/secondary/disable

  echo "Issuing fresh activation token from promoted secondary1..."
  token="$(bao_for "$promoted_addr" "$DR_PRIMARY_TOKEN" write -f -format=json sys/replication/dr/primary/secondary-token | jq -r '.data.token')"

  echo "Enabling secondary2 against promoted secondary1..."
  bao_for "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" write sys/replication/dr/secondary/enable token="$token"
  wait_secondary_ready "secondary2-promoted-lineage" "$DR_SECONDARY2_ADDR" 300

  secondary2_cluster_id="$(bao_for "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status | jq -r '.data.cluster_id')"
  [[ "$secondary2_cluster_id" == "$promoted_cluster_id" ]] || die "secondary2 cluster_id ${secondary2_cluster_id} does not match promoted primary ${promoted_cluster_id}"

  echo "Verifying secondary2 now follows promoted timeline..."
  assert_kv_present "secondary2 promoted lineage" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" "$promoted_old_key"
  assert_kv_absent "secondary2 promoted lineage" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" "$old_primary_key"
  assert_kv_absent "promoted secondary1" "$promoted_addr" "$DR_PRIMARY_TOKEN" "$old_primary_key"
  assert_kv_present "old primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$old_primary_key"
  assert_kv_absent "old primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$promoted_old_key"

  echo "Writing new promoted-primary key and waiting for secondary2 convergence..."
  kv_put "$promoted_addr" "$DR_PRIMARY_TOKEN" "$promoted_after_reseed_key" "promoted-after-reseed" "$run_id"
  wait_secondary_ready "secondary2-promoted-lineage" "$DR_SECONDARY2_ADDR" 180
  assert_kv_present "promoted secondary1" "$promoted_addr" "$DR_PRIMARY_TOKEN" "$promoted_after_reseed_key"
  assert_kv_present "secondary2 promoted lineage" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" "$promoted_after_reseed_key"
  assert_kv_absent "old primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$promoted_after_reseed_key"

  echo "Secondary reseed smoke passed. Secondary2 now follows the promoted authority and old-primary-only keys did not merge."
}

cmd_failover_load_lifecycle() {
  [[ "$TOPOLOGY" == "ha" ]] || die "failover-load-lifecycle requires --topology ha"
  need_bin jq

  local duration=150
  local concurrency=48
  local hard_stop_after=75
  local put_percent=70
  local get_primary_percent=20
  local status_s1_percent=5
  local status_s2_percent=5
  local cold_key_count=5000
  local max_wait_seconds=1
  local sentinel_write_timeout=10
  local progress_interval=5
  local do_reset=true
  local do_build=false

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --duration)
        duration="${2:?missing value for --duration}"
        shift 2
        ;;
      --concurrency)
        concurrency="${2:?missing value for --concurrency}"
        shift 2
        ;;
      --hard-stop-after)
        hard_stop_after="${2:?missing value for --hard-stop-after}"
        shift 2
        ;;
      --put-percent)
        put_percent="${2:?missing value for --put-percent}"
        shift 2
        ;;
      --get-primary-percent)
        get_primary_percent="${2:?missing value for --get-primary-percent}"
        shift 2
        ;;
      --status-s1-percent)
        status_s1_percent="${2:?missing value for --status-s1-percent}"
        shift 2
        ;;
      --status-s2-percent)
        status_s2_percent="${2:?missing value for --status-s2-percent}"
        shift 2
        ;;
      --cold-key-count)
        cold_key_count="${2:?missing value for --cold-key-count}"
        shift 2
        ;;
      --max-wait-seconds)
        max_wait_seconds="${2:?missing value for --max-wait-seconds}"
        shift 2
        ;;
      --sentinel-write-timeout)
        sentinel_write_timeout="${2:?missing value for --sentinel-write-timeout}"
        shift 2
        ;;
      --progress-interval)
        progress_interval="${2:?missing value for --progress-interval}"
        shift 2
        ;;
      --no-reset)
        do_reset=false
        shift
        ;;
      --build)
        do_build=true
        shift
        ;;
      *)
        die "unknown failover-load-lifecycle option: $1"
        ;;
    esac
  done

  [[ "$duration" =~ ^[0-9]+$ ]] || die "--duration must be an integer"
  [[ "$concurrency" =~ ^[0-9]+$ ]] || die "--concurrency must be an integer"
  [[ "$hard_stop_after" =~ ^[0-9]+$ ]] || die "--hard-stop-after must be an integer"
  (( duration > 0 )) || die "--duration must be > 0"
  (( concurrency > 0 )) || die "--concurrency must be > 0"
  (( hard_stop_after > 0 && hard_stop_after < duration )) || die "--hard-stop-after must be > 0 and less than --duration"

  if [[ "$do_reset" == "true" ]]; then
    if [[ "$do_build" == "true" ]]; then
      cmd_reset --build
    else
      cmd_reset
    fi
  else
    load_env
    wait_secondary_ready "secondary1" "$DR_SECONDARY1_ADDR"
    wait_secondary_ready "secondary2" "$DR_SECONDARY2_ADDR"
  fi

  ensure_dr_stress

  local run_id run_dir stress_pid no_accept_rc stress_rc
  local promoted_addr durability_log reseed_log
  run_id="failover-load-$(date -u +%Y%m%dT%H%M%SZ)"
  run_dir="${RESULTS_DIR}/${run_id}"
  mkdir -p "$run_dir"

  {
    echo "run_id=${run_id}"
    echo "run_dir=${run_dir}"
    echo "started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "duration=${duration}"
    echo "concurrency=${concurrency}"
    echo "hard_stop_after=${hard_stop_after}"
  } | tee "$run_dir/orchestrator.log"

  "$DR_STRESS_BIN" run \
    -run-id "$run_id" \
    -output-dir "$RESULTS_DIR" \
    -primary-addr "$DR_PRIMARY_ADDR" \
    -primary-token "$DR_PRIMARY_TOKEN" \
    -secondary1-addr "$DR_SECONDARY1_ADDR" \
    -secondary1-token "$DR_PRIMARY_TOKEN" \
    -secondary2-addr "$DR_SECONDARY2_ADDR" \
    -secondary2-token "$DR_PRIMARY_TOKEN" \
    -ensure-kv \
    -duration "$duration" \
    -concurrency "$concurrency" \
    -put-percent "$put_percent" \
    -get-primary-percent "$get_primary_percent" \
    -status-s1-percent "$status_s1_percent" \
    -status-s2-percent "$status_s2_percent" \
    -cold-key-count "$cold_key_count" \
    -max-wait-seconds "$max_wait_seconds" \
    -sentinel-write-timeout "$sentinel_write_timeout" \
    -progress-interval "$progress_interval" \
    >"$run_dir/harness.out" 2>"$run_dir/harness.err" &
  stress_pid=$!
  echo "stress_pid=${stress_pid}" | tee -a "$run_dir/orchestrator.log"

  sleep "$hard_stop_after"
  echo "hard_stop_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$run_dir/orchestrator.log"
  bao_for "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status >"$run_dir/secondary1-before-hard-stop-status.json" 2>"$run_dir/secondary1-before-hard-stop-status.err" || true
  stop_old_primary_services | tee -a "$run_dir/orchestrator.log"
  bao_for "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status >"$run_dir/secondary1-after-hard-stop-before-promotion-status.json" 2>"$run_dir/secondary1-after-hard-stop-before-promotion-status.err" || true

  set +e
  bao_for "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" write -format=json \
    sys/replication/dr/secondary/promote \
    confirm_primary_unreachable=true >"$run_dir/promotion-no-accept.json" 2>"$run_dir/promotion-no-accept.err"
  no_accept_rc=$?
  set -e
  echo "promotion_no_accept_rc=${no_accept_rc}" | tee -a "$run_dir/orchestrator.log"
  if [[ "$no_accept_rc" -eq 0 ]]; then
    die "promotion without accept_data_loss unexpectedly succeeded"
  fi
  if ! grep -qi "accept_data_loss" "$run_dir/promotion-no-accept.err" "$run_dir/promotion-no-accept.json"; then
    cat "$run_dir/promotion-no-accept.err" "$run_dir/promotion-no-accept.json" >&2
    die "promotion without accept_data_loss failed without the expected guard"
  fi

  bao_for "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" write -format=json \
    sys/replication/dr/secondary/promote \
    confirm_primary_unreachable=true \
    accept_data_loss=true >"$run_dir/promotion.json" 2>"$run_dir/promotion.err"
  jq '.data | {
    promotion_id,
    promotion_class,
    clean_promotion_eligible,
    clean_promotion_proof_available,
    forced_promotion_requires_acknowledgement,
    forced_promotion_reason_codes,
    forced_promotion_reason_details,
    data_loss_accepted,
    last_applied_index,
    last_known_primary_index,
    estimated_data_loss_entries,
    estimated_data_loss_entries_basis,
    duration
  }' "$run_dir/promotion.json" | tee "$run_dir/promotion-summary.json"

  bao_for "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status >"$run_dir/secondary1-after-promotion-status.json" 2>"$run_dir/secondary1-after-promotion-status.err" || true
  promoted_addr="$(wait_secondary1_active_addr 120)"
  bao_for "$promoted_addr" "$DR_PRIMARY_TOKEN" kv put "kv/${run_id}/post-promotion-marker" \
    run_id="$run_id" \
    marker=post-promotion \
    promoted_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$run_dir/post-promotion-marker.out" 2>"$run_dir/post-promotion-marker.err"

  echo "waiting_for_stress_pid=${stress_pid}" | tee -a "$run_dir/orchestrator.log"
  set +e
  wait "$stress_pid"
  stress_rc=$?
  set -e
  echo "stress_rc=${stress_rc}" | tee -a "$run_dir/orchestrator.log"
  if [[ "$stress_rc" -ne 0 ]]; then
    die "dr-stress exited non-zero; artifacts preserved in ${run_dir}"
  fi

  "$DR_STRESS_BIN" verify \
    -addr "$promoted_addr" \
    -token "$DR_PRIMARY_TOKEN" \
    -run-dir "$run_dir" \
    -sample 0 \
    -json >"$run_dir/verify-promoted-secondary1.json"
  jq '.summary // .' "$run_dir/verify-promoted-secondary1.json" | tee "$run_dir/verify-promoted-secondary1-summary.json"

  restart_old_primary_services

  durability_log="${RESULTS_DIR}/promoted-durability-$(date -u +%Y%m%dT%H%M%SZ)-after-load.log"
  cmd_promoted_durability_smoke 2>&1 | tee "$durability_log"

  reseed_log="${RESULTS_DIR}/reseed-secondary-$(date -u +%Y%m%dT%H%M%SZ)-after-load.log"
  cmd_reseed_secondary_smoke 2>&1 | tee "$reseed_log"

  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status >"$run_dir/final-old-primary-status.json" 2>"$run_dir/final-old-primary-status.err" || true
  bao_for "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status >"$run_dir/final-promoted-secondary1-status.json" 2>"$run_dir/final-promoted-secondary1-status.err" || true
  bao_for "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status >"$run_dir/final-secondary2-status.json" 2>"$run_dir/final-secondary2-status.err" || true
  echo "completed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$run_dir/orchestrator.log"

  echo "Failover load lifecycle passed."
  echo "Load run: ${run_dir}"
  echo "Durability log: ${durability_log}"
  echo "Reseed log: ${reseed_log}"
}

cmd_logs() {
  compose logs --tail "${DR_LOG_TAIL:-200}" "$@"
}

main() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --topology)
        TOPOLOGY="${2:?missing value for --topology}"
        shift 2
        ;;
      --topology=*)
        TOPOLOGY="${1#--topology=}"
        shift
        ;;
      *)
        break
        ;;
    esac
  done
  configure_topology

  local cmd="${1:-help}"
  shift || true
  case "$cmd" in
    up) cmd_up "$@" ;;
    bootstrap) cmd_bootstrap "$@" ;;
    configure) cmd_configure "$@" ;;
    reset) cmd_reset "$@" ;;
    status) cmd_status "$@" ;;
    smoke) cmd_smoke "$@" ;;
    verify) cmd_verify "$@" ;;
    failover-smoke) cmd_failover_smoke "$@" ;;
    promoted-durability-smoke) cmd_promoted_durability_smoke "$@" ;;
    reseed-secondary-smoke) cmd_reseed_secondary_smoke "$@" ;;
    failover-load-lifecycle) cmd_failover_load_lifecycle "$@" ;;
    down) cmd_down "$@" ;;
    logs) cmd_logs "$@" ;;
    help|-h|--help) usage ;;
    *) usage; die "unknown command: $cmd" ;;
  esac
}

main "$@"
