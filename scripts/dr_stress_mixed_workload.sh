#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Run a mixed DR workload (writes + reads, hot/cold skew, variable payloads).

Usage:
  scripts/dr_stress_mixed_workload.sh run [options]
  scripts/dr_stress_mixed_workload.sh analyze [paths...] [--output-dir DIR]
  scripts/dr_stress_mixed_workload.sh help

Run options:
  --primary-addr ADDR                Primary BAO_ADDR (required)
  --primary-token TOKEN              Primary BAO_TOKEN (required)
  --secondary1-addr ADDR             Secondary #1 BAO_ADDR (optional)
  --secondary1-token TOKEN           Secondary #1 BAO_TOKEN (optional)
  --secondary2-addr ADDR             Secondary #2 BAO_ADDR (optional)
  --secondary2-token TOKEN           Secondary #2 BAO_TOKEN (optional)
  --primary-cacert PATH              Optional primary BAO_CACERT
  --secondary1-cacert PATH           Optional secondary #1 BAO_CACERT
  --secondary2-cacert PATH           Optional secondary #2 BAO_CACERT
  --primary-tls-server-name NAME     Optional primary BAO_TLS_SERVER_NAME
  --secondary1-tls-server-name NAME  Optional secondary #1 BAO_TLS_SERVER_NAME
  --secondary2-tls-server-name NAME  Optional secondary #2 BAO_TLS_SERVER_NAME
  --primary-skip-verify              Set BAO_SKIP_VERIFY=true for primary
  --secondary1-skip-verify           Set BAO_SKIP_VERIFY=true for secondary #1
  --secondary2-skip-verify           Set BAO_SKIP_VERIFY=true for secondary #2
  --bao-client-timeout DURATION      BAO_CLIENT_TIMEOUT (default: 20s)

  --kv-mount NAME                    KV v2 mount (default: kv)
  --key-prefix PREFIX                Key prefix (default: dr-mixed)
  --run-id ID                        Run identifier (default: drmixed-<utcstamp>)
  --output-dir DIR                   Results root (default: ./dr-stress-results)
  --ensure-kv                        Create KV mount on primary if missing

  --duration-seconds N               Workload duration (default: 900)
  --concurrency N                    Worker count (default: 24)
  --write-retries N                  Put retries (default: 2)
  --monitor-interval N               Status timeline interval (default: 2)
  --progress-interval N              Console progress interval (default: 2)
  --max-wait-seconds N               Sentinel wait timeout (default: 600)
  --stepdown-interval-seconds N      Primary stepdown interval; 0 disables (default: 0)

  --put-percent N                    Percent PUT ops (default: 55)
  --get-primary-percent N            Percent GETs from primary (default: 25)
  --get-secondary1-percent N         Percent GETs from secondary #1 (default: 10)
  --get-secondary2-percent N         Percent GETs from secondary #2 (default: 10)

  --hot-key-count N                  Number of hot keys (default: 200)
  --cold-key-count N                 Number of cold keys (default: 20000)
  --hot-key-percent N                Chance to target hot key [0-100] (default: 80)

  --payload-small-bytes N            Small payload size (default: 512)
  --payload-large-bytes N            Large payload size (default: 8192)
  --large-payload-percent N          Large payload chance [0-100] (default: 15)

Analyze:
  Pass result.json files or run directories.
  If omitted, analyze scans --output-dir for drmixed-*/result.json.
USAGE
}

die() {
  echo "ERROR: $*" >&2
  exit 1
}

require_bin() {
  command -v "$1" >/dev/null 2>&1 || die "missing required binary: $1"
}

safe_int() {
  local name="$1"
  local value="$2"
  [[ "$value" =~ ^[0-9]+$ ]] || die "$name must be a non-negative integer, got: $value"
}

now_epoch_ms() {
  if date +%s%3N >/dev/null 2>&1; then
    date +%s%3N
    return
  fi
  if command -v python3 >/dev/null 2>&1; then
    python3 - <<'PY'
import time
print(int(time.time() * 1000))
PY
    return
  fi
  echo "$(( $(date +%s) * 1000 ))"
}

format_seconds() {
  local total="$1"
  if [[ "$total" -lt 0 ]]; then
    echo "n/a"
    return
  fi
  local h=$((total / 3600))
  local m=$(((total % 3600) / 60))
  local s=$((total % 60))
  if [[ "$h" -gt 0 ]]; then
    printf "%02d:%02d:%02d" "$h" "$m" "$s"
    return
  fi
  printf "%02d:%02d" "$m" "$s"
}

rand_mod() {
  local mod="$1"
  if [[ "$mod" -le 1 ]]; then
    echo 0
    return
  fi
  echo $(( (((RANDOM << 15) | RANDOM) & 0x7fffffff) % mod ))
}

list_descendant_pids() {
  local root_pid="$1"
  local queue=("$root_pid")
  local descendants=()
  local current
  while [[ "${#queue[@]}" -gt 0 ]]; do
    current="${queue[0]}"
    queue=("${queue[@]:1}")
    local children=()
    if command -v pgrep >/dev/null 2>&1; then
      while IFS= read -r cpid; do
        [[ -n "$cpid" ]] || continue
        children+=("$cpid")
      done < <(pgrep -P "$current" 2>/dev/null || true)
    fi
    if [[ "${#children[@]}" -gt 0 ]]; then
      descendants+=("${children[@]}")
      queue+=("${children[@]}")
    fi
  done
  printf "%s\n" "${descendants[@]}" | awk 'NF' | sort -u
}

kill_pid_set() {
  local signal="$1"
  shift || true
  local pid
  for pid in "$@"; do
    [[ -n "$pid" ]] || continue
    kill "-$signal" "$pid" 2>/dev/null || true
  done
}

terminate_descendants() {
  local root_pid="$1"
  local signal="$2"
  local pids=()
  while IFS= read -r pid; do
    [[ -n "$pid" ]] || continue
    pids+=("$pid")
  done < <(list_descendant_pids "$root_pid")
  if [[ "${#pids[@]}" -gt 0 ]]; then
    kill_pid_set "$signal" "${pids[@]}"
  fi
}

bao_role() {
  local role="$1"
  shift
  local addr token cacert skip tls_server_name
  case "$role" in
    primary)
      addr="$PRIMARY_ADDR"
      token="$PRIMARY_TOKEN"
      cacert="$PRIMARY_CACERT"
      skip="$PRIMARY_SKIP_VERIFY"
      tls_server_name="$PRIMARY_TLS_SERVER_NAME"
      ;;
    secondary1)
      addr="$SECONDARY1_ADDR"
      token="$SECONDARY1_TOKEN"
      cacert="$SECONDARY1_CACERT"
      skip="$SECONDARY1_SKIP_VERIFY"
      tls_server_name="$SECONDARY1_TLS_SERVER_NAME"
      ;;
    secondary2)
      addr="$SECONDARY2_ADDR"
      token="$SECONDARY2_TOKEN"
      cacert="$SECONDARY2_CACERT"
      skip="$SECONDARY2_SKIP_VERIFY"
      tls_server_name="$SECONDARY2_TLS_SERVER_NAME"
      ;;
    *)
      die "unknown role: $role"
      ;;
  esac

  local env_args=()
  env_args+=(BAO_ADDR="$addr")
  env_args+=(BAO_TOKEN="$token")
  if [[ -n "$cacert" ]]; then
    env_args+=(BAO_CACERT="$cacert")
  fi
  if [[ -n "$tls_server_name" ]]; then
    env_args+=(BAO_TLS_SERVER_NAME="$tls_server_name")
  fi
  if [[ "$skip" == "true" ]]; then
    env_args+=(BAO_SKIP_VERIFY=true)
  fi
  if [[ -n "$BAO_CLIENT_TIMEOUT_DURATION" ]]; then
    env_args+=(BAO_CLIENT_TIMEOUT="$BAO_CLIENT_TIMEOUT_DURATION")
  fi
  env "${env_args[@]}" bao "$@"
}

json_status_for_role() {
  local role="$1"
  local out
  if out="$(bao_role "$role" read -format=json sys/replication/dr/status 2>/dev/null)"; then
    jq -c '.data // {}' <<<"$out"
  else
    echo '{}'
  fi
}

choose_key_index() {
  if [[ "$HOT_KEY_COUNT" -le 0 && "$COLD_KEY_COUNT" -le 0 ]]; then
    echo 1
    return
  fi
  if [[ "$HOT_KEY_COUNT" -le 0 ]]; then
    echo $((1 + $(rand_mod "$COLD_KEY_COUNT")))
    return
  fi
  if [[ "$COLD_KEY_COUNT" -le 0 ]]; then
    echo $((1 + $(rand_mod "$HOT_KEY_COUNT")))
    return
  fi

  local hot_roll
  hot_roll="$(rand_mod 100)"
  if [[ "$hot_roll" -lt "$HOT_KEY_PERCENT" ]]; then
    echo $((1 + $(rand_mod "$HOT_KEY_COUNT")))
  else
    echo $((HOT_KEY_COUNT + 1 + $(rand_mod "$COLD_KEY_COUNT")))
  fi
}

get_payload() {
  local large_roll
  large_roll="$(rand_mod 100)"
  if [[ "$large_roll" -lt "$LARGE_PAYLOAD_PERCENT" ]]; then
    printf "%s" "$PAYLOAD_LARGE"
  else
    printf "%s" "$PAYLOAD_SMALL"
  fi
}

write_put() {
  local key_path="$1"
  local payload="$2"
  local seq="$3"
  local attempt=0
  local max_attempts=$((WRITE_RETRIES + 1))
  while [[ "$attempt" -lt "$max_attempts" ]]; do
    if bao_role primary kv put "$key_path" payload="$payload" seq="$seq" run_id="$RUN_ID" >/dev/null 2>&1; then
      return 0
    fi
    attempt=$((attempt + 1))
    if [[ "$attempt" -lt "$max_attempts" ]]; then
      sleep "0.$((attempt + 1))"
    fi
  done
  return 1
}

read_get() {
  local role="$1"
  local key_path="$2"
  local out
  if out="$(bao_role "$role" kv get -format=json "$key_path" 2>&1)"; then
    return 0
  fi
  if grep -qiE 'No value found|Code:[[:space:]]*404' <<<"$out"; then
    return 0
  fi
  # Surface the actual error so sentinel-wait stalls are diagnosable.
  if [[ -n "${SENTINEL_DIAG:-}" ]]; then
    echo "[read_get] $role $key_path FAILED: $out" >&2
  fi
  return 1
}

sum_worker_stats() {
  local progress_dir="$1"
  if ! compgen -G "$progress_dir/worker-*.stats" >/dev/null; then
    echo "0 0 0 0 0"
    return
  fi
  awk '{done+=$1; put_ok+=$2; put_fail+=$3; get_ok+=$4; get_fail+=$5} END {print done+0, put_ok+0, put_fail+0, get_ok+0, get_fail+0}' "$progress_dir"/worker-*.stats
}

latest_timeline_snapshot() {
  local timeline_file="$1"
  if [[ -s "$timeline_file" ]]; then
    tail -n1 "$timeline_file"
  else
    echo '{}'
  fi
}

monitor_status_loop() {
  local timeline_file="$1"
  local stop_file="$2"
  local parent_pid="$3"

  while [[ ! -f "$stop_file" ]]; do
    if ! kill -0 "$parent_pid" 2>/dev/null; then
      break
    fi
    local ts p s1 s2
    ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    p="$(json_status_for_role primary)"
    if [[ "$HAS_SECONDARY1" == "true" ]]; then
      s1="$(json_status_for_role secondary1)"
    else
      s1='{}'
    fi
    if [[ "$HAS_SECONDARY2" == "true" ]]; then
      s2="$(json_status_for_role secondary2)"
    else
      s2='{}'
    fi

    jq -cn --arg ts "$ts" --argjson primary "$p" --argjson secondary1 "$s1" --argjson secondary2 "$s2" \
      '{ts:$ts, primary:$primary, secondary1:$secondary1, secondary2:$secondary2}' >>"$timeline_file"
    sleep "$MONITOR_INTERVAL"
  done
}

maybe_stepdown_loop() {
  local stop_file="$1"
  local parent_pid="$2"
  local interval="$3"
  [[ "$interval" -gt 0 ]] || return 0

  while [[ ! -f "$stop_file" ]]; do
    if ! kill -0 "$parent_pid" 2>/dev/null; then
      break
    fi
    sleep "$interval"
    [[ -f "$stop_file" ]] && break
    bao_role primary operator step-down >/dev/null 2>&1 || true
  done
}

progress_loop() {
  local progress_dir="$1"
  local timeline_file="$2"
  local stop_file="$3"
  local start_ms="$4"

  while [[ ! -f "$stop_file" ]]; do
    local now_ms elapsed_s remaining_s done put_ok put_fail get_ok get_fail ops_rate
    now_ms="$(now_epoch_ms)"
    elapsed_s=$(((now_ms - start_ms) / 1000))
    remaining_s=$((DURATION_SECONDS - elapsed_s))
    if [[ "$remaining_s" -lt 0 ]]; then
      remaining_s=0
    fi

    read -r done put_ok put_fail get_ok get_fail < <(sum_worker_stats "$progress_dir")
    ops_rate="0.00"
    if [[ "$elapsed_s" -gt 0 ]]; then
      ops_rate="$(awk -v d="$done" -v e="$elapsed_s" 'BEGIN { printf "%.2f", d/e }')"
    fi

    local snapshot p_buf p_buf_max p_horizon s1_state s2_state s1_idx s2_idx
    snapshot="$(latest_timeline_snapshot "$timeline_file")"
    p_buf="$(jq -r '.primary.stream_buffer_entries // 0' <<<"$snapshot")"
    p_buf_max="$(jq -r '.primary.stream_buffer_max_entries // 0' <<<"$snapshot")"
    p_horizon="$(jq -r '.primary.stream_buffer_horizon_seconds // 0' <<<"$snapshot")"
    s1_state="$(jq -r '.secondary1.secondary_state // "n/a"' <<<"$snapshot")"
    s2_state="$(jq -r '.secondary2.secondary_state // "n/a"' <<<"$snapshot")"
    s1_idx="$(jq -r '.secondary1.last_applied_index // 0' <<<"$snapshot")"
    s2_idx="$(jq -r '.secondary2.last_applied_index // 0' <<<"$snapshot")"

    printf "[mixed] elapsed=%s remaining=%s ops=%s rate=%s/s put(ok=%s fail=%s) get(ok=%s fail=%s) s1=%s(idx=%s) s2=%s(idx=%s) primary_buf=%s/%s horizon=%ss\n" \
      "$(format_seconds "$elapsed_s")" "$(format_seconds "$remaining_s")" "$done" "$ops_rate" \
      "$put_ok" "$put_fail" "$get_ok" "$get_fail" \
      "$s1_state" "$s1_idx" "$s2_state" "$s2_idx" "$p_buf" "$p_buf_max" "$p_horizon"

    sleep "$PROGRESS_INTERVAL"
  done
}

wait_for_sentinel() {
  local role="$1"
  local path="$2"
  local timeout="$3"
  local start now elapsed attempts=0
  start="$(date +%s)"
  # Enable diagnostic output from read_get during sentinel polling.
  SENTINEL_DIAG=1
  echo "[sentinel] waiting for $path on $role (timeout=${timeout}s)" >&2
  while true; do
    attempts=$((attempts + 1))
    if read_get "$role" "$path"; then
      now="$(date +%s)"
      elapsed=$((now - start))
      echo "[sentinel] $role: found after ${elapsed}s ($attempts attempts)" >&2
      SENTINEL_DIAG=""
      echo "$elapsed"
      return 0
    fi
    now="$(date +%s)"
    elapsed=$((now - start))
    if (( elapsed >= timeout )); then
      echo "[sentinel] $role: TIMEOUT after ${elapsed}s ($attempts attempts)" >&2
      SENTINEL_DIAG=""
      echo -1
      return 1
    fi
    # Log progress every 30 attempts (~30s).
    if (( attempts % 30 == 0 )); then
      echo "[sentinel] $role: still waiting after ${elapsed}s ($attempts attempts)..." >&2
    fi
    sleep 1
  done
}

run_mode() {
  require_bin bao
  require_bin jq
  require_bin awk
  require_bin sort

  [[ -n "$PRIMARY_ADDR" ]] || die "--primary-addr is required"
  [[ -n "$PRIMARY_TOKEN" ]] || die "--primary-token is required"

  safe_int "duration-seconds" "$DURATION_SECONDS"
  safe_int "concurrency" "$CONCURRENCY"
  safe_int "write-retries" "$WRITE_RETRIES"
  safe_int "monitor-interval" "$MONITOR_INTERVAL"
  safe_int "progress-interval" "$PROGRESS_INTERVAL"
  safe_int "max-wait-seconds" "$MAX_WAIT_SECONDS"
  safe_int "stepdown-interval-seconds" "$STEPDOWN_INTERVAL_SECONDS"
  safe_int "put-percent" "$PUT_PERCENT"
  safe_int "get-primary-percent" "$GET_PRIMARY_PERCENT"
  safe_int "get-secondary1-percent" "$GET_SECONDARY1_PERCENT"
  safe_int "get-secondary2-percent" "$GET_SECONDARY2_PERCENT"
  safe_int "hot-key-count" "$HOT_KEY_COUNT"
  safe_int "cold-key-count" "$COLD_KEY_COUNT"
  safe_int "hot-key-percent" "$HOT_KEY_PERCENT"
  safe_int "payload-small-bytes" "$PAYLOAD_SMALL_BYTES"
  safe_int "payload-large-bytes" "$PAYLOAD_LARGE_BYTES"
  safe_int "large-payload-percent" "$LARGE_PAYLOAD_PERCENT"

  [[ "$CONCURRENCY" -gt 0 ]] || die "concurrency must be > 0"
  [[ "$DURATION_SECONDS" -gt 0 ]] || die "duration-seconds must be > 0"

  local mix_total=$((PUT_PERCENT + GET_PRIMARY_PERCENT + GET_SECONDARY1_PERCENT + GET_SECONDARY2_PERCENT))
  [[ "$mix_total" -eq 100 ]] || die "operation percentages must sum to 100"

  HAS_SECONDARY1=false
  HAS_SECONDARY2=false
  if [[ -n "$SECONDARY1_ADDR" || -n "$SECONDARY1_TOKEN" ]]; then
    [[ -n "$SECONDARY1_ADDR" && -n "$SECONDARY1_TOKEN" ]] || die "secondary1 addr/token must be provided together"
    HAS_SECONDARY1=true
  fi
  if [[ -n "$SECONDARY2_ADDR" || -n "$SECONDARY2_TOKEN" ]]; then
    [[ -n "$SECONDARY2_ADDR" && -n "$SECONDARY2_TOKEN" ]] || die "secondary2 addr/token must be provided together"
    HAS_SECONDARY2=true
  fi
  if [[ "$HAS_SECONDARY1" != "true" && "$GET_SECONDARY1_PERCENT" -gt 0 ]]; then
    die "get-secondary1-percent > 0 requires secondary1 endpoint"
  fi
  if [[ "$HAS_SECONDARY2" != "true" && "$GET_SECONDARY2_PERCENT" -gt 0 ]]; then
    die "get-secondary2-percent > 0 requires secondary2 endpoint"
  fi

  mkdir -p "$OUTPUT_DIR"
  local run_dir="$OUTPUT_DIR/$RUN_ID"
  mkdir -p "$run_dir"
  local result_file="$run_dir/result.json"
  local timeline_file="$run_dir/status_timeline.ndjson"
  local stop_file="$run_dir/.stop"
  local progress_stop_file="$run_dir/.progress_stop"
  local worker_stop_file="$run_dir/.workers_stop"
  local progress_dir="$run_dir/progress"
  mkdir -p "$progress_dir"

  if [[ "$ENSURE_KV" == "true" ]]; then
    if ! bao_role primary secrets list -format=json | jq -e --arg p "${KV_MOUNT}/" 'has($p)' >/dev/null; then
      echo "Creating KV mount at ${KV_MOUNT}/ on primary"
      bao_role primary secrets enable -path="$KV_MOUNT" kv-v2 >/dev/null
    fi
  fi

  PAYLOAD_SMALL="$(head -c "$PAYLOAD_SMALL_BYTES" </dev/zero | tr '\0' 's')"
  PAYLOAD_LARGE="$(head -c "$PAYLOAD_LARGE_BYTES" </dev/zero | tr '\0' 'L')"

  local data_prefix="${KV_MOUNT}/${KEY_PREFIX}/${RUN_ID}"
  local before_primary before_s1 before_s2
  before_primary="$(json_status_for_role primary)"
  if [[ "$HAS_SECONDARY1" == "true" ]]; then
    before_s1="$(json_status_for_role secondary1)"
  else
    before_s1='{}'
  fi
  if [[ "$HAS_SECONDARY2" == "true" ]]; then
    before_s2="$(json_status_for_role secondary2)"
  else
    before_s2='{}'
  fi

  : >"$timeline_file"
  rm -f "$stop_file" "$progress_stop_file" "$worker_stop_file"
  rm -f "$progress_dir"/worker-*.stats

  local root_pid="$$"
  monitor_status_loop "$timeline_file" "$stop_file" "$root_pid" &
  local monitor_pid=$!
  progress_loop "$progress_dir" "$timeline_file" "$progress_stop_file" "$(now_epoch_ms)" &
  local progress_pid=$!
  maybe_stepdown_loop "$stop_file" "$root_pid" "$STEPDOWN_INTERVAL_SECONDS" &
  local churn_pid=$!

  local worker_pids=()
  local finished=0
  local cleanup_started=0
  local interrupted=0

  cleanup_run() {
    if [[ "$cleanup_started" -eq 1 ]]; then
      return
    fi
    cleanup_started=1

    if [[ "$finished" -eq 0 ]]; then
      touch "$stop_file" 2>/dev/null || true
      touch "$progress_stop_file" 2>/dev/null || true
      touch "$worker_stop_file" 2>/dev/null || true

      local pid
      for pid in "${worker_pids[@]}"; do
        kill -TERM "$pid" 2>/dev/null || true
        terminate_descendants "$pid" TERM
      done

      kill -TERM "$monitor_pid" 2>/dev/null || true
      kill -TERM "$progress_pid" 2>/dev/null || true
      kill -TERM "$churn_pid" 2>/dev/null || true
      terminate_descendants "$monitor_pid" TERM
      terminate_descendants "$progress_pid" TERM
      terminate_descendants "$churn_pid" TERM
      terminate_descendants "$$" TERM
      sleep 0.25
      terminate_descendants "$$" KILL

      wait "$monitor_pid" 2>/dev/null || true
      wait "$progress_pid" 2>/dev/null || true
      wait "$churn_pid" 2>/dev/null || true
      for pid in "${worker_pids[@]}"; do
        wait "$pid" 2>/dev/null || true
      done
    fi
  }

  on_interrupt() {
    interrupted=1
    echo
    echo "Received interrupt; stopping mixed workload..."
    cleanup_run
    exit 130
  }

  trap on_interrupt INT TERM HUP QUIT PIPE
  trap cleanup_run EXIT

  local start_ms deadline_ms
  start_ms="$(now_epoch_ms)"
  deadline_ms=$((start_ms + DURATION_SECONDS * 1000))

  local worker
  for worker in $(seq 0 $((CONCURRENCY - 1))); do
    (
      trap 'exit 130' INT TERM HUP QUIT
      local stats_file="$progress_dir/worker-${worker}.stats"
      local done=0 put_ok=0 put_fail=0 get_ok=0 get_fail=0 seq=0
      echo "0 0 0 0 0" >"$stats_file"

      while true; do
        if [[ -f "$worker_stop_file" ]] || ! kill -0 "$root_pid" 2>/dev/null; then
          break
        fi
        local now_ms
        now_ms="$(now_epoch_ms)"
        if (( now_ms >= deadline_ms )); then
          break
        fi

        local key_idx key_path op_roll op
        key_idx="$(choose_key_index)"
        key_path="${data_prefix}/k${key_idx}"
        op_roll="$(rand_mod 100)"
        op="put"
        if (( op_roll < PUT_PERCENT )); then
          op="put"
        elif (( op_roll < PUT_PERCENT + GET_PRIMARY_PERCENT )); then
          op="get_primary"
        elif (( op_roll < PUT_PERCENT + GET_PRIMARY_PERCENT + GET_SECONDARY1_PERCENT )); then
          op="get_secondary1"
        else
          op="get_secondary2"
        fi

        case "$op" in
          put)
            seq=$((seq + 1))
            if write_put "$key_path" "$(get_payload)" "$seq"; then
              put_ok=$((put_ok + 1))
            else
              put_fail=$((put_fail + 1))
            fi
            ;;
          get_primary)
            if read_get primary "$key_path"; then
              get_ok=$((get_ok + 1))
            else
              get_fail=$((get_fail + 1))
            fi
            ;;
          get_secondary1)
            if [[ "$HAS_SECONDARY1" == "true" ]]; then
              if read_get secondary1 "$key_path"; then
                get_ok=$((get_ok + 1))
              else
                get_fail=$((get_fail + 1))
              fi
            else
              if read_get primary "$key_path"; then
                get_ok=$((get_ok + 1))
              else
                get_fail=$((get_fail + 1))
              fi
            fi
            ;;
          get_secondary2)
            if [[ "$HAS_SECONDARY2" == "true" ]]; then
              if read_get secondary2 "$key_path"; then
                get_ok=$((get_ok + 1))
              else
                get_fail=$((get_fail + 1))
              fi
            else
              if read_get primary "$key_path"; then
                get_ok=$((get_ok + 1))
              else
                get_fail=$((get_fail + 1))
              fi
            fi
            ;;
        esac

        done=$((done + 1))
        if (( done % 20 == 0 )); then
          echo "$done $put_ok $put_fail $get_ok $get_fail" >"$stats_file"
        fi
      done

      echo "$done $put_ok $put_fail $get_ok $get_fail" >"$stats_file"
    ) &
    worker_pids+=("$!")
  done

  local worker_wait_failed=0
  for worker in "${worker_pids[@]}"; do
    if ! wait "$worker"; then
      worker_wait_failed=1
    fi
  done
  if [[ "$interrupted" -eq 1 ]]; then
    return
  fi

  touch "$progress_stop_file"
  wait "$progress_pid" 2>/dev/null || true
  progress_pid=0

  # Sentinel verification after workload completes.
  local sentinel_path="${data_prefix}/sentinel"
  local sentinel_written_ms sentinel_s1_lag sentinel_s2_lag
  sentinel_s1_lag=-1
  sentinel_s2_lag=-1

  if write_put "$sentinel_path" "sentinel-${RUN_ID}" "999999"; then
    sentinel_written_ms="$(now_epoch_ms)"
    if [[ "$HAS_SECONDARY1" == "true" ]]; then
      sentinel_s1_lag="$(wait_for_sentinel secondary1 "$sentinel_path" "$MAX_WAIT_SECONDS")"
    fi
    if [[ "$HAS_SECONDARY2" == "true" ]]; then
      sentinel_s2_lag="$(wait_for_sentinel secondary2 "$sentinel_path" "$MAX_WAIT_SECONDS")"
    fi
  fi

  local end_ms
  end_ms="$(now_epoch_ms)"

  local after_primary after_s1 after_s2
  after_primary="$(json_status_for_role primary)"
  if [[ "$HAS_SECONDARY1" == "true" ]]; then
    after_s1="$(json_status_for_role secondary1)"
  else
    after_s1='{}'
  fi
  if [[ "$HAS_SECONDARY2" == "true" ]]; then
    after_s2="$(json_status_for_role secondary2)"
  else
    after_s2='{}'
  fi

  touch "$stop_file"
  wait "$monitor_pid" 2>/dev/null || true
  wait "$churn_pid" 2>/dev/null || true
  finished=1
  trap - EXIT

  local done put_ok put_fail get_ok get_fail
  read -r done put_ok put_fail get_ok get_fail < <(sum_worker_stats "$progress_dir")
  local duration_ms=$((end_ms - start_ms))
  local ops_per_sec
  ops_per_sec="0.00"
  if [[ "$duration_ms" -gt 0 ]]; then
    ops_per_sec="$(awk -v d="$done" -v ms="$duration_ms" 'BEGIN { printf "%.2f", (d*1000.0)/ms }')"
  fi

  jq -n \
    --arg run_id "$RUN_ID" \
    --arg created_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg data_prefix "$data_prefix" \
    --argjson duration_seconds "$DURATION_SECONDS" \
    --argjson duration_ms "$duration_ms" \
    --arg ops_per_sec "$ops_per_sec" \
    --argjson concurrency "$CONCURRENCY" \
    --argjson done "$done" \
    --argjson put_ok "$put_ok" \
    --argjson put_fail "$put_fail" \
    --argjson get_ok "$get_ok" \
    --argjson get_fail "$get_fail" \
    --argjson sentinel_s1_lag "$sentinel_s1_lag" \
    --argjson sentinel_s2_lag "$sentinel_s2_lag" \
    --argjson before_primary "$before_primary" \
    --argjson before_s1 "$before_s1" \
    --argjson before_s2 "$before_s2" \
    --argjson after_primary "$after_primary" \
    --argjson after_s1 "$after_s1" \
    --argjson after_s2 "$after_s2" \
    --arg timeline_file "$timeline_file" \
    --arg result_file "$result_file" \
    --argjson worker_wait_failed "$worker_wait_failed" \
    '{
      run_id: $run_id,
      created_at: $created_at,
      data_prefix: $data_prefix,
      parameters: {
        duration_seconds: $duration_seconds,
        concurrency: $concurrency,
        put_percent: $ENV.PUT_PERCENT|tonumber,
        get_primary_percent: $ENV.GET_PRIMARY_PERCENT|tonumber,
        get_secondary1_percent: $ENV.GET_SECONDARY1_PERCENT|tonumber,
        get_secondary2_percent: $ENV.GET_SECONDARY2_PERCENT|tonumber,
        hot_key_count: $ENV.HOT_KEY_COUNT|tonumber,
        cold_key_count: $ENV.COLD_KEY_COUNT|tonumber,
        hot_key_percent: $ENV.HOT_KEY_PERCENT|tonumber,
        payload_small_bytes: $ENV.PAYLOAD_SMALL_BYTES|tonumber,
        payload_large_bytes: $ENV.PAYLOAD_LARGE_BYTES|tonumber,
        large_payload_percent: $ENV.LARGE_PAYLOAD_PERCENT|tonumber,
        write_retries: $ENV.WRITE_RETRIES|tonumber,
        stepdown_interval_seconds: $ENV.STEPDOWN_INTERVAL_SECONDS|tonumber
      },
      workload: {
        duration_ms: $duration_ms,
        operations_total: $done,
        operations_per_second: ($ops_per_sec|tonumber),
        put_ok: $put_ok,
        put_fail: $put_fail,
        get_ok: $get_ok,
        get_fail: $get_fail,
        worker_wait_failed: ($worker_wait_failed == 1)
      },
      replication: {
        sentinel_lag_seconds_secondary1: $sentinel_s1_lag,
        sentinel_lag_seconds_secondary2: $sentinel_s2_lag
      },
      status_before: {
        primary: $before_primary,
        secondary1: $before_s1,
        secondary2: $before_s2
      },
      status_after: {
        primary: $after_primary,
        secondary1: $after_s1,
        secondary2: $after_s2
      },
      artifacts: {
        timeline_file: $timeline_file,
        result_file: $result_file
      }
    }' >"$result_file"

  echo "Run complete:"
  echo "  run_id=$RUN_ID"
  echo "  result_file=$result_file"
  echo "  timeline_file=$timeline_file"
  echo "  ops_total=$done ops_per_sec=$ops_per_sec"
  echo "  put_fail=$put_fail get_fail=$get_fail"
  echo "  sentinel_lag_s: secondary1=$sentinel_s1_lag secondary2=$sentinel_s2_lag"
}

resolve_results() {
  local out=()
  local path
  for path in "$@"; do
    if [[ -f "$path" ]]; then
      out+=("$path")
    elif [[ -d "$path" ]]; then
      if [[ -f "$path/result.json" ]]; then
        out+=("$path/result.json")
      fi
    fi
  done
  if [[ "${#out[@]}" -eq 0 ]]; then
    while IFS= read -r f; do
      out+=("$f")
    done < <(find "$OUTPUT_DIR" -maxdepth 2 -type f -name result.json -path '*/drmixed-*/*' 2>/dev/null | sort)
  fi
  printf "%s\n" "${out[@]}"
}

analyze_mode() {
  require_bin jq
  mapfile -t results < <(resolve_results "$@")
  [[ "${#results[@]}" -gt 0 ]] || die "no result files found"

  printf "%-26s %-8s %-8s %-8s %-10s %-10s %-8s %-8s\n" "run_id" "ops/s" "put_f" "get_f" "s1_lag_s" "s2_lag_s" "s1_state" "s2_state"
  printf "%-26s %-8s %-8s %-8s %-10s %-10s %-8s %-8s\n" "--------------------------" "------" "------" "------" "----------" "----------" "--------" "--------"

  local f
  for f in "${results[@]}"; do
    jq -r '[
      .run_id,
      ((.workload.operations_per_second // 0)|tostring),
      ((.workload.put_fail // 0)|tostring),
      ((.workload.get_fail // 0)|tostring),
      ((.replication.sentinel_lag_seconds_secondary1 // -1)|tostring),
      ((.replication.sentinel_lag_seconds_secondary2 // -1)|tostring),
      (.status_after.secondary1.secondary_state // "n/a"),
      (.status_after.secondary2.secondary_state // "n/a")
    ] | @tsv' "$f" | awk -F'\t' '{printf "%-26s %-8s %-8s %-8s %-10s %-10s %-8s %-8s\n", $1,$2,$3,$4,$5,$6,$7,$8}'
  done
}

MODE="${1:-help}"
shift || true

PRIMARY_ADDR=""
PRIMARY_TOKEN=""
SECONDARY1_ADDR=""
SECONDARY1_TOKEN=""
SECONDARY2_ADDR=""
SECONDARY2_TOKEN=""
PRIMARY_CACERT=""
SECONDARY1_CACERT=""
SECONDARY2_CACERT=""
PRIMARY_TLS_SERVER_NAME=""
SECONDARY1_TLS_SERVER_NAME=""
SECONDARY2_TLS_SERVER_NAME=""
PRIMARY_SKIP_VERIFY="false"
SECONDARY1_SKIP_VERIFY="false"
SECONDARY2_SKIP_VERIFY="false"

KV_MOUNT="kv"
KEY_PREFIX="dr-mixed"
RUN_ID="drmixed-$(date -u +%Y%m%dT%H%M%SZ)"
OUTPUT_DIR="./dr-stress-results"
ENSURE_KV="false"

DURATION_SECONDS=900
CONCURRENCY=24
WRITE_RETRIES=2
MONITOR_INTERVAL=2
PROGRESS_INTERVAL=2
MAX_WAIT_SECONDS=600
STEPDOWN_INTERVAL_SECONDS=0

PUT_PERCENT=55
GET_PRIMARY_PERCENT=25
GET_SECONDARY1_PERCENT=10
GET_SECONDARY2_PERCENT=10

HOT_KEY_COUNT=200
COLD_KEY_COUNT=20000
HOT_KEY_PERCENT=80

PAYLOAD_SMALL_BYTES=512
PAYLOAD_LARGE_BYTES=8192
LARGE_PAYLOAD_PERCENT=15

BAO_CLIENT_TIMEOUT_DURATION="20s"

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --primary-addr) PRIMARY_ADDR="$2"; shift 2 ;;
    --primary-token) PRIMARY_TOKEN="$2"; shift 2 ;;
    --secondary1-addr) SECONDARY1_ADDR="$2"; shift 2 ;;
    --secondary1-token) SECONDARY1_TOKEN="$2"; shift 2 ;;
    --secondary2-addr) SECONDARY2_ADDR="$2"; shift 2 ;;
    --secondary2-token) SECONDARY2_TOKEN="$2"; shift 2 ;;
    --primary-cacert) PRIMARY_CACERT="$2"; shift 2 ;;
    --secondary1-cacert) SECONDARY1_CACERT="$2"; shift 2 ;;
    --secondary2-cacert) SECONDARY2_CACERT="$2"; shift 2 ;;
    --primary-tls-server-name) PRIMARY_TLS_SERVER_NAME="$2"; shift 2 ;;
    --secondary1-tls-server-name) SECONDARY1_TLS_SERVER_NAME="$2"; shift 2 ;;
    --secondary2-tls-server-name) SECONDARY2_TLS_SERVER_NAME="$2"; shift 2 ;;
    --primary-skip-verify) PRIMARY_SKIP_VERIFY="true"; shift ;;
    --secondary1-skip-verify) SECONDARY1_SKIP_VERIFY="true"; shift ;;
    --secondary2-skip-verify) SECONDARY2_SKIP_VERIFY="true"; shift ;;
    --bao-client-timeout) BAO_CLIENT_TIMEOUT_DURATION="$2"; shift 2 ;;

    --kv-mount) KV_MOUNT="$2"; shift 2 ;;
    --key-prefix) KEY_PREFIX="$2"; shift 2 ;;
    --run-id) RUN_ID="$2"; shift 2 ;;
    --output-dir) OUTPUT_DIR="$2"; shift 2 ;;
    --ensure-kv) ENSURE_KV="true"; shift ;;

    --duration-seconds) DURATION_SECONDS="$2"; shift 2 ;;
    --concurrency) CONCURRENCY="$2"; shift 2 ;;
    --write-retries) WRITE_RETRIES="$2"; shift 2 ;;
    --monitor-interval) MONITOR_INTERVAL="$2"; shift 2 ;;
    --progress-interval) PROGRESS_INTERVAL="$2"; shift 2 ;;
    --max-wait-seconds) MAX_WAIT_SECONDS="$2"; shift 2 ;;
    --stepdown-interval-seconds) STEPDOWN_INTERVAL_SECONDS="$2"; shift 2 ;;

    --put-percent) PUT_PERCENT="$2"; shift 2 ;;
    --get-primary-percent) GET_PRIMARY_PERCENT="$2"; shift 2 ;;
    --get-secondary1-percent) GET_SECONDARY1_PERCENT="$2"; shift 2 ;;
    --get-secondary2-percent) GET_SECONDARY2_PERCENT="$2"; shift 2 ;;

    --hot-key-count) HOT_KEY_COUNT="$2"; shift 2 ;;
    --cold-key-count) COLD_KEY_COUNT="$2"; shift 2 ;;
    --hot-key-percent) HOT_KEY_PERCENT="$2"; shift 2 ;;

    --payload-small-bytes) PAYLOAD_SMALL_BYTES="$2"; shift 2 ;;
    --payload-large-bytes) PAYLOAD_LARGE_BYTES="$2"; shift 2 ;;
    --large-payload-percent) LARGE_PAYLOAD_PERCENT="$2"; shift 2 ;;

    -h|--help|help)
      usage
      exit 0
      ;;
    *)
      if [[ "$MODE" == "analyze" ]]; then
        break
      fi
      die "unknown option: $1"
      ;;
  esac
done

case "$MODE" in
  run)
    run_mode
    ;;
  analyze)
    analyze_mode "$@"
    ;;
  help|-h|--help|"")
    usage
    ;;
  *)
    die "unknown mode: $MODE"
    ;;
esac
