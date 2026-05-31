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
  scripts/dr_local_test.sh [--topology single|ha] engine-matrix
  scripts/dr_local_test.sh --topology ha engine-lifecycle-matrix
  scripts/dr_local_test.sh [--topology single|ha] failover-smoke
  scripts/dr_local_test.sh --topology ha promoted-durability-smoke
  scripts/dr_local_test.sh --topology ha reseed-secondary-smoke
  scripts/dr_local_test.sh --topology ha quiescent-reconnect-smoke [--no-reset] [--build]
  scripts/dr_local_test.sh --topology ha accumulator-cold-restart-smoke [--no-reset] [--build] [--stop-seconds N]
  scripts/dr_local_test.sh --topology ha secondary-outage-smoke [--duration N] [--concurrency N] [--outage-after N] [--outage-seconds N] [--no-reset] [--build]
  scripts/dr_local_test.sh --topology ha secondary-outage-reconcile-smoke [--duration N] [--concurrency N] [--outage-after N] [--outage-seconds N] [--no-reset] [--build]
  scripts/dr_local_test.sh --topology ha indexed-repair-smoke [--duration N] [--concurrency N] [--stepdown-interval N] [--no-reset] [--build]
  scripts/dr_local_test.sh --topology ha tuning-load-smoke [--duration N] [--concurrency N] [--no-reset]
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
  scripts/dr_local_test.sh engine-matrix

HA topology:
  scripts/dr_local_test.sh --topology ha reset
  scripts/dr_local_test.sh --topology ha engine-lifecycle-matrix
  scripts/dr_local_test.sh --topology ha smoke --duration 900 --concurrency 48 --stepdown-interval 300
  scripts/dr_local_test.sh --topology ha quiescent-reconnect-smoke
  scripts/dr_local_test.sh --topology ha accumulator-cold-restart-smoke
  scripts/dr_local_test.sh --topology ha secondary-outage-smoke
  scripts/dr_local_test.sh --topology ha secondary-outage-reconcile-smoke
  scripts/dr_local_test.sh --topology ha indexed-repair-smoke
  scripts/dr_local_test.sh --topology ha tuning-load-smoke
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

join_csv() {
  local IFS=,
  printf "%s" "$*"
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

bao_for_ns() {
  local addr="$1"
  local token="$2"
  local ns="$3"
  shift 3 || true
  if [[ -n "$token" ]]; then
    env BAO_ADDR="$addr" BAO_TOKEN="$token" BAO_NAMESPACE="$ns" "$(bao_bin)" "$@"
  else
    env BAO_ADDR="$addr" BAO_NAMESPACE="$ns" "$(bao_bin)" "$@"
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

unseal_with_any_key() {
  local name="$1"
  local addr="$2"
  shift 2
  local status sealed key tmp_err

  wait_status "$name" "$addr" 180
  status="$(status_json "$addr")"
  sealed="$(jq -r '.sealed' <<<"$status")"
  if [[ "$sealed" != "true" ]]; then
    return 0
  fi

  tmp_err="$(mktemp)"
  for key in "$@"; do
    [[ -n "$key" ]] || continue
    if bao_for "$addr" "" operator unseal "$key" >/dev/null 2>"$tmp_err"; then
      rm -f "$tmp_err"
      return 0
    fi
  done

  echo "failed to unseal ${name} with any configured local DR key" >&2
  cat "$tmp_err" >&2 || true
  rm -f "$tmp_err"
  return 1
}

unseal_secondary1_dr() {
  local name="$1"
  local addr="$2"
  unseal_with_any_key "$name" "$addr" "${DR_SECONDARY1_UNSEAL_KEY:-}" "${DR_PRIMARY_UNSEAL_KEY:-}"
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

dr_status_json() {
  local addr="$1"
  local token="$2"
  bao_for "$addr" "$token" read -format=json sys/replication/dr/status
}

dr_uint_field_from_file() {
  local file="$1"
  local field="$2"
  local value
  value="$(jq -r --arg field "$field" '.data[$field] // 0' "$file")"
  [[ "$value" =~ ^[0-9]+$ ]] || die "invalid ${field} in ${file}: ${value}"
  printf "%s" "$value"
}

dr_sum_uint_field_from_files() {
  local field="$1"
  shift
  local value
  value="$(jq -s --arg field "$field" '[.[].data[$field] // 0] | add' "$@")"
  [[ "$value" =~ ^[0-9]+$ ]] || die "invalid ${field} sum: ${value}"
  printf "%s" "$value"
}

dr_fast_path_total_from_file() {
  local file="$1"
  local value
  value="$(jq -r '.data.flat_accumulator_fast_path_total // 0' "$file")"
  [[ "$value" =~ ^[0-9]+$ ]] || die "invalid flat_accumulator_fast_path_total in ${file}: ${value}"
  printf "%s" "$value"
}

dr_reconcile_count_from_file() {
  local file="$1"
  local value
  value="$(jq -r '.data.reconcile_count // 0' "$file")"
  [[ "$value" =~ ^[0-9]+$ ]] || die "invalid reconcile_count in ${file}: ${value}"
  printf "%s" "$value"
}

dr_last_applied_index_from_file() {
  local file="$1"
  local value
  value="$(jq -r '.data.last_applied_index // 0' "$file")"
  [[ "$value" =~ ^[0-9]+$ ]] || die "invalid last_applied_index in ${file}: ${value}"
  printf "%s" "$value"
}

wait_secondary_ready() {
  local name="$1"
  local addr="$2"
  local timeout="${3:-240}"
  local deadline=$(( $(date +%s) + timeout ))
  local out mode state lag primary_index last_applied

  while true; do
    out="$(bao_for "$addr" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status 2>/dev/null || true)"
    mode="$(jq -r '.data.mode // ""' <<<"$out" 2>/dev/null || true)"
    state="$(jq -r '.data.secondary_state // ""' <<<"$out" 2>/dev/null || true)"
    lag="$(jq -r '.data.lag_entries // 0' <<<"$out" 2>/dev/null || echo 0)"
    primary_index="$(jq -r '.data.primary_index // 0' <<<"$out" 2>/dev/null || echo 0)"
    last_applied="$(jq -r '.data.last_applied_index // 0' <<<"$out" 2>/dev/null || echo 0)"
    if [[ "$mode" == "secondary" && "$state" == "streaming" && "$lag" == "0" && "$primary_index" =~ ^[0-9]+$ && "$last_applied" =~ ^[0-9]+$ && "$primary_index" -gt 0 && "$last_applied" -ge "$primary_index" ]]; then
      echo "${name} ready: state=${state} lag=${lag} applied=${last_applied} primary=${primary_index}"
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      echo "$out" | jq '.data // {}' >&2 || true
      die "timed out waiting for ${name} to reach streaming lag=0 with a current primary index"
    fi
    sleep 2
  done
}

wait_flat_accumulator_fast_path_increment() {
  local name="$1"
  local addr="$2"
  local before="$3"
  local timeout="${4:-240}"
  local out_file="$5"
  local deadline=$(( $(date +%s) + timeout ))
  local out after state lag

  while true; do
    out="$(dr_status_json "$addr" "$DR_PRIMARY_TOKEN" 2>/dev/null || true)"
    after="$(jq -r '.data.flat_accumulator_fast_path_total // 0' <<<"$out" 2>/dev/null || printf "0")"
    state="$(jq -r '.data.secondary_state // ""' <<<"$out" 2>/dev/null || true)"
    lag="$(jq -r '.data.lag_entries // 0' <<<"$out" 2>/dev/null || printf "0")"

    if [[ "$after" =~ ^[0-9]+$ && "$state" == "streaming" && "$lag" == "0" && "$after" -gt "$before" ]]; then
      printf "%s\n" "$out" >"$out_file"
      echo "${name} flat accumulator fast path incremented: ${before} -> ${after}"
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      printf "%s\n" "$out" >"$out_file" || true
      echo "$out" | jq '.data // {}' >&2 || true
      die "timed out waiting for ${name} flat accumulator fast path counter to increment beyond ${before}"
    fi
    sleep 2
  done
}

wait_quiescent_reconnect_optimized() {
  local name="$1"
  local addr="$2"
  local before_fast="$3"
  local before_reconcile="$4"
  local timeout="${5:-240}"
  local out_file="$6"
  local deadline=$(( $(date +%s) + timeout ))
  local out after_fast after_reconcile state lag

  while true; do
    out="$(dr_status_json "$addr" "$DR_PRIMARY_TOKEN" 2>/dev/null || true)"
    after_fast="$(jq -r '.data.flat_accumulator_fast_path_total // 0' <<<"$out" 2>/dev/null || printf "0")"
    after_reconcile="$(jq -r '.data.reconcile_count // 0' <<<"$out" 2>/dev/null || printf "0")"
    state="$(jq -r '.data.secondary_state // ""' <<<"$out" 2>/dev/null || true)"
    lag="$(jq -r '.data.lag_entries // 0' <<<"$out" 2>/dev/null || printf "0")"

    if [[ "$after_fast" =~ ^[0-9]+$ && "$after_reconcile" =~ ^[0-9]+$ && "$state" == "streaming" && "$lag" == "0" ]]; then
      printf "%s\n" "$out" >"$out_file"
      if [[ "$after_fast" -gt "$before_fast" ]]; then
        echo "${name} optimized reconnect: flat accumulator fast path ${before_fast} -> ${after_fast}"
        return 0
      fi
      if [[ "$after_reconcile" -eq "$before_reconcile" ]]; then
        echo "${name} optimized reconnect: stream resumed without reconciliation"
        return 0
      fi
      echo "$out" | jq '.data // {}' >&2 || true
      die "${name} reconnected via scanned reconciliation: reconcile_count ${before_reconcile} -> ${after_reconcile}, flat_accumulator_fast_path_total ${before_fast} -> ${after_fast}"
    fi
    if (( $(date +%s) >= deadline )); then
      printf "%s\n" "$out" >"$out_file" || true
      echo "$out" | jq '.data // {}' >&2 || true
      die "timed out waiting for ${name} optimized quiescent reconnect"
    fi
    sleep 2
  done
}

wait_read_jq() {
  local label="$1"
  local addr="$2"
  local token="$3"
  local path="$4"
  local expr="$5"
  local timeout="${6:-120}"
  local deadline=$(( $(date +%s) + timeout ))
  local out=""

  while true; do
    if out="$(bao_for "$addr" "$token" read -format=json "$path" 2>/dev/null)" && jq -e "$expr" >/dev/null <<<"$out"; then
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      printf "%s\n" "$out" >&2
      die "timed out waiting for ${label} to satisfy ${path} ${expr}"
    fi
    sleep 2
  done
}

wait_kv_jq() {
  local label="$1"
  local addr="$2"
  local token="$3"
  local key_name="$4"
  local expr="$5"
  local timeout="${6:-120}"
  local deadline=$(( $(date +%s) + timeout ))
  local out=""

  while true; do
    if out="$(bao_for "$addr" "$token" kv get -format=json "kv/${key_name}" 2>/dev/null)" && jq -e "$expr" >/dev/null <<<"$out"; then
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      printf "%s\n" "$out" >&2
      die "timed out waiting for ${label} to satisfy kv/${key_name} ${expr}"
    fi
    sleep 2
  done
}

wait_namespace_lookup_jq() {
  local label="$1"
  local addr="$2"
  local token="$3"
  local ns_path="$4"
  local expr="$5"
  local timeout="${6:-120}"
  local deadline=$(( $(date +%s) + timeout ))
  local out=""

  while true; do
    if out="$(bao_for "$addr" "$token" namespace lookup -format=json "$ns_path" 2>/dev/null)" && jq -e "$expr" >/dev/null <<<"$out"; then
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      printf "%s\n" "$out" >&2
      die "timed out waiting for ${label} namespace ${ns_path} to satisfy ${expr}"
    fi
    sleep 2
  done
}

wait_namespace_kv_jq() {
  local label="$1"
  local addr="$2"
  local token="$3"
  local ns_path="$4"
  local key_name="$5"
  local expr="$6"
  local timeout="${7:-120}"
  local deadline=$(( $(date +%s) + timeout ))
  local out=""

  while true; do
    if out="$(bao_for_ns "$addr" "$token" "$ns_path" kv get -format=json "kv/${key_name}" 2>/dev/null)" && jq -e "$expr" >/dev/null <<<"$out"; then
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      printf "%s\n" "$out" >&2
      die "timed out waiting for ${label} namespace ${ns_path} to satisfy kv/${key_name} ${expr}"
    fi
    sleep 2
  done
}

wait_token_self_lookup_jq() {
  local label="$1"
  local addr="$2"
  local lookup_token="$3"
  local expr="$4"
  local timeout="${5:-120}"
  local deadline=$(( $(date +%s) + timeout ))
  local out=""

  while true; do
    if out="$(bao_for "$addr" "$lookup_token" token lookup -format=json 2>&1)" && jq -e "$expr" >/dev/null <<<"$out"; then
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      printf "%s\n" "$out" >&2
      die "timed out waiting for ${label} token self lookup to satisfy ${expr}"
    fi
    sleep 2
  done
}

cluster_active_addr() {
  local addr status
  for addr in "$@"; do
    if status="$(status_json "$addr" 2>/dev/null)"; then
      if jq -e '.sealed == false and (.is_self == true)' >/dev/null <<<"$status"; then
        printf "%s\n" "$addr"
        return 0
      fi
    fi
  done
  return 1
}

wait_cluster_active_addr() {
  local name="$1"
  local timeout="$2"
  shift 2
  local deadline=$(( $(date +%s) + timeout ))
  local active=""

  while true; do
    active="$(cluster_active_addr "$@" || true)"
    if [[ -n "$active" ]]; then
      printf "%s\n" "$active"
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      die "timed out waiting for ${name} active node"
    fi
    sleep 2
  done
}

assert_secret_mount_present() {
  local label="$1"
  local addr="$2"
  local token="$3"
  local mount_path="$4"
  local out

  out="$(bao_for "$addr" "$token" secrets list -format=json)"
  jq -e --arg path "${mount_path}/" 'has($path)' >/dev/null <<<"$out" || die "expected ${label} to have secret mount ${mount_path}/"
}

assert_auth_mount_present() {
  local label="$1"
  local addr="$2"
  local token="$3"
  local mount_path="$4"
  local out

  out="$(bao_for "$addr" "$token" auth list -format=json)"
  jq -e --arg path "${mount_path}/" 'has($path)' >/dev/null <<<"$out" || die "expected ${label} to have auth mount ${mount_path}/"
}

assert_namespace_secret_mount_present() {
  local label="$1"
  local addr="$2"
  local token="$3"
  local ns_path="$4"
  local mount_path="$5"
  local out

  out="$(bao_for_ns "$addr" "$token" "$ns_path" secrets list -format=json)"
  jq -e --arg path "${mount_path}/" 'has($path)' >/dev/null <<<"$out" || die "expected ${label} namespace ${ns_path} to have secret mount ${mount_path}/"
}

assert_policy_contains() {
  local label="$1"
  local addr="$2"
  local token="$3"
  local policy_name="$4"
  local expected="$5"
  local out

  out="$(bao_for "$addr" "$token" policy read "$policy_name")" || die "expected ${label} to read policy ${policy_name}"
  grep -q "$expected" <<<"$out" || die "expected ${label} policy ${policy_name} to contain ${expected}"
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
  local primary_addr="$DR_PRIMARY_ADDR"
  local secondary1_addr="$DR_SECONDARY1_ADDR"
  local secondary2_addr="$DR_SECONDARY2_ADDR"

  if [[ "$TOPOLOGY" == "ha" ]]; then
    primary_addr="$(cluster_active_addr "${PRIMARY_NODE_ADDRS[@]}" || printf "%s" "$DR_PRIMARY_ADDR")"
    secondary1_addr="$(cluster_active_addr "${SECONDARY1_NODE_ADDRS[@]}" || printf "%s" "$DR_SECONDARY1_ADDR")"
    secondary2_addr="$(cluster_active_addr "${SECONDARY2_NODE_ADDRS[@]}" || printf "%s" "$DR_SECONDARY2_ADDR")"
  fi

  dr_status "primary" "$primary_addr" "$DR_PRIMARY_TOKEN"
  dr_status "secondary1" "$secondary1_addr" "$DR_PRIMARY_TOKEN"
  dr_status "secondary2" "$secondary2_addr" "$DR_PRIMARY_TOKEN"
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

verify_engine_matrix_target() {
  local label="$1"
  local addr="$2"
  local token="$3"
  local run_id="$4"
  local ns_path="$5"
  local transit_path="$6"
  local pki_path="$7"
  local userpass_path="$8"
  local approle_path="$9"
  local policy_name="${10}"
  local entity_id="${11}"
  local group_id="${12}"
  local alias_id="${13}"
  local issued_serial="${14}"
  local client_token="${15}"
  local kv1_path="${16}"
  local ssh_path="${17}"
  local totp_path="${18}"
  local database_path="${19}"
  local cert_path="${20}"
  local jwt_path="${21}"
  local token_role="${22}"

  echo "Verifying engine/runtime matrix on ${label}..."
  assert_secret_mount_present "$label" "$addr" "$token" "kv"
  assert_secret_mount_present "$label" "$addr" "$token" "$kv1_path"
  assert_secret_mount_present "$label" "$addr" "$token" "$transit_path"
  assert_secret_mount_present "$label" "$addr" "$token" "$pki_path"
  assert_secret_mount_present "$label" "$addr" "$token" "$ssh_path"
  assert_secret_mount_present "$label" "$addr" "$token" "$totp_path"
  assert_secret_mount_present "$label" "$addr" "$token" "$database_path"
  assert_auth_mount_present "$label" "$addr" "$token" "$userpass_path"
  assert_auth_mount_present "$label" "$addr" "$token" "$approle_path"
  assert_auth_mount_present "$label" "$addr" "$token" "$cert_path"
  assert_auth_mount_present "$label" "$addr" "$token" "$jwt_path"
  assert_policy_contains "$label" "$addr" "$token" "$policy_name" "$run_id"

  wait_kv_jq "$label" "$addr" "$token" "${run_id}/app/config" '.data.data.app == "payments" and .data.data.region == "eu"'
  wait_read_jq "$label" "$addr" "$token" "${kv1_path}/app/config" ".data.engine == \"kv-v1\" and .data.run_id == \"${run_id}\""
  wait_namespace_lookup_jq "$label" "$addr" "$token" "$ns_path" ".data.path == \"${ns_path}/\" and .data.custom_metadata.run_id == \"${run_id}\""
  assert_namespace_secret_mount_present "$label" "$addr" "$token" "$ns_path" "kv"
  wait_namespace_kv_jq "$label" "$addr" "$token" "$ns_path" "app/config" ".data.data.scope == \"namespace\" and .data.data.run_id == \"${run_id}\""
  wait_read_jq "$label" "$addr" "$token" "${transit_path}/keys/app-key" '.data.type == "aes256-gcm96"'
  wait_read_jq "$label" "$addr" "$token" "${pki_path}/cert/ca" '.data.certificate != ""'
  wait_read_jq "$label" "$addr" "$token" "${pki_path}/roles/app" '.data.allowed_domains | index("example.test")'
  wait_read_jq "$label" "$addr" "$token" "${pki_path}/cert/${issued_serial}" '.data.certificate != ""'
  wait_read_jq "$label" "$addr" "$token" "${ssh_path}/config/ca" '.data.public_key != ""'
  wait_read_jq "$label" "$addr" "$token" "${ssh_path}/roles/app" '.data.key_type == "ca" and .data.default_user == "bao"'
  wait_read_jq "$label" "$addr" "$token" "${totp_path}/keys/app" ".data.issuer == \"${run_id}\" and .data.account_name == \"app@example.com\""
  wait_read_jq "$label" "$addr" "$token" "${database_path}/config/app" '.data.plugin_name == "postgresql-database-plugin" and (.data.allowed_roles | index("app"))'
  wait_read_jq "$label" "$addr" "$token" "auth/${userpass_path}/users/alice" ".data.policies | index(\"${policy_name}\")"
  wait_read_jq "$label" "$addr" "$token" "auth/${approle_path}/role/app/role-id" '.data.role_id != ""'
  wait_read_jq "$label" "$addr" "$token" "auth/${cert_path}/certs/app" ".data.display_name == \"${run_id}-cert\" and .data.certificate != \"\""
  wait_read_jq "$label" "$addr" "$token" "auth/${jwt_path}/config" '.data.jwt_validation_pubkeys[0] != ""'
  wait_read_jq "$label" "$addr" "$token" "auth/${jwt_path}/role/app" '.data.role_type == "jwt" and (.data.bound_audiences | index("dr-engine"))'
  wait_read_jq "$label" "$addr" "$token" "auth/token/roles/${token_role}" ".data.allowed_policies | index(\"${policy_name}\")"
  wait_read_jq "$label" "$addr" "$token" "identity/entity/id/${entity_id}" ".data.policies | index(\"${policy_name}\")"
  wait_read_jq "$label" "$addr" "$token" "identity/group/id/${group_id}" ".data.member_entity_ids | index(\"${entity_id}\")"
  wait_read_jq "$label" "$addr" "$token" "identity/entity-alias/id/${alias_id}" ".data.canonical_id == \"${entity_id}\""
  wait_token_self_lookup_jq "$label" "$addr" "$client_token" ".data.policies | index(\"${policy_name}\")"
}

cmd_engine_matrix() {
  need_bin jq
  load_env

  local run_id run_dir ns_path kv1_path transit_path pki_path ssh_path totp_path database_path
  local userpass_path approle_path cert_path jwt_path token_role policy_name
  local policy_file jwt_pubkey plaintext ciphertext decrypted token_json client_token
  local pki_json pki_ca_cert issued_serial auth_json entity_json entity_id alias_json alias_id group_json group_id userpass_accessor
  local secondary1_verify_addr secondary2_verify_addr

  run_id="engine-matrix-$(date -u +%Y%m%d%H%M%S)"
  run_dir="${RESULTS_DIR}/${run_id}"
  ns_path="ns-${run_id}"
  kv1_path="kv1-${run_id}"
  transit_path="transit-${run_id}"
  pki_path="pki-${run_id}"
  ssh_path="ssh-${run_id}"
  totp_path="totp-${run_id}"
  database_path="database-${run_id}"
  userpass_path="userpass-${run_id}"
  approle_path="approle-${run_id}"
  cert_path="cert-${run_id}"
  jwt_path="jwt-${run_id}"
  token_role="token-${run_id}"
  policy_name="dr-engine-${run_id}"
  mkdir -p "$run_dir"

  echo "Preparing engine/runtime matrix: ${run_id}"
  ensure_kv_mount

  echo "Seeding namespace state..."
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" namespace create -format=json \
    -custom-metadata=run_id="$run_id" \
    "$ns_path" >"${run_dir}/namespace.json"
  bao_for_ns "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$ns_path" secrets enable -path=kv kv-v2 >/dev/null
  bao_for_ns "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$ns_path" kv put "kv/app/config" \
    scope=namespace \
    app=payments \
    run_id="$run_id" >/dev/null

  policy_file="$(mktemp)"
  printf 'path "kv/data/%s/*" {\n  capabilities = ["read"]\n}\npath "%s/*" {\n  capabilities = ["read"]\n}\n' "$run_id" "$transit_path" >"$policy_file"
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" policy write "$policy_name" "$policy_file" >/dev/null
  rm -f "$policy_file"

  echo "Seeding KV v2 data..."
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" kv put "kv/${run_id}/app/config" \
    app=payments \
    region=eu \
    tier=prodlike \
    run_id="$run_id" >/dev/null

  echo "Seeding KV v1 data..."
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" secrets enable -path="$kv1_path" kv >/dev/null
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write "${kv1_path}/app/config" \
    engine=kv-v1 \
    run_id="$run_id" >/dev/null

  echo "Seeding transit key material..."
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" secrets enable -path="$transit_path" transit >/dev/null
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write -f "${transit_path}/keys/app-key" >/dev/null
  plaintext="$(printf 'dr-engine-matrix-%s' "$run_id" | base64)"
  ciphertext="$(bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write -format=json "${transit_path}/encrypt/app-key" plaintext="$plaintext" | jq -r '.data.ciphertext')"
  decrypted="$(bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write -format=json "${transit_path}/decrypt/app-key" ciphertext="$ciphertext" | jq -r '.data.plaintext')"
  [[ "$decrypted" == "$plaintext" ]] || die "transit decrypt did not round-trip on primary"

  echo "Seeding PKI config, role, and issued certificate..."
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" secrets enable -path="$pki_path" pki >/dev/null
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" secrets tune -max-lease-ttl=8760h "$pki_path" >/dev/null
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write -format=json "${pki_path}/root/generate/internal" \
    common_name="${run_id}.example.test" \
    ttl=8760h >"${run_dir}/pki-root.json"
  pki_ca_cert="$(jq -r '.data.certificate' "${run_dir}/pki-root.json")"
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write "${pki_path}/roles/app" \
    allowed_domains=example.test \
    allow_subdomains=true \
    max_ttl=72h >/dev/null
  pki_json="$(bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write -format=json "${pki_path}/issue/app" \
    common_name="api.example.test" \
    ttl=1h)"
  printf "%s\n" "$pki_json" >"${run_dir}/pki-issued.json"
  issued_serial="$(jq -r '.data.serial_number' <<<"$pki_json")"
  [[ -n "$issued_serial" && "$issued_serial" != "null" ]] || die "PKI issue did not return a serial number"

  echo "Seeding SSH, TOTP, and database secret engines..."
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" secrets enable -path="$ssh_path" ssh >/dev/null
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write "${ssh_path}/config/ca" \
    generate_signing_key=true \
    key_type=ec >/dev/null
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write "${ssh_path}/roles/app" \
    key_type=ca \
    allow_user_certificates=true \
    allowed_users=bao \
    default_user=bao \
    ttl=30m >/dev/null

  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" secrets enable -path="$totp_path" totp >/dev/null
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write "${totp_path}/keys/app" \
    generate=true \
    exported=false \
    issuer="$run_id" \
    account_name=app@example.com \
    period=30 \
    algorithm=SHA1 \
    digits=6 \
    qr_size=0 >/dev/null

  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" secrets enable -path="$database_path" database >/dev/null
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write "${database_path}/config/app" \
    plugin_name=postgresql-database-plugin \
    connection_url='postgresql://{{username}}:{{password}}@localhost:5432/postgres?sslmode=disable' \
    allowed_roles=app \
    verify_connection=false >/dev/null

  echo "Seeding userpass and AppRole auth mounts..."
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" auth enable -path="$userpass_path" userpass >/dev/null
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write "auth/${userpass_path}/users/alice" \
    password="correct-horse-${run_id}" \
    policies="$policy_name" >/dev/null
  auth_json="$(bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write -format=json "auth/${userpass_path}/login/alice" password="correct-horse-${run_id}")"
  printf "%s\n" "$auth_json" >"${run_dir}/userpass-login.json"

  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" auth enable -path="$approle_path" approle >/dev/null
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write "auth/${approle_path}/role/app" \
    token_policies="$policy_name" \
    token_ttl=1h >/dev/null

  echo "Seeding cert, JWT, and token-role auth state..."
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" auth enable -path="$cert_path" cert >/dev/null
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write "auth/${cert_path}/certs/app" \
    display_name="${run_id}-cert" \
    certificate="$pki_ca_cert" \
    policies="$policy_name" >/dev/null

  jwt_pubkey='-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEEVs/o5+uQbTjL3chynL4wXgUg2R9
q9UU8I5mEovUf86QZ7kOBIjJwqnzD1omageEHWwHdBO6B+dFabmdT9POxg==
-----END PUBLIC KEY-----'
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" auth enable -path="$jwt_path" jwt >/dev/null
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write "auth/${jwt_path}/config" \
    jwt_validation_pubkeys="$jwt_pubkey" >/dev/null
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write "auth/${jwt_path}/role/app" \
    role_type=jwt \
    bound_audiences=dr-engine \
    user_claim=sub \
    policies="$policy_name" >/dev/null

  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write "auth/token/roles/${token_role}" \
    allowed_policies="$policy_name" \
    orphan=true \
    token_ttl=1h >/dev/null

  echo "Seeding token and identity state..."
  token_json="$(bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" token create -format=json -policy="$policy_name" -ttl=1h)"
  printf "%s\n" "$token_json" >"${run_dir}/token.json"
  client_token="$(jq -r '.auth.client_token' <<<"$token_json")"
  [[ -n "$client_token" && "$client_token" != "null" ]] || die "token create did not return a client token"

  userpass_accessor="$(bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" auth list -format=json | jq -r --arg path "${userpass_path}/" '.[$path].accessor')"
  [[ -n "$userpass_accessor" && "$userpass_accessor" != "null" ]] || die "could not find userpass accessor"
  entity_json="$(bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write -format=json identity/entity name="entity-${run_id}" policies="$policy_name")"
  printf "%s\n" "$entity_json" >"${run_dir}/identity-entity.json"
  entity_id="$(jq -r '.data.id' <<<"$entity_json")"
  [[ -n "$entity_id" && "$entity_id" != "null" ]] || die "identity entity create did not return an ID"
  alias_json="$(bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write -format=json identity/entity-alias \
    name="alice-${run_id}" \
    canonical_id="$entity_id" \
    mount_accessor="$userpass_accessor")"
  printf "%s\n" "$alias_json" >"${run_dir}/identity-alias.json"
  alias_id="$(jq -r '.data.id' <<<"$alias_json")"
  [[ -n "$alias_id" && "$alias_id" != "null" ]] || die "identity alias create did not return an ID"
  group_json="$(bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" write -format=json identity/group \
    name="group-${run_id}" \
    type=internal \
    policies="$policy_name" \
    member_entity_ids="$entity_id")"
  printf "%s\n" "$group_json" >"${run_dir}/identity-group.json"
  group_id="$(jq -r '.data.id' <<<"$group_json")"
  [[ -n "$group_id" && "$group_id" != "null" ]] || die "identity group create did not return an ID"

  wait_secondary_ready "secondary1" "$DR_SECONDARY1_ADDR" 180
  wait_secondary_ready "secondary2" "$DR_SECONDARY2_ADDR" 180
  secondary1_verify_addr="$(wait_cluster_active_addr "secondary1" 120 "${SECONDARY1_NODE_ADDRS[@]}")"
  secondary2_verify_addr="$(wait_cluster_active_addr "secondary2" 120 "${SECONDARY2_NODE_ADDRS[@]}")"

  verify_engine_matrix_target "primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$run_id" "$ns_path" "$transit_path" "$pki_path" "$userpass_path" "$approle_path" "$policy_name" "$entity_id" "$group_id" "$alias_id" "$issued_serial" "$client_token" "$kv1_path" "$ssh_path" "$totp_path" "$database_path" "$cert_path" "$jwt_path" "$token_role"
  verify_engine_matrix_target "secondary1" "$secondary1_verify_addr" "$DR_PRIMARY_TOKEN" "$run_id" "$ns_path" "$transit_path" "$pki_path" "$userpass_path" "$approle_path" "$policy_name" "$entity_id" "$group_id" "$alias_id" "$issued_serial" "$client_token" "$kv1_path" "$ssh_path" "$totp_path" "$database_path" "$cert_path" "$jwt_path" "$token_role"
  verify_engine_matrix_target "secondary2" "$secondary2_verify_addr" "$DR_PRIMARY_TOKEN" "$run_id" "$ns_path" "$transit_path" "$pki_path" "$userpass_path" "$approle_path" "$policy_name" "$entity_id" "$group_id" "$alias_id" "$issued_serial" "$client_token" "$kv1_path" "$ssh_path" "$totp_path" "$database_path" "$cert_path" "$jwt_path" "$token_role"

  jq -n \
    --arg run_id "$run_id" \
    --arg ns_path "$ns_path" \
    --arg kv1_path "$kv1_path" \
    --arg transit_path "$transit_path" \
    --arg pki_path "$pki_path" \
    --arg ssh_path "$ssh_path" \
    --arg totp_path "$totp_path" \
    --arg database_path "$database_path" \
    --arg userpass_path "$userpass_path" \
    --arg approle_path "$approle_path" \
    --arg cert_path "$cert_path" \
    --arg jwt_path "$jwt_path" \
    --arg token_role "$token_role" \
    --arg policy_name "$policy_name" \
    --arg entity_id "$entity_id" \
    --arg group_id "$group_id" \
    --arg alias_id "$alias_id" \
    --arg issued_serial "$issued_serial" \
    '{run_id:$run_id,result:"pass",coverage:{namespaces:[$ns_path],secret_engines:["kv-v2",$kv1_path,$transit_path,$pki_path,$ssh_path,$totp_path,$database_path],auth_engines:[$userpass_path,$approle_path,$cert_path,$jwt_path],token_roles:[$token_role],policy:$policy_name,identity:{entity_id:$entity_id,group_id:$group_id,alias_id:$alias_id},pki_issued_serial:$issued_serial}}' >"${run_dir}/engine-matrix-summary.json"

  ENGINE_MATRIX_LAST_RUN_DIR="$run_dir"
  echo "Engine/runtime matrix passed."
  echo "Run dir: ${run_dir}"
}

verify_engine_matrix_run_dir() {
  local label="$1"
  local addr="$2"
  local token="$3"
  local run_dir="$4"
  local summary="${run_dir}/engine-matrix-summary.json"
  local token_file="${run_dir}/token.json"
  local run_id ns_path kv1_path transit_path pki_path ssh_path totp_path database_path
  local userpass_path approle_path cert_path jwt_path token_role policy_name
  local entity_id group_id alias_id issued_serial client_token

  [[ -s "$summary" ]] || die "missing engine matrix summary: ${summary}"
  [[ -s "$token_file" ]] || die "missing engine matrix token file: ${token_file}"

  run_id="$(jq -r '.run_id' "$summary")"
  ns_path="$(jq -r '.coverage.namespaces[0]' "$summary")"
  kv1_path="$(jq -r '.coverage.secret_engines[] | select(startswith("kv1-"))' "$summary")"
  transit_path="$(jq -r '.coverage.secret_engines[] | select(startswith("transit-"))' "$summary")"
  pki_path="$(jq -r '.coverage.secret_engines[] | select(startswith("pki-"))' "$summary")"
  ssh_path="$(jq -r '.coverage.secret_engines[] | select(startswith("ssh-"))' "$summary")"
  totp_path="$(jq -r '.coverage.secret_engines[] | select(startswith("totp-"))' "$summary")"
  database_path="$(jq -r '.coverage.secret_engines[] | select(startswith("database-"))' "$summary")"
  userpass_path="$(jq -r '.coverage.auth_engines[] | select(startswith("userpass-"))' "$summary")"
  approle_path="$(jq -r '.coverage.auth_engines[] | select(startswith("approle-"))' "$summary")"
  cert_path="$(jq -r '.coverage.auth_engines[] | select(startswith("cert-"))' "$summary")"
  jwt_path="$(jq -r '.coverage.auth_engines[] | select(startswith("jwt-"))' "$summary")"
  token_role="$(jq -r '.coverage.token_roles[0]' "$summary")"
  policy_name="$(jq -r '.coverage.policy' "$summary")"
  entity_id="$(jq -r '.coverage.identity.entity_id' "$summary")"
  group_id="$(jq -r '.coverage.identity.group_id' "$summary")"
  alias_id="$(jq -r '.coverage.identity.alias_id' "$summary")"
  issued_serial="$(jq -r '.coverage.pki_issued_serial' "$summary")"
  client_token="$(jq -r '.auth.client_token' "$token_file")"

  verify_engine_matrix_target "$label" "$addr" "$token" "$run_id" "$ns_path" "$transit_path" "$pki_path" "$userpass_path" "$approle_path" "$policy_name" "$entity_id" "$group_id" "$alias_id" "$issued_serial" "$client_token" "$kv1_path" "$ssh_path" "$totp_path" "$database_path" "$cert_path" "$jwt_path" "$token_role"
}

cmd_engine_lifecycle_matrix() {
  [[ "$TOPOLOGY" == "ha" ]] || die "engine-lifecycle-matrix requires --topology ha"
  need_bin jq
  load_env

  local run_dir promoted_addr secondary2_verify_addr lifecycle_summary

  echo "Running pre-failover engine/runtime matrix..."
  cmd_engine_matrix
  run_dir="${ENGINE_MATRIX_LAST_RUN_DIR:-}"
  [[ -n "$run_dir" && -d "$run_dir" ]] || die "engine matrix did not record a run directory"

  echo "Running failover smoke before promoted-cluster engine verification..."
  cmd_failover_smoke
  promoted_addr="$(wait_secondary1_active_addr 120)"
  verify_engine_matrix_run_dir "promoted secondary1" "$promoted_addr" "$DR_PRIMARY_TOKEN" "$run_dir"

  echo "Running explicit secondary2 reseed before promoted-lineage engine verification..."
  cmd_reseed_secondary_smoke
  secondary2_verify_addr="$(wait_cluster_active_addr "secondary2" 120 "${SECONDARY2_NODE_ADDRS[@]}")"
  verify_engine_matrix_run_dir "secondary2 promoted lineage" "$secondary2_verify_addr" "$DR_PRIMARY_TOKEN" "$run_dir"

  lifecycle_summary="${run_dir}/engine-lifecycle-summary.json"
  jq -n \
    --arg run_dir "$run_dir" \
    --arg result "pass" \
    '{run_dir:$run_dir,result:$result,checks:["pre_failover_primary_and_secondaries","promoted_secondary1_after_failover","secondary2_after_promoted_reseed"]}' >"$lifecycle_summary"

  echo "Engine/runtime lifecycle matrix passed."
  echo "Run dir: ${run_dir}"
  echo "Lifecycle summary: ${lifecycle_summary}"
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
  local deadline=$(( $(date +%s) + 60 ))
  local out

  while true; do
    if out="$(bao_for "$addr" "$token" kv put "kv/${key_name}" \
      phase="$phase" \
      run_id="$run_id" \
      ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)" 2>&1)"; then
      return 0
    fi
    if ! grep -qi "Upgrading from non-versioned to versioned data" <<<"$out"; then
      printf "%s\n" "$out" >&2
      return 1
    fi
    if (( $(date +%s) >= deadline )); then
      printf "%s\n" "$out" >&2
      return 1
    fi
    sleep 2
  done
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
  cluster_active_addr "${SECONDARY1_NODE_ADDRS[@]}"
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
    unseal_secondary1_dr "secondary1-$((i + 1))" "${SECONDARY1_NODE_ADDRS[$i]}"
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

capture_tuning_load_state() {
  local run_dir="$1"
  local label="$2"

  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status >"$run_dir/${label}-primary-status.json" 2>"$run_dir/${label}-primary-status.err" || true
  bao_for "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status >"$run_dir/${label}-secondary1-status.json" 2>"$run_dir/${label}-secondary1-status.err" || true
  bao_for "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/status >"$run_dir/${label}-secondary2-status.json" 2>"$run_dir/${label}-secondary2-status.err" || true
  bao_for "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/tuning >"$run_dir/${label}-primary-tuning.json" 2>"$run_dir/${label}-primary-tuning.err" || true
  bao_for "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/tuning >"$run_dir/${label}-secondary1-tuning.json" 2>"$run_dir/${label}-secondary1-tuning.err" || true
  bao_for "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" read -format=json sys/replication/dr/tuning >"$run_dir/${label}-secondary2-tuning.json" 2>"$run_dir/${label}-secondary2-tuning.err" || true
}

write_tuning_profile() {
  local label="$1"
  local addr="$2"
  local token="$3"
  local profile="$4"
  local run_dir="$5"
  local args=()

  case "$profile" in
    constrained)
      args=(
        checkpoint_ttl_seconds=900
        checkpoint_global_budget_bytes=536870912
        checkpoint_per_relationship_budget_bytes=134217728
        stream_buffer_max_entries=20000
        stream_buffer_max_bytes=134217728
        reconcile_max_rpc_bytes=67108864
        reconcile_max_wall_time_seconds=900
        reconcile_max_inflight_tasks=8
        stream_batch_max_entries=128
        stream_batch_max_bytes=524288
        stream_batch_max_wait_milliseconds=20
        stream_journal_enabled=true
        stream_journal_max_bytes=67108864
        stream_journal_segment_bytes=4194304
        stream_journal_retention_seconds=900
        reconcile_apply_workers=8
        reconcile_put_batch_max_entries=256
        reconcile_put_batch_max_bytes=1048576
        convergence_min_rate_ratio=0.40
        convergence_stall_seconds=90
        fallback_enabled=true
        fallback_stall_seconds=90
        fallback_failure_threshold=2
        fallback_min_lag_entries=256
        fallback_cooldown_seconds=180
        fallback_max_per_hour=4
        checkpoint_artifact_enabled=true
        checkpoint_artifact_global_budget_bytes=536870912
        checkpoint_artifact_per_relationship_budget_bytes=134217728
        checkpoint_artifact_ttl_seconds=900
        checkpoint_artifact_segment_bytes=4194304
        dr_backpressure_enabled=true
        dr_backpressure_degraded_ratio=0.75
        dr_backpressure_critical_ratio=0.50
        dr_backpressure_min_lag_entries=512
        dr_backpressure_horizon_seconds=60
        dr_backpressure_degraded_min_qps=96
        dr_backpressure_critical_min_qps=48
      )
      ;;
    out-of-horizon)
      args=(
        checkpoint_ttl_seconds=900
        checkpoint_global_budget_bytes=536870912
        checkpoint_per_relationship_budget_bytes=134217728
        stream_buffer_max_entries=1024
        stream_buffer_max_bytes=4194304
        reconcile_max_rpc_bytes=67108864
        reconcile_max_wall_time_seconds=900
        reconcile_max_inflight_tasks=8
        stream_batch_max_entries=128
        stream_batch_max_bytes=524288
        stream_batch_max_wait_milliseconds=20
        stream_journal_enabled=true
        stream_journal_max_bytes=262144
        stream_journal_segment_bytes=32768
        stream_journal_retention_seconds=30
        reconcile_apply_workers=8
        reconcile_put_batch_max_entries=256
        reconcile_put_batch_max_bytes=1048576
        convergence_min_rate_ratio=0.40
        convergence_stall_seconds=90
        fallback_enabled=true
        fallback_stall_seconds=90
        fallback_failure_threshold=2
        fallback_min_lag_entries=256
        fallback_cooldown_seconds=180
        fallback_max_per_hour=4
        checkpoint_artifact_enabled=true
        checkpoint_artifact_global_budget_bytes=536870912
        checkpoint_artifact_per_relationship_budget_bytes=134217728
        checkpoint_artifact_ttl_seconds=900
        checkpoint_artifact_segment_bytes=4194304
        dr_backpressure_enabled=false
        dr_backpressure_degraded_ratio=0.75
        dr_backpressure_critical_ratio=0.50
        dr_backpressure_min_lag_entries=512
        dr_backpressure_horizon_seconds=60
        dr_backpressure_degraded_min_qps=96
        dr_backpressure_critical_min_qps=48
      )
      ;;
    relaxed)
      args=(
        checkpoint_ttl_seconds=1800
        checkpoint_global_budget_bytes=1073741824
        checkpoint_per_relationship_budget_bytes=268435456
        stream_buffer_max_entries=50000
        stream_buffer_max_bytes=268435456
        reconcile_max_rpc_bytes=134217728
        reconcile_max_wall_time_seconds=1800
        reconcile_max_inflight_tasks=16
        stream_batch_max_entries=256
        stream_batch_max_bytes=1048576
        stream_batch_max_wait_milliseconds=10
        stream_journal_enabled=true
        stream_journal_max_bytes=4294967296
        stream_journal_segment_bytes=67108864
        stream_journal_retention_seconds=7200
        reconcile_apply_workers=16
        reconcile_put_batch_max_entries=512
        reconcile_put_batch_max_bytes=2097152
        convergence_min_rate_ratio=0.80
        convergence_stall_seconds=180
        fallback_enabled=true
        fallback_stall_seconds=180
        fallback_failure_threshold=3
        fallback_min_lag_entries=1024
        fallback_cooldown_seconds=600
        fallback_max_per_hour=2
        checkpoint_artifact_enabled=true
        checkpoint_artifact_global_budget_bytes=8589934592
        checkpoint_artifact_per_relationship_budget_bytes=2147483648
        checkpoint_artifact_ttl_seconds=1800
        checkpoint_artifact_segment_bytes=67108864
        dr_backpressure_enabled=true
        dr_backpressure_degraded_ratio=0.80
        dr_backpressure_critical_ratio=0.50
        dr_backpressure_min_lag_entries=1024
        dr_backpressure_horizon_seconds=180
        dr_backpressure_degraded_min_qps=50
        dr_backpressure_critical_min_qps=10
      )
      ;;
    *)
      die "unknown tuning profile: $profile"
      ;;
  esac

  bao_for "$addr" "$token" write sys/replication/dr/tuning "${args[@]}" >"$run_dir/tuning-${profile}-${label}.out" 2>"$run_dir/tuning-${profile}-${label}.err"
}

apply_tuning_profile_to_all() {
  local profile="$1"
  local run_dir="$2"

  echo "Applying ${profile} tuning profile..."
  write_tuning_profile "primary" "$DR_PRIMARY_ADDR" "$DR_PRIMARY_TOKEN" "$profile" "$run_dir"
  write_tuning_profile "secondary1" "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" "$profile" "$run_dir"
  write_tuning_profile "secondary2" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" "$profile" "$run_dir"
}

force_ha_handoff_for_tuning_load() {
  local run_dir="$1"
  local primary_active secondary1_active secondary2_active

  primary_active="$(wait_cluster_active_addr "primary" 120 "${PRIMARY_NODE_ADDRS[@]}")"
  secondary1_active="$(wait_cluster_active_addr "secondary1" 120 "${SECONDARY1_NODE_ADDRS[@]}")"
  secondary2_active="$(wait_cluster_active_addr "secondary2" 120 "${SECONDARY2_NODE_ADDRS[@]}")"

  echo "Forcing HA handoff after tuning: primary=${primary_active} secondary1=${secondary1_active} secondary2=${secondary2_active}" | tee -a "$run_dir/orchestrator.log"
  bao_for "$primary_active" "$DR_PRIMARY_TOKEN" write -f sys/step-down >"$run_dir/stepdown-primary.out" 2>"$run_dir/stepdown-primary.err" || true
  bao_for "$secondary1_active" "$DR_PRIMARY_TOKEN" write -f sys/step-down >"$run_dir/stepdown-secondary1.out" 2>"$run_dir/stepdown-secondary1.err" || true
  bao_for "$secondary2_active" "$DR_PRIMARY_TOKEN" write -f sys/step-down >"$run_dir/stepdown-secondary2.out" 2>"$run_dir/stepdown-secondary2.err" || true

  wait_cluster_active_addr "primary after handoff" 120 "${PRIMARY_NODE_ADDRS[@]}" >/dev/null
  wait_secondary_ready "secondary1 after handoff" "$DR_SECONDARY1_ADDR" 240
  wait_secondary_ready "secondary2 after handoff" "$DR_SECONDARY2_ADDR" 240
}

cmd_quiescent_reconnect_smoke() {
  [[ "$TOPOLOGY" == "ha" ]] || die "quiescent-reconnect-smoke requires --topology ha"
  need_bin jq

  local do_reset=true
  local do_build=false
  local timeout=240

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --timeout)
        timeout="${2:?missing value for --timeout}"
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
        die "unknown quiescent-reconnect-smoke option: $1"
        ;;
    esac
  done

  [[ "$timeout" =~ ^[0-9]+$ ]] || die "--timeout must be an integer"
  (( timeout > 0 )) || die "--timeout must be > 0"
  if [[ "$do_reset" == "false" && "$do_build" == "true" ]]; then
    die "--build cannot be used with --no-reset"
  fi

  if [[ "$do_reset" == "true" ]]; then
    if [[ "$do_build" == "true" ]]; then
      cmd_reset --build
    else
      cmd_reset
    fi
  else
    load_env
    wait_secondary_ready "secondary1 pre-handoff" "$DR_SECONDARY1_ADDR" "$timeout"
    wait_secondary_ready "secondary2 pre-handoff" "$DR_SECONDARY2_ADDR" "$timeout"
  fi

  local run_id run_dir marker_key primary_active primary_after before_fast1 before_fast2 before_reconcile1 before_reconcile2
  run_id="quiescent-reconnect-$(date -u +%Y%m%dT%H%M%SZ)"
  run_dir="${RESULTS_DIR}/${run_id}"
  marker_key="${run_id}/marker"
  mkdir -p "$run_dir"

  {
    echo "run_id=${run_id}"
    echo "run_dir=${run_dir}"
    echo "started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "timeout=${timeout}"
  } | tee "$run_dir/orchestrator.log"

  primary_active="$(wait_cluster_active_addr "primary" 120 "${PRIMARY_NODE_ADDRS[@]}")"
  echo "primary_active_before=${primary_active}" | tee -a "$run_dir/orchestrator.log"

  ensure_kv_mount_on "$primary_active" "$DR_PRIMARY_TOKEN"
  kv_put "$primary_active" "$DR_PRIMARY_TOKEN" "$marker_key" "pre-handoff" "$run_id"
  wait_secondary_ready "secondary1 pre-handoff" "$DR_SECONDARY1_ADDR" "$timeout"
  wait_secondary_ready "secondary2 pre-handoff" "$DR_SECONDARY2_ADDR" "$timeout"
  assert_kv_present "secondary1 pre-handoff" "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" "$marker_key"
  assert_kv_present "secondary2 pre-handoff" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" "$marker_key"

  dr_status_json "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" >"$run_dir/before-secondary1-status.json"
  dr_status_json "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" >"$run_dir/before-secondary2-status.json"
  before_fast1="$(dr_fast_path_total_from_file "$run_dir/before-secondary1-status.json")"
  before_fast2="$(dr_fast_path_total_from_file "$run_dir/before-secondary2-status.json")"
  before_reconcile1="$(dr_reconcile_count_from_file "$run_dir/before-secondary1-status.json")"
  before_reconcile2="$(dr_reconcile_count_from_file "$run_dir/before-secondary2-status.json")"
  echo "fast_path_before_secondary1=${before_fast1}" | tee -a "$run_dir/orchestrator.log"
  echo "fast_path_before_secondary2=${before_fast2}" | tee -a "$run_dir/orchestrator.log"
  echo "reconcile_count_before_secondary1=${before_reconcile1}" | tee -a "$run_dir/orchestrator.log"
  echo "reconcile_count_before_secondary2=${before_reconcile2}" | tee -a "$run_dir/orchestrator.log"

  echo "forcing_primary_handoff_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$run_dir/orchestrator.log"
  bao_for "$primary_active" "$DR_PRIMARY_TOKEN" write -f sys/step-down >"$run_dir/stepdown-primary.out" 2>"$run_dir/stepdown-primary.err" || true
  primary_after="$(wait_cluster_active_addr "primary after handoff" 120 "${PRIMARY_NODE_ADDRS[@]}")"
  echo "primary_active_after=${primary_after}" | tee -a "$run_dir/orchestrator.log"

  wait_secondary_ready "secondary1 after handoff" "$DR_SECONDARY1_ADDR" "$timeout"
  wait_secondary_ready "secondary2 after handoff" "$DR_SECONDARY2_ADDR" "$timeout"
  wait_quiescent_reconnect_optimized "secondary1" "$DR_SECONDARY1_ADDR" "$before_fast1" "$before_reconcile1" "$timeout" "$run_dir/after-secondary1-status.json"
  wait_quiescent_reconnect_optimized "secondary2" "$DR_SECONDARY2_ADDR" "$before_fast2" "$before_reconcile2" "$timeout" "$run_dir/after-secondary2-status.json"

  assert_kv_present "primary after handoff" "$primary_after" "$DR_PRIMARY_TOKEN" "$marker_key"
  assert_kv_present "secondary1 after handoff" "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" "$marker_key"
  assert_kv_present "secondary2 after handoff" "$DR_SECONDARY2_ADDR" "$DR_PRIMARY_TOKEN" "$marker_key"

  echo "completed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$run_dir/orchestrator.log"
  echo "Quiescent reconnect fast-path smoke passed."
  echo "Run: ${run_dir}"
}

cmd_accumulator_cold_restart_smoke() {
  [[ "$TOPOLOGY" == "ha" ]] || die "accumulator-cold-restart-smoke requires --topology ha"
  need_bin jq
  need_bin rg

  local do_reset=true
  local do_build=false
  local timeout=240
  local stop_seconds=10
  local secondary1_active=""
  local secondary2_active=""

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --timeout)
        timeout="${2:?missing value for --timeout}"
        shift 2
        ;;
      --stop-seconds)
        stop_seconds="${2:?missing value for --stop-seconds}"
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
        die "unknown accumulator-cold-restart-smoke option: $1"
        ;;
    esac
  done

  [[ "$timeout" =~ ^[0-9]+$ ]] || die "--timeout must be an integer"
  [[ "$stop_seconds" =~ ^[0-9]+$ ]] || die "--stop-seconds must be an integer"
  (( timeout > 0 )) || die "--timeout must be > 0"
  (( stop_seconds >= 0 )) || die "--stop-seconds must be >= 0"
  if [[ "$do_reset" == "false" && "$do_build" == "true" ]]; then
    die "--build cannot be used with --no-reset"
  fi

  if [[ "$do_reset" == "true" ]]; then
    if [[ "$do_build" == "true" ]]; then
      cmd_reset --build
    else
      cmd_reset
    fi
  else
    load_env
    secondary1_active="$(wait_cluster_active_addr "secondary1 pre-restart" 120 "${SECONDARY1_NODE_ADDRS[@]}")"
    secondary2_active="$(wait_cluster_active_addr "secondary2 pre-restart" 120 "${SECONDARY2_NODE_ADDRS[@]}")"
    wait_secondary_ready "secondary1 pre-restart" "$secondary1_active" "$timeout"
    wait_secondary_ready "secondary2 pre-restart" "$secondary2_active" "$timeout"
  fi

  local run_id run_dir marker_key primary_active before_last before_reconcile before_scan_failures before_local_kid_fallback before_full_bucket before_proof_mismatch restart_since
  local after_last after_reconcile after_cursor after_snapshot after_scan_failures after_local_kid_fallback after_full_bucket after_proof_mismatch
  local services=(secondary1-1 secondary1-2 secondary1-3)

  run_id="accumulator-cold-restart-$(date -u +%Y%m%dT%H%M%SZ)"
  run_dir="${RESULTS_DIR}/${run_id}"
  marker_key="${run_id}/marker"
  mkdir -p "$run_dir"

  {
    echo "run_id=${run_id}"
    echo "run_dir=${run_dir}"
    echo "started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "timeout=${timeout}"
    echo "stop_seconds=${stop_seconds}"
  } | tee "$run_dir/orchestrator.log"

  primary_active="$(wait_cluster_active_addr "primary" 120 "${PRIMARY_NODE_ADDRS[@]}")"
  echo "primary_active=${primary_active}" | tee -a "$run_dir/orchestrator.log"

  ensure_kv_mount_on "$primary_active" "$DR_PRIMARY_TOKEN"
  kv_put "$primary_active" "$DR_PRIMARY_TOKEN" "$marker_key" "pre-cold-restart" "$run_id"
  secondary1_active="$(wait_cluster_active_addr "secondary1 pre-restart" 120 "${SECONDARY1_NODE_ADDRS[@]}")"
  secondary2_active="$(wait_cluster_active_addr "secondary2 pre-restart" 120 "${SECONDARY2_NODE_ADDRS[@]}")"
  echo "secondary1_active_before=${secondary1_active}" | tee -a "$run_dir/orchestrator.log"
  echo "secondary2_active_before=${secondary2_active}" | tee -a "$run_dir/orchestrator.log"
  wait_secondary_ready "secondary1 pre-restart" "$secondary1_active" "$timeout"
  wait_secondary_ready "secondary2 pre-restart" "$secondary2_active" "$timeout"
  assert_kv_present "secondary1 pre-restart" "$secondary1_active" "$DR_PRIMARY_TOKEN" "$marker_key"
  assert_kv_present "secondary2 pre-restart" "$secondary2_active" "$DR_PRIMARY_TOKEN" "$marker_key"

  dr_status_json "$secondary1_active" "$DR_PRIMARY_TOKEN" >"$run_dir/before-secondary1-status.json"
  before_last="$(dr_last_applied_index_from_file "$run_dir/before-secondary1-status.json")"
  before_reconcile="$(dr_reconcile_count_from_file "$run_dir/before-secondary1-status.json")"
  before_scan_failures="$(dr_uint_field_from_file "$run_dir/before-secondary1-status.json" "scan_failures_total")"
  before_local_kid_fallback="$(dr_uint_field_from_file "$run_dir/before-secondary1-status.json" "local_kid_index_fallback_scans_total")"
  before_full_bucket="$(dr_uint_field_from_file "$run_dir/before-secondary1-status.json" "flat_accumulator_indexed_repair_full_bucket_fallback_total")"
  before_proof_mismatch="$(dr_uint_field_from_file "$run_dir/before-secondary1-status.json" "flat_accumulator_indexed_repair_proof_mismatches_total")"
  echo "last_applied_before_secondary1=${before_last}" | tee -a "$run_dir/orchestrator.log"
  echo "reconcile_count_before_secondary1=${before_reconcile}" | tee -a "$run_dir/orchestrator.log"
  echo "scan_failures_before_secondary1=${before_scan_failures}" | tee -a "$run_dir/orchestrator.log"
  echo "local_kid_index_fallback_scans_before_secondary1=${before_local_kid_fallback}" | tee -a "$run_dir/orchestrator.log"
  echo "full_bucket_fallback_before_secondary1=${before_full_bucket}" | tee -a "$run_dir/orchestrator.log"
  echo "indexed_proof_mismatch_before_secondary1=${before_proof_mismatch}" | tee -a "$run_dir/orchestrator.log"

  restart_since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "stopping_secondary1_at=${restart_since}" | tee -a "$run_dir/orchestrator.log"
  compose stop "${services[@]}" >"$run_dir/secondary1-stop.out" 2>"$run_dir/secondary1-stop.err"
  if (( stop_seconds > 0 )); then
    sleep "$stop_seconds"
  fi
  echo "starting_secondary1_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$run_dir/orchestrator.log"
  compose start "${services[@]}" >"$run_dir/secondary1-start.out" 2>"$run_dir/secondary1-start.err"

  for i in "${!SECONDARY1_NODE_ADDRS[@]}"; do
    unseal_secondary1_dr "secondary1-$((i + 1))" "${SECONDARY1_NODE_ADDRS[$i]}"
  done
  wait_raft_peers "secondary1 restarted" "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" "$EXPECTED_RAFT_PEERS" "$timeout"
  secondary1_active="$(wait_cluster_active_addr "secondary1 after cold restart" 120 "${SECONDARY1_NODE_ADDRS[@]}")"
  secondary2_active="$(wait_cluster_active_addr "secondary2 after secondary1 restart" 120 "${SECONDARY2_NODE_ADDRS[@]}")"
  echo "secondary1_active_after=${secondary1_active}" | tee -a "$run_dir/orchestrator.log"
  echo "secondary2_active_after=${secondary2_active}" | tee -a "$run_dir/orchestrator.log"
  wait_secondary_ready "secondary1 after cold restart" "$secondary1_active" "$timeout"
  wait_secondary_ready "secondary2 after secondary1 restart" "$secondary2_active" "$timeout"

  dr_status_json "$secondary1_active" "$DR_PRIMARY_TOKEN" >"$run_dir/after-secondary1-status.json"
  after_last="$(dr_last_applied_index_from_file "$run_dir/after-secondary1-status.json")"
  after_reconcile="$(dr_reconcile_count_from_file "$run_dir/after-secondary1-status.json")"
  after_cursor="$(dr_uint_field_from_file "$run_dir/after-secondary1-status.json" "flat_accumulator_cursor_index")"
  after_snapshot="$(dr_uint_field_from_file "$run_dir/after-secondary1-status.json" "flat_accumulator_snapshot_index")"
  after_scan_failures="$(dr_uint_field_from_file "$run_dir/after-secondary1-status.json" "scan_failures_total")"
  after_local_kid_fallback="$(dr_uint_field_from_file "$run_dir/after-secondary1-status.json" "local_kid_index_fallback_scans_total")"
  after_full_bucket="$(dr_uint_field_from_file "$run_dir/after-secondary1-status.json" "flat_accumulator_indexed_repair_full_bucket_fallback_total")"
  after_proof_mismatch="$(dr_uint_field_from_file "$run_dir/after-secondary1-status.json" "flat_accumulator_indexed_repair_proof_mismatches_total")"
  echo "last_applied_after_secondary1=${after_last}" | tee -a "$run_dir/orchestrator.log"
  echo "reconcile_count_after_secondary1=${after_reconcile}" | tee -a "$run_dir/orchestrator.log"
  echo "flat_accumulator_cursor_after_secondary1=${after_cursor}" | tee -a "$run_dir/orchestrator.log"
  echo "flat_accumulator_snapshot_after_secondary1=${after_snapshot}" | tee -a "$run_dir/orchestrator.log"
  echo "scan_failures_after_secondary1=${after_scan_failures}" | tee -a "$run_dir/orchestrator.log"
  echo "local_kid_index_fallback_scans_after_secondary1=${after_local_kid_fallback}" | tee -a "$run_dir/orchestrator.log"
  echo "full_bucket_fallback_after_secondary1=${after_full_bucket}" | tee -a "$run_dir/orchestrator.log"
  echo "indexed_proof_mismatch_after_secondary1=${after_proof_mismatch}" | tee -a "$run_dir/orchestrator.log"

  if (( after_last < before_last )); then
    die "secondary1 last_applied_index moved backwards after cold restart: ${before_last} -> ${after_last}"
  fi
  if (( after_cursor < before_last )); then
    die "secondary1 flat accumulator cursor did not cover the pre-restart applied index: cursor=${after_cursor}, before=${before_last}"
  fi
  if (( after_reconcile != 0 )); then
    die "secondary1 ran reconciliation after cold restart; expected stream replay from persisted accumulator cursor: ${after_reconcile}"
  fi
  if (( after_scan_failures != 0 )); then
    die "secondary1 reported scan failures after cold restart: ${after_scan_failures}"
  fi
  if (( after_local_kid_fallback != 0 )); then
    die "secondary1 ran local KID-index fallback scan after cold restart: ${after_local_kid_fallback}"
  fi
  if (( after_full_bucket != 0 )); then
    die "secondary1 ran full-bucket indexed repair fallback after cold restart: ${after_full_bucket}"
  fi
  if (( after_proof_mismatch != 0 )); then
    die "secondary1 recorded indexed repair proof mismatch after cold restart: ${after_proof_mismatch}"
  fi

  compose logs --no-color --since "$restart_since" "${services[@]}" >"$run_dir/secondary1-restart.log" 2>"$run_dir/secondary1-restart.log.err" || true
  if rg -q "loaded DR flat accumulator snapshot|loaded DR flat accumulator cursor without snapshot|discarding stale DR flat accumulator snapshot behind cursor" "$run_dir/secondary1-restart.log"; then
    echo "secondary1 restart accumulator log observed" | tee -a "$run_dir/orchestrator.log"
  else
    echo "secondary1 restart accumulator log not observed; validated via status cursor=${after_cursor} snapshot=${after_snapshot}" | tee -a "$run_dir/orchestrator.log"
  fi
  if rg -q "local scan complete|starting reconciliation" "$run_dir/secondary1-restart.log"; then
    die "secondary1 restart logs show scanned reconciliation during cold restart"
  fi

  assert_kv_present "primary after secondary1 restart" "$primary_active" "$DR_PRIMARY_TOKEN" "$marker_key"
  assert_kv_present "secondary1 after cold restart" "$secondary1_active" "$DR_PRIMARY_TOKEN" "$marker_key"
  assert_kv_present "secondary2 after secondary1 restart" "$secondary2_active" "$DR_PRIMARY_TOKEN" "$marker_key"

  echo "completed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$run_dir/orchestrator.log"
  echo "Accumulator cold-restart smoke passed."
  echo "Run: ${run_dir}"
}

cmd_secondary_outage_smoke() {
  [[ "$TOPOLOGY" == "ha" ]] || die "secondary-outage-smoke requires --topology ha"
  need_bin jq
  need_bin rg

  local duration=180
  local concurrency=36
  local outage_after=30
  local outage_seconds=60
  local progress_interval=10
  local monitor_interval=2
  local max_wait_seconds=300
  local do_reset=true
  local do_build=false
  local expect_reconcile=false
  local tuning_profile=""
  local run_prefix="secondary-outage"

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
      --outage-after)
        outage_after="${2:?missing value for --outage-after}"
        shift 2
        ;;
      --outage-seconds)
        outage_seconds="${2:?missing value for --outage-seconds}"
        shift 2
        ;;
      --progress-interval)
        progress_interval="${2:?missing value for --progress-interval}"
        shift 2
        ;;
      --monitor-interval)
        monitor_interval="${2:?missing value for --monitor-interval}"
        shift 2
        ;;
      --max-wait-seconds)
        max_wait_seconds="${2:?missing value for --max-wait-seconds}"
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
      --expect-reconcile)
        expect_reconcile=true
        shift
        ;;
      --tuning-profile)
        tuning_profile="${2:?missing value for --tuning-profile}"
        shift 2
        ;;
      --run-prefix)
        run_prefix="${2:?missing value for --run-prefix}"
        shift 2
        ;;
      *)
        die "unknown secondary-outage-smoke option: $1"
        ;;
    esac
  done

  [[ "$duration" =~ ^[0-9]+$ ]] || die "--duration must be an integer"
  [[ "$concurrency" =~ ^[0-9]+$ ]] || die "--concurrency must be an integer"
  [[ "$outage_after" =~ ^[0-9]+$ ]] || die "--outage-after must be an integer"
  [[ "$outage_seconds" =~ ^[0-9]+$ ]] || die "--outage-seconds must be an integer"
  [[ "$progress_interval" =~ ^[0-9]+$ ]] || die "--progress-interval must be an integer"
  [[ "$monitor_interval" =~ ^[0-9]+$ ]] || die "--monitor-interval must be an integer"
  [[ "$max_wait_seconds" =~ ^[0-9]+$ ]] || die "--max-wait-seconds must be an integer"
  [[ "$run_prefix" =~ ^[a-zA-Z0-9._-]+$ ]] || die "--run-prefix contains unsupported characters"
  (( duration > 0 )) || die "--duration must be > 0"
  (( concurrency > 0 )) || die "--concurrency must be > 0"
  (( outage_after > 0 && outage_after < duration )) || die "--outage-after must be > 0 and less than --duration"
  (( outage_seconds > 0 )) || die "--outage-seconds must be > 0"
  (( outage_after + outage_seconds + 15 < duration )) || die "--duration must leave at least 15s after secondary restart"
  if [[ "$do_reset" == "false" && "$do_build" == "true" ]]; then
    die "--build cannot be used with --no-reset"
  fi

  if [[ "$do_reset" == "true" ]]; then
    if [[ "$do_build" == "true" ]]; then
      cmd_reset --build
    else
      cmd_reset
    fi
  else
    load_env
    wait_secondary_ready "secondary1 pre-outage" "$DR_SECONDARY1_ADDR"
    wait_secondary_ready "secondary2 pre-outage" "$DR_SECONDARY2_ADDR"
  fi

  ensure_dr_stress

  local run_id run_dir stress_pid stress_rc restart_since primary_active secondary1_active secondary2_active secondary1_addrs_csv secondary2_addrs_csv
  local before_last before_reconcile before_scan_failures before_local_kid_fallback before_full_bucket before_proof_mismatch before_range_too_old
  local after_last after_reconcile after_cursor after_snapshot after_scan_failures after_local_kid_fallback after_full_bucket after_proof_mismatch after_range_too_old
  local services=(secondary1-1 secondary1-2 secondary1-3)

  run_id="${run_prefix}-$(date -u +%Y%m%dT%H%M%SZ)"
  run_dir="${RESULTS_DIR}/${run_id}"
  mkdir -p "$run_dir"

  {
    echo "run_id=${run_id}"
    echo "run_dir=${run_dir}"
    echo "started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "duration=${duration}"
    echo "concurrency=${concurrency}"
    echo "outage_after=${outage_after}"
    echo "outage_seconds=${outage_seconds}"
    echo "expect_reconcile=${expect_reconcile}"
    echo "tuning_profile=${tuning_profile}"
  } | tee "$run_dir/orchestrator.log"

  if [[ -n "$tuning_profile" ]]; then
    apply_tuning_profile_to_all "$tuning_profile" "$run_dir"
    capture_tuning_load_state "$run_dir" "after-${tuning_profile}"
  fi

  primary_active="$(wait_cluster_active_addr "primary pre-outage" 120 "${PRIMARY_NODE_ADDRS[@]}")"
  secondary1_active="$(wait_cluster_active_addr "secondary1 pre-outage" 120 "${SECONDARY1_NODE_ADDRS[@]}")"
  secondary2_active="$(wait_cluster_active_addr "secondary2 pre-outage" 120 "${SECONDARY2_NODE_ADDRS[@]}")"
  secondary1_addrs_csv="$(join_csv "${SECONDARY1_NODE_ADDRS[@]}")"
  secondary2_addrs_csv="$(join_csv "${SECONDARY2_NODE_ADDRS[@]}")"
  echo "primary_active_before=${primary_active}" | tee -a "$run_dir/orchestrator.log"
  echo "secondary1_active_before=${secondary1_active}" | tee -a "$run_dir/orchestrator.log"
  echo "secondary2_active_before=${secondary2_active}" | tee -a "$run_dir/orchestrator.log"
  echo "secondary1_addrs=${secondary1_addrs_csv}" | tee -a "$run_dir/orchestrator.log"
  echo "secondary2_addrs=${secondary2_addrs_csv}" | tee -a "$run_dir/orchestrator.log"
  wait_secondary_ready "secondary1 pre-outage" "$secondary1_active"
  wait_secondary_ready "secondary2 pre-outage" "$secondary2_active"

  dr_status_json "$secondary1_active" "$DR_PRIMARY_TOKEN" >"$run_dir/before-secondary1-status.json"
  dr_status_json "$primary_active" "$DR_PRIMARY_TOKEN" >"$run_dir/before-primary-status.json"
  before_last="$(dr_last_applied_index_from_file "$run_dir/before-secondary1-status.json")"
  before_reconcile="$(dr_reconcile_count_from_file "$run_dir/before-secondary1-status.json")"
  before_scan_failures="$(dr_uint_field_from_file "$run_dir/before-secondary1-status.json" "scan_failures_total")"
  before_local_kid_fallback="$(dr_uint_field_from_file "$run_dir/before-secondary1-status.json" "local_kid_index_fallback_scans_total")"
  before_full_bucket="$(dr_uint_field_from_file "$run_dir/before-secondary1-status.json" "flat_accumulator_indexed_repair_full_bucket_fallback_total")"
  before_proof_mismatch="$(dr_uint_field_from_file "$run_dir/before-secondary1-status.json" "flat_accumulator_indexed_repair_proof_mismatches_total")"
  before_range_too_old="$(dr_uint_field_from_file "$run_dir/before-primary-status.json" "journal_range_too_old_total")"
  echo "last_applied_before_secondary1=${before_last}" | tee -a "$run_dir/orchestrator.log"
  echo "reconcile_count_before_secondary1=${before_reconcile}" | tee -a "$run_dir/orchestrator.log"
  echo "scan_failures_before_secondary1=${before_scan_failures}" | tee -a "$run_dir/orchestrator.log"
  echo "local_kid_index_fallback_scans_before_secondary1=${before_local_kid_fallback}" | tee -a "$run_dir/orchestrator.log"
  echo "full_bucket_fallback_before_secondary1=${before_full_bucket}" | tee -a "$run_dir/orchestrator.log"
  echo "indexed_proof_mismatch_before_secondary1=${before_proof_mismatch}" | tee -a "$run_dir/orchestrator.log"
  echo "journal_range_too_old_before_primary=${before_range_too_old}" | tee -a "$run_dir/orchestrator.log"

  "$DR_STRESS_BIN" run \
    -run-id "$run_id" \
    -output-dir "$RESULTS_DIR" \
    -primary-addr "$primary_active" \
    -primary-token "$DR_PRIMARY_TOKEN" \
    -secondary1-addr "$secondary1_addrs_csv" \
    -secondary1-token "$DR_PRIMARY_TOKEN" \
    -secondary2-addr "$secondary2_addrs_csv" \
    -secondary2-token "$DR_PRIMARY_TOKEN" \
    -ensure-kv \
    -duration "$duration" \
    -concurrency "$concurrency" \
    -put-percent 70 \
    -get-primary-percent 20 \
    -status-s1-percent 0 \
    -status-s2-percent 10 \
    -test-class recovery_disruption \
    -topology-label primary+2-secondary \
    -disruption-profile "secondary1_full_cluster_outage_${outage_seconds}s_${tuning_profile:-default}" \
    -max-wait-seconds "$max_wait_seconds" \
    -progress-interval "$progress_interval" \
    -monitor-interval "$monitor_interval" \
    >"$run_dir/harness.out" 2>"$run_dir/harness.err" &
  stress_pid=$!
  echo "stress_pid=${stress_pid}" | tee -a "$run_dir/orchestrator.log"

  sleep "$outage_after"
  restart_since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "stopping_secondary1_at=${restart_since}" | tee -a "$run_dir/orchestrator.log"
  compose stop "${services[@]}" >"$run_dir/secondary1-stop.out" 2>"$run_dir/secondary1-stop.err"
  sleep "$outage_seconds"
  echo "starting_secondary1_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$run_dir/orchestrator.log"
  compose start "${services[@]}" >"$run_dir/secondary1-start.out" 2>"$run_dir/secondary1-start.err"
  for i in "${!SECONDARY1_NODE_ADDRS[@]}"; do
    unseal_secondary1_dr "secondary1-$((i + 1))" "${SECONDARY1_NODE_ADDRS[$i]}"
  done
  wait_raft_peers "secondary1 after outage" "$DR_SECONDARY1_ADDR" "$DR_PRIMARY_TOKEN" "$EXPECTED_RAFT_PEERS" "$max_wait_seconds"
  secondary1_active="$(wait_cluster_active_addr "secondary1 after outage" 120 "${SECONDARY1_NODE_ADDRS[@]}")"
  echo "secondary1_active_after=${secondary1_active}" | tee -a "$run_dir/orchestrator.log"
  wait_secondary_ready "secondary1 after outage" "$secondary1_active" "$max_wait_seconds"

  echo "waiting_for_stress_pid=${stress_pid}" | tee -a "$run_dir/orchestrator.log"
  set +e
  wait "$stress_pid"
  stress_rc=$?
  set -e
  echo "stress_rc=${stress_rc}" | tee -a "$run_dir/orchestrator.log"
  if [[ "$stress_rc" -ne 0 ]]; then
    die "dr-stress exited non-zero; artifacts preserved in ${run_dir}"
  fi

  wait_secondary_ready "secondary1 final" "$secondary1_active" "$max_wait_seconds"
  wait_secondary_ready "secondary2 final" "$DR_SECONDARY2_ADDR" "$max_wait_seconds"
  dr_status_json "$secondary1_active" "$DR_PRIMARY_TOKEN" >"$run_dir/after-secondary1-status.json"
  primary_active="$(wait_cluster_active_addr "primary final" 120 "${PRIMARY_NODE_ADDRS[@]}")"
  dr_status_json "$primary_active" "$DR_PRIMARY_TOKEN" >"$run_dir/after-primary-status.json"

  after_last="$(dr_last_applied_index_from_file "$run_dir/after-secondary1-status.json")"
  after_reconcile="$(dr_reconcile_count_from_file "$run_dir/after-secondary1-status.json")"
  after_cursor="$(dr_uint_field_from_file "$run_dir/after-secondary1-status.json" "flat_accumulator_cursor_index")"
  after_snapshot="$(dr_uint_field_from_file "$run_dir/after-secondary1-status.json" "flat_accumulator_snapshot_index")"
  after_scan_failures="$(dr_uint_field_from_file "$run_dir/after-secondary1-status.json" "scan_failures_total")"
  after_local_kid_fallback="$(dr_uint_field_from_file "$run_dir/after-secondary1-status.json" "local_kid_index_fallback_scans_total")"
  after_full_bucket="$(dr_uint_field_from_file "$run_dir/after-secondary1-status.json" "flat_accumulator_indexed_repair_full_bucket_fallback_total")"
  after_proof_mismatch="$(dr_uint_field_from_file "$run_dir/after-secondary1-status.json" "flat_accumulator_indexed_repair_proof_mismatches_total")"
  after_range_too_old="$(dr_uint_field_from_file "$run_dir/after-primary-status.json" "journal_range_too_old_total")"
  echo "last_applied_after_secondary1=${after_last}" | tee -a "$run_dir/orchestrator.log"
  echo "reconcile_count_after_secondary1=${after_reconcile}" | tee -a "$run_dir/orchestrator.log"
  echo "flat_accumulator_cursor_after_secondary1=${after_cursor}" | tee -a "$run_dir/orchestrator.log"
  echo "flat_accumulator_snapshot_after_secondary1=${after_snapshot}" | tee -a "$run_dir/orchestrator.log"
  echo "scan_failures_after_secondary1=${after_scan_failures}" | tee -a "$run_dir/orchestrator.log"
  echo "local_kid_index_fallback_scans_after_secondary1=${after_local_kid_fallback}" | tee -a "$run_dir/orchestrator.log"
  echo "full_bucket_fallback_after_secondary1=${after_full_bucket}" | tee -a "$run_dir/orchestrator.log"
  echo "indexed_proof_mismatch_after_secondary1=${after_proof_mismatch}" | tee -a "$run_dir/orchestrator.log"
  echo "journal_range_too_old_after_primary=${after_range_too_old}" | tee -a "$run_dir/orchestrator.log"

  if (( after_last < before_last )); then
    die "secondary1 last_applied_index moved backwards after outage: ${before_last} -> ${after_last}"
  fi
  if (( after_cursor < before_last )); then
    die "secondary1 flat accumulator cursor did not cover the pre-outage applied index: cursor=${after_cursor}, before=${before_last}"
  fi
  if (( after_scan_failures != 0 )); then
    die "secondary1 reported scan failures after outage: ${after_scan_failures}"
  fi
  if [[ "$expect_reconcile" == "true" ]]; then
    if (( after_reconcile == 0 )); then
      die "secondary1 did not run reconciliation after out-of-horizon outage"
    fi
    if (( after_range_too_old <= before_range_too_old )); then
      die "primary did not report journal range too old during out-of-horizon outage: ${before_range_too_old} -> ${after_range_too_old}"
    fi
    if (( after_cursor < after_last )); then
      die "secondary1 flat accumulator cursor did not cover final applied index after reconciliation: cursor=${after_cursor}, applied=${after_last}"
    fi
  else
    if (( after_reconcile != 0 )); then
      die "secondary1 ran reconciliation after within-horizon outage: ${after_reconcile}"
    fi
    if (( after_local_kid_fallback != 0 )); then
      die "secondary1 ran local KID-index fallback scan after outage: ${after_local_kid_fallback}"
    fi
    if (( after_full_bucket != 0 )); then
      die "secondary1 ran full-bucket indexed repair fallback after outage: ${after_full_bucket}"
    fi
    if (( after_proof_mismatch != 0 )); then
      die "secondary1 recorded indexed repair proof mismatch after outage: ${after_proof_mismatch}"
    fi
    if (( after_range_too_old > before_range_too_old )); then
      die "primary reported journal range too old during within-horizon outage: ${before_range_too_old} -> ${after_range_too_old}"
    fi
  fi

  compose logs --no-color --since "$restart_since" "${services[@]}" >"$run_dir/secondary1-outage-restart.log" 2>"$run_dir/secondary1-outage-restart.log.err" || true
  if [[ "$expect_reconcile" == "true" ]]; then
    if rg -q "journal cannot satisfy catch-up|starting reconciliation" "$run_dir/secondary1-outage-restart.log"; then
      echo "secondary1 out-of-horizon reconciliation log observed" | tee -a "$run_dir/orchestrator.log"
    else
      echo "secondary1 out-of-horizon reconciliation log not observed; validated via counters" | tee -a "$run_dir/orchestrator.log"
    fi
  elif rg -q "local scan complete|starting reconciliation" "$run_dir/secondary1-outage-restart.log"; then
    die "secondary1 restart logs show scanned reconciliation during within-horizon outage"
  fi

  verify_one "primary" "$primary_active" "$run_dir" 0
  verify_one "secondary1" "$secondary1_active" "$run_dir" 0
  verify_one "secondary2" "$DR_SECONDARY2_ADDR" "$run_dir" 0

  echo "completed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$run_dir/orchestrator.log"
  if [[ "$expect_reconcile" == "true" ]]; then
    echo "Secondary outage reconcile smoke passed."
  else
    echo "Secondary outage smoke passed."
  fi
  echo "Run: ${run_dir}"
}

cmd_secondary_outage_reconcile_smoke() {
  cmd_secondary_outage_smoke \
    --expect-reconcile \
    --tuning-profile out-of-horizon \
    --run-prefix secondary-outage-reconcile \
    --duration 180 \
    --concurrency 48 \
    --outage-after 20 \
    --outage-seconds 90 \
    --max-wait-seconds 900 \
    "$@"
}

cmd_indexed_repair_smoke() {
  [[ "$TOPOLOGY" == "ha" ]] || die "indexed-repair-smoke requires --topology ha"
  need_bin jq

  local duration=900
  local concurrency=48
  local stepdown_interval=300
  local progress_interval=30
  local monitor_interval=5
  local max_wait_seconds=900
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
      --stepdown-interval)
        stepdown_interval="${2:?missing value for --stepdown-interval}"
        shift 2
        ;;
      --progress-interval)
        progress_interval="${2:?missing value for --progress-interval}"
        shift 2
        ;;
      --monitor-interval)
        monitor_interval="${2:?missing value for --monitor-interval}"
        shift 2
        ;;
      --max-wait-seconds)
        max_wait_seconds="${2:?missing value for --max-wait-seconds}"
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
        die "unknown indexed-repair-smoke option: $1"
        ;;
    esac
  done

  [[ "$duration" =~ ^[0-9]+$ ]] || die "--duration must be an integer"
  [[ "$concurrency" =~ ^[0-9]+$ ]] || die "--concurrency must be an integer"
  [[ "$stepdown_interval" =~ ^[0-9]+$ ]] || die "--stepdown-interval must be an integer"
  [[ "$progress_interval" =~ ^[0-9]+$ ]] || die "--progress-interval must be an integer"
  [[ "$monitor_interval" =~ ^[0-9]+$ ]] || die "--monitor-interval must be an integer"
  [[ "$max_wait_seconds" =~ ^[0-9]+$ ]] || die "--max-wait-seconds must be an integer"
  (( duration > 0 )) || die "--duration must be > 0"
  (( concurrency > 0 )) || die "--concurrency must be > 0"
  (( stepdown_interval > 0 && stepdown_interval < duration )) || die "--stepdown-interval must be > 0 and less than --duration"
  (( progress_interval > 0 )) || die "--progress-interval must be > 0"
  (( monitor_interval > 0 )) || die "--monitor-interval must be > 0"
  (( max_wait_seconds > 0 )) || die "--max-wait-seconds must be > 0"
  if [[ "$do_reset" == "false" && "$do_build" == "true" ]]; then
    die "--build cannot be used with --no-reset"
  fi

  if [[ "$do_reset" == "true" ]]; then
    if [[ "$do_build" == "true" ]]; then
      cmd_reset --build
    else
      cmd_reset
    fi
  else
    load_env
    wait_secondary_ready "secondary1 pre-indexed-repair" "$DR_SECONDARY1_ADDR"
    wait_secondary_ready "secondary2 pre-indexed-repair" "$DR_SECONDARY2_ADDR"
  fi

  ensure_dr_stress

  local run_id run_dir stress_rc
  local primary_active secondary1_active secondary2_active secondary1_addrs_csv secondary2_addrs_csv
  local indexed_before indexed_after indexed_delta indexed_ranges_before indexed_ranges_after indexed_ranges_delta
  local bucket_loads_before bucket_loads_after bucket_loads_delta entries_loaded_before entries_loaded_after entries_loaded_delta
  local scan_failures_before scan_failures_after scan_failures_delta local_kid_fallback_before local_kid_fallback_after local_kid_fallback_delta
  local full_bucket_before full_bucket_after full_bucket_delta proof_mismatch_before proof_mismatch_after proof_mismatch_delta
  local load_failures_before load_failures_after load_failures_delta

  run_id="drmixed-indexed-repair-$(date -u +%Y%m%dT%H%M%SZ)"
  run_dir="${RESULTS_DIR}/${run_id}"
  mkdir -p "$run_dir"

  {
    echo "run_id=${run_id}"
    echo "run_dir=${run_dir}"
    echo "started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "duration=${duration}"
    echo "concurrency=${concurrency}"
    echo "stepdown_interval=${stepdown_interval}"
  } | tee "$run_dir/orchestrator.log"

  primary_active="$(wait_cluster_active_addr "primary pre-indexed-repair" 120 "${PRIMARY_NODE_ADDRS[@]}")"
  secondary1_active="$(wait_cluster_active_addr "secondary1 pre-indexed-repair" 120 "${SECONDARY1_NODE_ADDRS[@]}")"
  secondary2_active="$(wait_cluster_active_addr "secondary2 pre-indexed-repair" 120 "${SECONDARY2_NODE_ADDRS[@]}")"
  secondary1_addrs_csv="$(join_csv "${SECONDARY1_NODE_ADDRS[@]}")"
  secondary2_addrs_csv="$(join_csv "${SECONDARY2_NODE_ADDRS[@]}")"
  echo "primary_active_before=${primary_active}" | tee -a "$run_dir/orchestrator.log"
  echo "secondary1_active_before=${secondary1_active}" | tee -a "$run_dir/orchestrator.log"
  echo "secondary2_active_before=${secondary2_active}" | tee -a "$run_dir/orchestrator.log"
  echo "secondary1_addrs=${secondary1_addrs_csv}" | tee -a "$run_dir/orchestrator.log"
  echo "secondary2_addrs=${secondary2_addrs_csv}" | tee -a "$run_dir/orchestrator.log"

  dr_status_json "$secondary1_active" "$DR_PRIMARY_TOKEN" >"$run_dir/before-secondary1-status.json"
  dr_status_json "$secondary2_active" "$DR_PRIMARY_TOKEN" >"$run_dir/before-secondary2-status.json"
  indexed_before="$(dr_sum_uint_field_from_files "flat_accumulator_indexed_repair_total" "$run_dir/before-secondary1-status.json" "$run_dir/before-secondary2-status.json")"
  indexed_ranges_before="$(dr_sum_uint_field_from_files "flat_accumulator_indexed_repair_ranges_total" "$run_dir/before-secondary1-status.json" "$run_dir/before-secondary2-status.json")"
  bucket_loads_before="$(dr_sum_uint_field_from_files "local_kid_index_bucket_loads_total" "$run_dir/before-secondary1-status.json" "$run_dir/before-secondary2-status.json")"
  entries_loaded_before="$(dr_sum_uint_field_from_files "local_kid_index_entries_loaded_total" "$run_dir/before-secondary1-status.json" "$run_dir/before-secondary2-status.json")"
  scan_failures_before="$(dr_sum_uint_field_from_files "scan_failures_total" "$run_dir/before-secondary1-status.json" "$run_dir/before-secondary2-status.json")"
  local_kid_fallback_before="$(dr_sum_uint_field_from_files "local_kid_index_fallback_scans_total" "$run_dir/before-secondary1-status.json" "$run_dir/before-secondary2-status.json")"
  full_bucket_before="$(dr_sum_uint_field_from_files "flat_accumulator_indexed_repair_full_bucket_fallback_total" "$run_dir/before-secondary1-status.json" "$run_dir/before-secondary2-status.json")"
  proof_mismatch_before="$(dr_sum_uint_field_from_files "flat_accumulator_indexed_repair_proof_mismatches_total" "$run_dir/before-secondary1-status.json" "$run_dir/before-secondary2-status.json")"
  load_failures_before="$(dr_sum_uint_field_from_files "local_kid_index_load_failures_total" "$run_dir/before-secondary1-status.json" "$run_dir/before-secondary2-status.json")"

  set +e
  "$DR_STRESS_BIN" run \
    -run-id "$run_id" \
    -output-dir "$RESULTS_DIR" \
    -primary-addr "$DR_PRIMARY_ADDR" \
    -primary-token "$DR_PRIMARY_TOKEN" \
    -secondary1-addr "$secondary1_addrs_csv" \
    -secondary1-token "$DR_PRIMARY_TOKEN" \
    -secondary2-addr "$secondary2_addrs_csv" \
    -secondary2-token "$DR_PRIMARY_TOKEN" \
    -ensure-kv \
    -duration "$duration" \
    -concurrency "$concurrency" \
    -stepdown-interval "$stepdown_interval" \
    -test-class ha_correctness_stress \
    -topology-label primary+2-secondary-ha \
    -disruption-profile primary_active_stepdown_indexed_repair \
    -max-wait-seconds "$max_wait_seconds" \
    -progress-interval "$progress_interval" \
    -monitor-interval "$monitor_interval" \
    2>&1 | tee "$run_dir/harness.log"
  stress_rc=${PIPESTATUS[0]}
  set -e
  echo "stress_rc=${stress_rc}" | tee -a "$run_dir/orchestrator.log"
  if [[ "$stress_rc" -ne 0 ]]; then
    die "dr-stress exited non-zero; artifacts preserved in ${run_dir}"
  fi

  primary_active="$(wait_cluster_active_addr "primary final" 120 "${PRIMARY_NODE_ADDRS[@]}")"
  secondary1_active="$(wait_cluster_active_addr "secondary1 final" 120 "${SECONDARY1_NODE_ADDRS[@]}")"
  secondary2_active="$(wait_cluster_active_addr "secondary2 final" 120 "${SECONDARY2_NODE_ADDRS[@]}")"
  wait_secondary_ready "secondary1 final" "$secondary1_active" "$max_wait_seconds"
  wait_secondary_ready "secondary2 final" "$secondary2_active" "$max_wait_seconds"
  dr_status_json "$primary_active" "$DR_PRIMARY_TOKEN" >"$run_dir/final-primary-status.json"
  dr_status_json "$secondary1_active" "$DR_PRIMARY_TOKEN" >"$run_dir/final-secondary1-status.json"
  dr_status_json "$secondary2_active" "$DR_PRIMARY_TOKEN" >"$run_dir/final-secondary2-status.json"

  indexed_after="$(dr_sum_uint_field_from_files "flat_accumulator_indexed_repair_total" "$run_dir/final-secondary1-status.json" "$run_dir/final-secondary2-status.json")"
  indexed_ranges_after="$(dr_sum_uint_field_from_files "flat_accumulator_indexed_repair_ranges_total" "$run_dir/final-secondary1-status.json" "$run_dir/final-secondary2-status.json")"
  bucket_loads_after="$(dr_sum_uint_field_from_files "local_kid_index_bucket_loads_total" "$run_dir/final-secondary1-status.json" "$run_dir/final-secondary2-status.json")"
  entries_loaded_after="$(dr_sum_uint_field_from_files "local_kid_index_entries_loaded_total" "$run_dir/final-secondary1-status.json" "$run_dir/final-secondary2-status.json")"
  scan_failures_after="$(dr_sum_uint_field_from_files "scan_failures_total" "$run_dir/final-secondary1-status.json" "$run_dir/final-secondary2-status.json")"
  local_kid_fallback_after="$(dr_sum_uint_field_from_files "local_kid_index_fallback_scans_total" "$run_dir/final-secondary1-status.json" "$run_dir/final-secondary2-status.json")"
  full_bucket_after="$(dr_sum_uint_field_from_files "flat_accumulator_indexed_repair_full_bucket_fallback_total" "$run_dir/final-secondary1-status.json" "$run_dir/final-secondary2-status.json")"
  proof_mismatch_after="$(dr_sum_uint_field_from_files "flat_accumulator_indexed_repair_proof_mismatches_total" "$run_dir/final-secondary1-status.json" "$run_dir/final-secondary2-status.json")"
  load_failures_after="$(dr_sum_uint_field_from_files "local_kid_index_load_failures_total" "$run_dir/final-secondary1-status.json" "$run_dir/final-secondary2-status.json")"
  indexed_delta=$(( indexed_after - indexed_before ))
  indexed_ranges_delta=$(( indexed_ranges_after - indexed_ranges_before ))
  bucket_loads_delta=$(( bucket_loads_after - bucket_loads_before ))
  entries_loaded_delta=$(( entries_loaded_after - entries_loaded_before ))
  scan_failures_delta=$(( scan_failures_after - scan_failures_before ))
  local_kid_fallback_delta=$(( local_kid_fallback_after - local_kid_fallback_before ))
  full_bucket_delta=$(( full_bucket_after - full_bucket_before ))
  proof_mismatch_delta=$(( proof_mismatch_after - proof_mismatch_before ))
  load_failures_delta=$(( load_failures_after - load_failures_before ))

  jq -n \
    --argjson indexed_repair_delta "$indexed_delta" \
    --argjson indexed_repair_ranges_delta "$indexed_ranges_delta" \
    --argjson local_kid_index_bucket_loads_delta "$bucket_loads_delta" \
    --argjson local_kid_index_entries_loaded_delta "$entries_loaded_delta" \
    --argjson scan_failures_delta "$scan_failures_delta" \
    --argjson local_kid_index_fallback_scans_delta "$local_kid_fallback_delta" \
    --argjson full_bucket_fallback_delta "$full_bucket_delta" \
    --argjson proof_mismatch_delta "$proof_mismatch_delta" \
    --argjson local_kid_index_load_failures_delta "$load_failures_delta" \
    '{
      indexed_repair_delta: $indexed_repair_delta,
      indexed_repair_ranges_delta: $indexed_repair_ranges_delta,
      local_kid_index_bucket_loads_delta: $local_kid_index_bucket_loads_delta,
      local_kid_index_entries_loaded_delta: $local_kid_index_entries_loaded_delta,
      scan_failures_delta: $scan_failures_delta,
      local_kid_index_fallback_scans_delta: $local_kid_index_fallback_scans_delta,
      full_bucket_fallback_delta: $full_bucket_fallback_delta,
      proof_mismatch_delta: $proof_mismatch_delta,
      local_kid_index_load_failures_delta: $local_kid_index_load_failures_delta
    }' >"$run_dir/indexed-repair-summary.json"
  jq '.' "$run_dir/indexed-repair-summary.json" | tee -a "$run_dir/orchestrator.log"

  jq -e '
    .workload.events_dropped == 0 and
    .workload.status_fail == 0 and
    .replication.sentinel_secondary1.converged == true and
    .replication.sentinel_secondary2.converged == true
  ' "$run_dir/result.json" >/dev/null || die "stress result did not satisfy indexed-repair smoke invariants"

  jq -e '
    .data.mode == "secondary" and
    .data.secondary_state == "streaming" and
    (.data.lag_entries // 0) == 0 and
    (.data.primary_index // 0) > 0 and
    (.data.last_applied_index // 0) >= (.data.primary_index // 0) and
    (.data.reconcile_phase // "") == "idle" and
    (.data.reconcile_fail_reason_last // "") == ""
  ' "$run_dir/final-secondary1-status.json" >/dev/null || die "secondary1 did not finish cleanly after indexed-repair smoke"
  jq -e '
    .data.mode == "secondary" and
    .data.secondary_state == "streaming" and
    (.data.lag_entries // 0) == 0 and
    (.data.primary_index // 0) > 0 and
    (.data.last_applied_index // 0) >= (.data.primary_index // 0) and
    (.data.reconcile_phase // "") == "idle" and
    (.data.reconcile_fail_reason_last // "") == ""
  ' "$run_dir/final-secondary2-status.json" >/dev/null || die "secondary2 did not finish cleanly after indexed-repair smoke"

  (( indexed_delta > 0 )) || die "indexed repair was not exercised"
  (( indexed_ranges_delta > 0 )) || die "indexed repair did not load any ranges"
  (( bucket_loads_delta > 0 )) || die "local KID-index bucket loads did not occur"
  (( entries_loaded_delta > 0 )) || die "local KID-index entries were not loaded"
  (( scan_failures_delta == 0 )) || die "scan failures increased during indexed-repair smoke"
  (( local_kid_fallback_delta == 0 )) || die "local KID-index fallback scan occurred during indexed-repair smoke"
  (( full_bucket_delta == 0 )) || die "full-bucket indexed repair fallback occurred during indexed-repair smoke"
  (( proof_mismatch_delta == 0 )) || die "indexed repair proof mismatch occurred during indexed-repair smoke"
  (( load_failures_delta == 0 )) || die "local KID-index load failures occurred during indexed-repair smoke"

  verify_one "primary" "$primary_active" "$run_dir" 0
  verify_one "secondary1" "$secondary1_active" "$run_dir" 0
  verify_one "secondary2" "$secondary2_active" "$run_dir" 0

  echo "completed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$run_dir/orchestrator.log"
  echo "Indexed repair HA smoke passed."
  echo "Run: ${run_dir}"
}

cmd_tuning_load_smoke() {
  [[ "$TOPOLOGY" == "ha" ]] || die "tuning-load-smoke requires --topology ha"
  need_bin jq

  local duration=360
  local concurrency=36
  local stepdown_interval=90
  local first_tune_after=60
  local second_tune_after=180
  local progress_interval=10
  local monitor_interval=2
  local max_wait_seconds=600
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
      --stepdown-interval)
        stepdown_interval="${2:?missing value for --stepdown-interval}"
        shift 2
        ;;
      --first-tune-after)
        first_tune_after="${2:?missing value for --first-tune-after}"
        shift 2
        ;;
      --second-tune-after)
        second_tune_after="${2:?missing value for --second-tune-after}"
        shift 2
        ;;
      --progress-interval)
        progress_interval="${2:?missing value for --progress-interval}"
        shift 2
        ;;
      --monitor-interval)
        monitor_interval="${2:?missing value for --monitor-interval}"
        shift 2
        ;;
      --max-wait-seconds)
        max_wait_seconds="${2:?missing value for --max-wait-seconds}"
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
        die "unknown tuning-load-smoke option: $1"
        ;;
    esac
  done

  [[ "$duration" =~ ^[0-9]+$ ]] || die "--duration must be an integer"
  [[ "$concurrency" =~ ^[0-9]+$ ]] || die "--concurrency must be an integer"
  [[ "$stepdown_interval" =~ ^[0-9]+$ ]] || die "--stepdown-interval must be an integer"
  [[ "$first_tune_after" =~ ^[0-9]+$ ]] || die "--first-tune-after must be an integer"
  [[ "$second_tune_after" =~ ^[0-9]+$ ]] || die "--second-tune-after must be an integer"
  (( duration > 0 )) || die "--duration must be > 0"
  (( concurrency > 0 )) || die "--concurrency must be > 0"
  (( first_tune_after > 0 && first_tune_after < duration )) || die "--first-tune-after must be > 0 and less than --duration"
  (( second_tune_after > first_tune_after && second_tune_after < duration )) || die "--second-tune-after must be greater than --first-tune-after and less than --duration"

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

  local run_id run_dir stress_pid stress_rc
  run_id="tuning-ha-load-$(date -u +%Y%m%dT%H%M%SZ)"
  run_dir="${RESULTS_DIR}/${run_id}"
  mkdir -p "$run_dir"

  {
    echo "run_id=${run_id}"
    echo "run_dir=${run_dir}"
    echo "started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "duration=${duration}"
    echo "concurrency=${concurrency}"
    echo "stepdown_interval=${stepdown_interval}"
    echo "first_tune_after=${first_tune_after}"
    echo "second_tune_after=${second_tune_after}"
  } | tee "$run_dir/orchestrator.log"

  capture_tuning_load_state "$run_dir" "before"

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
    -stepdown-interval "$stepdown_interval" \
    -put-percent 55 \
    -get-primary-percent 25 \
    -status-s1-percent 10 \
    -status-s2-percent 10 \
    -max-wait-seconds "$max_wait_seconds" \
    -progress-interval "$progress_interval" \
    -monitor-interval "$monitor_interval" \
    >"$run_dir/harness.out" 2>"$run_dir/harness.err" &
  stress_pid=$!
  echo "stress_pid=${stress_pid}" | tee -a "$run_dir/orchestrator.log"

  sleep "$first_tune_after"
  echo "constrained_tuning_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$run_dir/orchestrator.log"
  apply_tuning_profile_to_all "constrained" "$run_dir"
  capture_tuning_load_state "$run_dir" "after-constrained"

  sleep "$((second_tune_after - first_tune_after))"
  echo "relaxed_tuning_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$run_dir/orchestrator.log"
  apply_tuning_profile_to_all "relaxed" "$run_dir"
  capture_tuning_load_state "$run_dir" "after-relaxed"
  force_ha_handoff_for_tuning_load "$run_dir"
  capture_tuning_load_state "$run_dir" "after-handoff"

  echo "waiting_for_stress_pid=${stress_pid}" | tee -a "$run_dir/orchestrator.log"
  set +e
  wait "$stress_pid"
  stress_rc=$?
  set -e
  echo "stress_rc=${stress_rc}" | tee -a "$run_dir/orchestrator.log"
  if [[ "$stress_rc" -ne 0 ]]; then
    die "dr-stress exited non-zero; artifacts preserved in ${run_dir}"
  fi

  wait_secondary_ready "secondary1 final" "$DR_SECONDARY1_ADDR" 300
  wait_secondary_ready "secondary2 final" "$DR_SECONDARY2_ADDR" 300
  capture_tuning_load_state "$run_dir" "final"

  verify_one "primary" "$DR_PRIMARY_ADDR" "$run_dir" 0
  verify_one "secondary1" "$DR_SECONDARY1_ADDR" "$run_dir" 0
  verify_one "secondary2" "$DR_SECONDARY2_ADDR" "$run_dir" 0

  echo "completed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$run_dir/orchestrator.log"
  echo "Dynamic tuning HA load smoke passed."
  echo "Run: ${run_dir}"
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
    engine-matrix) cmd_engine_matrix "$@" ;;
    engine-lifecycle-matrix) cmd_engine_lifecycle_matrix "$@" ;;
    failover-smoke) cmd_failover_smoke "$@" ;;
    promoted-durability-smoke) cmd_promoted_durability_smoke "$@" ;;
    reseed-secondary-smoke) cmd_reseed_secondary_smoke "$@" ;;
    quiescent-reconnect-smoke) cmd_quiescent_reconnect_smoke "$@" ;;
    accumulator-cold-restart-smoke) cmd_accumulator_cold_restart_smoke "$@" ;;
    secondary-outage-smoke) cmd_secondary_outage_smoke "$@" ;;
    secondary-outage-reconcile-smoke) cmd_secondary_outage_reconcile_smoke "$@" ;;
    indexed-repair-smoke) cmd_indexed_repair_smoke "$@" ;;
    tuning-load-smoke) cmd_tuning_load_smoke "$@" ;;
    failover-load-lifecycle) cmd_failover_load_lifecycle "$@" ;;
    down) cmd_down "$@" ;;
    logs) cmd_logs "$@" ;;
    help|-h|--help) usage ;;
    *) usage; die "unknown command: $cmd" ;;
  esac
}

main "$@"
