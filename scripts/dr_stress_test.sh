#!/usr/bin/env bash
set -euo pipefail

SCRIPT_NAME="$(basename "$0")"

usage() {
  cat <<'EOF'
DR replication load/stress runner and result analyzer.

Usage:
  scripts/dr_stress_test.sh run [options]
  scripts/dr_stress_test.sh analyze [paths...] [--output-dir DIR]
  scripts/dr_stress_test.sh help

Run options:
  --primary-addr ADDR           Primary BAO_ADDR (required)
  --primary-token TOKEN         Primary BAO_TOKEN (required)
  --secondary-addr ADDR         Secondary BAO_ADDR (required)
  --secondary-token TOKEN       Secondary BAO_TOKEN (required)
  --primary-cacert PATH         Optional primary BAO_CACERT
  --secondary-cacert PATH       Optional secondary BAO_CACERT
  --primary-skip-verify         Set BAO_SKIP_VERIFY=true for primary calls
  --secondary-skip-verify       Set BAO_SKIP_VERIFY=true for secondary calls
  --kv-mount NAME               KV v2 mount path (default: kv)
  --key-prefix PREFIX           Prefix under mount (default: dr-stress)
  --run-id ID                   Run identifier (default: drload-<utcstamp>)
  --write-count N               Number of writes (default: 2000)
  --concurrency N               Parallel workers (default: 24)
  --payload-bytes N             Value payload size in bytes (default: 1024)
  --sample-keys CSV             Keys to verify on secondary (default: auto)
  --poll-interval SECONDS       Sentinel poll interval (default: 1)
  --max-wait-seconds SECONDS    Max sentinel wait (default: 300)
  --monitor-interval SECONDS    DR status timeline interval (default: 2)
  --output-dir DIR              Result root dir (default: ./dr-stress-results)
  --ensure-kv                   Create KV mount on primary if missing

Analyze:
  Provide one or more result.json files or run directories.
  If no paths are provided, analyze scans --output-dir for */result.json.
EOF
}

die() {
  echo "ERROR: $*" >&2
  exit 1
}

require_bin() {
  command -v "$1" >/dev/null 2>&1 || die "missing required binary: $1"
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

safe_int() {
  local name="$1"
  local value="$2"
  [[ "$value" =~ ^[0-9]+$ ]] || die "$name must be a non-negative integer, got: $value"
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

bao_role() {
  local role="$1"
  shift
  local addr token cacert skip
  case "$role" in
    primary)
      addr="$PRIMARY_ADDR"
      token="$PRIMARY_TOKEN"
      cacert="$PRIMARY_CACERT"
      skip="$PRIMARY_SKIP_VERIFY"
      ;;
    secondary)
      addr="$SECONDARY_ADDR"
      token="$SECONDARY_TOKEN"
      cacert="$SECONDARY_CACERT"
      skip="$SECONDARY_SKIP_VERIFY"
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
  if [[ "$skip" == "true" ]]; then
    env_args+=(BAO_SKIP_VERIFY=true)
  fi
  env "${env_args[@]}" bao "$@"
}

extract_num() {
  local field="$1"
  local json="$2"
  jq -r --arg f "$field" '.[$f] // 0' <<<"$json"
}

calc_delta() {
  local before="$1"
  local after="$2"
  echo $((after - before))
}

write_key() {
  local idx="$1"
  local key_path="$2"
  bao_role primary kv put "$key_path/k${idx}" \
    payload="$PAYLOAD" \
    run_id="$RUN_ID" \
    seq="$idx" >/dev/null
}

monitor_status_loop() {
  local timeline_file="$1"
  local stop_file="$2"
  local parent_pid="$3"
  while [[ ! -f "$stop_file" ]]; do
    if ! kill -0 "$parent_pid" 2>/dev/null; then
      break
    fi
    local ts p s
    ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    p="$(json_status_for_role primary)"
    s="$(json_status_for_role secondary)"
    jq -cn \
      --arg ts "$ts" \
      --argjson primary "$p" \
      --argjson secondary "$s" \
      '{ts: $ts, primary: $primary, secondary: $secondary}' >>"$timeline_file"
    sleep "$MONITOR_INTERVAL"
  done
}

normalize_sample_keys() {
  if [[ -n "$SAMPLE_KEYS_CSV" ]]; then
    echo "$SAMPLE_KEYS_CSV" | tr ',' '\n' | awk 'NF' | awk '$1+0 >= 1' | sort -n | uniq
    return
  fi

  local q1=$((WRITE_COUNT / 4))
  local q2=$((WRITE_COUNT / 2))
  local q3=$(((WRITE_COUNT * 3) / 4))
  printf "%s\n" 1 "$q1" "$q2" "$q3" "$WRITE_COUNT" | awk '$1 >= 1' | sort -n | uniq
}

run_mode() {
  require_bin bao
  require_bin jq
  require_bin awk
  require_bin sort

  [[ -n "$PRIMARY_ADDR" ]] || die "--primary-addr is required"
  [[ -n "$PRIMARY_TOKEN" ]] || die "--primary-token is required"
  [[ -n "$SECONDARY_ADDR" ]] || die "--secondary-addr is required"
  [[ -n "$SECONDARY_TOKEN" ]] || die "--secondary-token is required"

  safe_int "write-count" "$WRITE_COUNT"
  safe_int "concurrency" "$CONCURRENCY"
  safe_int "payload-bytes" "$PAYLOAD_BYTES"
  safe_int "poll-interval" "$POLL_INTERVAL"
  safe_int "max-wait-seconds" "$MAX_WAIT_SECONDS"
  safe_int "monitor-interval" "$MONITOR_INTERVAL"
  [[ "$CONCURRENCY" -gt 0 ]] || die "concurrency must be > 0"

  mkdir -p "$OUTPUT_DIR"
  local run_dir="$OUTPUT_DIR/$RUN_ID"
  mkdir -p "$run_dir"

  local data_path="${KV_MOUNT}/${KEY_PREFIX}/${RUN_ID}"
  local result_file="$run_dir/result.json"
  local timeline_file="$run_dir/status_timeline.ndjson"
  local stop_file="$run_dir/.monitor_stop"
  local worker_stop_file="$run_dir/.workers_stop"

  if [[ "$ENSURE_KV" == "true" ]]; then
    if ! bao_role primary secrets list -format=json | jq -e --arg p "${KV_MOUNT}/" 'has($p)' >/dev/null; then
      echo "Creating KV mount at ${KV_MOUNT}/ on primary"
      bao_role primary secrets enable -path="$KV_MOUNT" kv-v2 >/dev/null
    fi
  fi

  PAYLOAD="$(head -c "$PAYLOAD_BYTES" </dev/zero | tr '\0' 'x')"

  echo "run_id=$RUN_ID"
  echo "output_dir=$run_dir"
  echo "data_path=$data_path"
  echo "writes=$WRITE_COUNT concurrency=$CONCURRENCY payload_bytes=$PAYLOAD_BYTES"

  local before_primary before_secondary
  before_primary="$(json_status_for_role primary)"
  before_secondary="$(json_status_for_role secondary)"

  : >"$timeline_file"
  rm -f "$stop_file"
  rm -f "$worker_stop_file"
  local root_pid="$$"
  monitor_status_loop "$timeline_file" "$stop_file" "$root_pid" &
  local monitor_pid=$!
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
      touch "$worker_stop_file" 2>/dev/null || true
      local pid
      for pid in "${worker_pids[@]}"; do
        kill -TERM "$pid" 2>/dev/null || true
        terminate_descendants "$pid" TERM
      done
      if [[ "$monitor_pid" -gt 0 ]]; then
        kill -TERM "$monitor_pid" 2>/dev/null || true
        terminate_descendants "$monitor_pid" TERM
      fi
      terminate_descendants "$$" TERM
      sleep 0.25
      terminate_descendants "$$" KILL
      wait "$monitor_pid" 2>/dev/null || true
      for pid in "${worker_pids[@]}"; do
        wait "$pid" 2>/dev/null || true
      done
    fi
  }
  on_interrupt() {
    interrupted=1
    echo
    echo "Received interrupt; stopping stress run and terminating workers..."
    cleanup_run
    exit 130
  }
  trap on_interrupt INT TERM HUP QUIT PIPE
  trap cleanup_run EXIT

  local start_ms write_done_ms sentinel_written_ms sentinel_seen_ms
  start_ms="$(now_epoch_ms)"

  local worker
  for worker in $(seq 0 $((CONCURRENCY - 1))); do
    (
      trap 'exit 130' INT TERM HUP QUIT
      local idx=$((worker + 1))
      while [[ "$idx" -le "$WRITE_COUNT" ]]; do
        if [[ -f "$worker_stop_file" ]] || ! kill -0 "$root_pid" 2>/dev/null; then
          break
        fi
        write_key "$idx" "$data_path"
        if [[ -f "$worker_stop_file" ]] || ! kill -0 "$root_pid" 2>/dev/null; then
          break
        fi
        idx=$((idx + CONCURRENCY))
      done
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
  if [[ "$worker_wait_failed" -ne 0 ]]; then
    echo "One or more workers exited non-zero; continuing with collected results." >&2
  fi

  write_done_ms="$(now_epoch_ms)"

  bao_role primary kv put "$data_path/sentinel" \
    done=true \
    count="$WRITE_COUNT" \
    write_duration_ms="$((write_done_ms - start_ms))" >/dev/null
  sentinel_written_ms="$(now_epoch_ms)"

  local sentinel_ok=false
  local sentinel_lag_s=-1
  local deadline_ms now_ms
  deadline_ms=$((sentinel_written_ms + (MAX_WAIT_SECONDS * 1000)))
  while true; do
    if bao_role secondary kv get -format=json "$data_path/sentinel" >/dev/null 2>&1; then
      sentinel_ok=true
      break
    fi
    now_ms="$(now_epoch_ms)"
    if [[ "$now_ms" -ge "$deadline_ms" ]]; then
      break
    fi
    sleep "$POLL_INTERVAL"
  done
  sentinel_seen_ms="$(now_epoch_ms)"

  local samples_json='[]'
  local key_idx
  while read -r key_idx; do
    [[ -n "$key_idx" ]] || continue
    local ok=true
    if ! bao_role secondary kv get -format=json "$data_path/k${key_idx}" >/dev/null 2>&1; then
      ok=false
    fi
    samples_json="$(jq -c \
      --argjson arr "$samples_json" \
      --arg key "$key_idx" \
      --argjson ok "$ok" \
      '$arr + [{key_index: ($key|tonumber), ok: $ok}]' <<< '{}')"
  done < <(normalize_sample_keys)

  local after_primary after_secondary
  after_primary="$(json_status_for_role primary)"
  after_secondary="$(json_status_for_role secondary)"

  touch "$stop_file"
  wait "$monitor_pid" || true
  finished=1
  trap - EXIT

  local before_entries after_entries before_reconcile after_reconcile before_fail after_fail before_retry after_retry
  before_entries="$(extract_num entries_applied "$before_secondary")"
  after_entries="$(extract_num entries_applied "$after_secondary")"
  before_reconcile="$(extract_num reconcile_count "$before_secondary")"
  after_reconcile="$(extract_num reconcile_count "$after_secondary")"
  before_fail="$(extract_num connect_failures "$before_secondary")"
  after_fail="$(extract_num connect_failures "$after_secondary")"
  before_retry="$(extract_num connect_retries "$before_secondary")"
  after_retry="$(extract_num connect_retries "$after_secondary")"

  local write_duration_ms sentinel_wait_ms total_ms
  write_duration_ms=$((write_done_ms - start_ms))
  sentinel_wait_ms=$((sentinel_seen_ms - sentinel_written_ms))
  total_ms=$((sentinel_seen_ms - start_ms))
  if [[ "$sentinel_ok" == "true" ]]; then
    sentinel_lag_s=$(( (sentinel_wait_ms + 999) / 1000 ))
  fi

  local write_tps=0
  if [[ "$write_duration_ms" -gt 0 ]]; then
    write_tps="$(awk -v c="$WRITE_COUNT" -v ms="$write_duration_ms" 'BEGIN { printf "%.2f", (c * 1000.0) / ms }')"
  fi

  jq -n \
    --arg run_id "$RUN_ID" \
    --arg created_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg data_path "$data_path" \
    --arg primary_addr "$PRIMARY_ADDR" \
    --arg secondary_addr "$SECONDARY_ADDR" \
    --argjson write_count "$WRITE_COUNT" \
    --argjson concurrency "$CONCURRENCY" \
    --argjson payload_bytes "$PAYLOAD_BYTES" \
    --argjson max_wait_seconds "$MAX_WAIT_SECONDS" \
    --argjson poll_interval "$POLL_INTERVAL" \
    --argjson monitor_interval "$MONITOR_INTERVAL" \
    --argjson ensure_kv "$( [[ "$ENSURE_KV" == "true" ]] && echo true || echo false )" \
    --argjson before_primary "$before_primary" \
    --argjson after_primary "$after_primary" \
    --argjson before_secondary "$before_secondary" \
    --argjson after_secondary "$after_secondary" \
    --argjson samples "$samples_json" \
    --argjson sentinel_ok "$( [[ "$sentinel_ok" == "true" ]] && echo true || echo false )" \
    --argjson sentinel_lag_seconds "$sentinel_lag_s" \
    --argjson write_duration_ms "$write_duration_ms" \
    --argjson sentinel_wait_ms "$sentinel_wait_ms" \
    --argjson total_ms "$total_ms" \
    --arg write_tps "$write_tps" \
    --argjson entries_applied_delta "$(calc_delta "$before_entries" "$after_entries")" \
    --argjson reconcile_count_delta "$(calc_delta "$before_reconcile" "$after_reconcile")" \
    --argjson connect_failures_delta "$(calc_delta "$before_fail" "$after_fail")" \
    --argjson connect_retries_delta "$(calc_delta "$before_retry" "$after_retry")" \
    --arg timeline_file "$timeline_file" \
    --arg result_file "$result_file" \
    '{
      run_id: $run_id,
      created_at: $created_at,
      endpoints: {
        primary_addr: $primary_addr,
        secondary_addr: $secondary_addr
      },
      parameters: {
        write_count: $write_count,
        concurrency: $concurrency,
        payload_bytes: $payload_bytes,
        max_wait_seconds: $max_wait_seconds,
        poll_interval_seconds: $poll_interval,
        monitor_interval_seconds: $monitor_interval,
        ensure_kv: $ensure_kv
      },
      data_path: $data_path,
      result: (if $sentinel_ok then "ok" else "timeout_waiting_for_sentinel" end),
      metrics: {
        write_duration_ms: $write_duration_ms,
        write_tps: ($write_tps | tonumber),
        sentinel_lag_seconds: $sentinel_lag_seconds,
        sentinel_wait_ms: $sentinel_wait_ms,
        end_to_end_ms: $total_ms,
        entries_applied_delta: $entries_applied_delta,
        reconcile_count_delta: $reconcile_count_delta,
        connect_failures_delta: $connect_failures_delta,
        connect_retries_delta: $connect_retries_delta
      },
      samples: $samples,
      status_before: {
        primary: $before_primary,
        secondary: $before_secondary
      },
      status_after: {
        primary: $after_primary,
        secondary: $after_secondary
      },
      artifacts: {
        timeline_file: $timeline_file,
        result_file: $result_file
      }
    }' >"$result_file"

  echo
  echo "Run complete:"
  echo "  result_file=$result_file"
  echo "  timeline_file=$timeline_file"
  jq -r '
    "  status=\(.result)",
    "  write_duration_ms=\(.metrics.write_duration_ms)",
    "  write_tps=\(.metrics.write_tps)",
    "  sentinel_lag_seconds=\(.metrics.sentinel_lag_seconds)",
    "  entries_applied_delta=\(.metrics.entries_applied_delta)",
    "  reconcile_count_delta=\(.metrics.reconcile_count_delta)",
    "  connect_failures_delta=\(.metrics.connect_failures_delta)",
    "  connect_retries_delta=\(.metrics.connect_retries_delta)"
  ' "$result_file"
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
      else
        while IFS= read -r f; do
          out+=("$f")
        done < <(find "$path" -maxdepth 2 -type f -name result.json | sort)
      fi
    fi
  done
  printf "%s\n" "${out[@]}"
}

print_stats_from_values_file() {
  local label="$1"
  local file="$2"
  local count
  count="$(wc -l <"$file" | awk '{print $1}')"
  if [[ "$count" -eq 0 ]]; then
    echo "$label: n=0"
    return
  fi
  local min max avg p50 p95
  min="$(sort -g "$file" | head -n1)"
  max="$(sort -g "$file" | tail -n1)"
  avg="$(awk '{sum+=$1} END{printf "%.2f", sum/NR}' "$file")"
  local idx50=$(( (50 * (count - 1)) / 100 + 1 ))
  local idx95=$(( (95 * (count - 1)) / 100 + 1 ))
  p50="$(sort -g "$file" | sed -n "${idx50}p")"
  p95="$(sort -g "$file" | sed -n "${idx95}p")"
  echo "$label: n=$count min=$min p50=$p50 p95=$p95 avg=$avg max=$max"
}

analyze_mode() {
  require_bin jq
  require_bin awk
  require_bin sort

  local inputs=("$@")
  local files=()

  if [[ "${#inputs[@]}" -eq 0 ]]; then
    if [[ -d "$OUTPUT_DIR" ]]; then
      while IFS= read -r f; do
        files+=("$f")
      done < <(find "$OUTPUT_DIR" -maxdepth 2 -type f -name result.json | sort)
    fi
  else
    while IFS= read -r f; do
      [[ -n "$f" ]] && files+=("$f")
    done < <(resolve_results "${inputs[@]}")
  fi

  [[ "${#files[@]}" -gt 0 ]] || die "no result.json files found"

  echo "Analyzing ${#files[@]} result file(s)"
  echo
  printf "%-26s %-8s %-5s %-9s %-10s %-9s %-8s %-8s %-8s %-12s\n" \
    "run_id" "writes" "conc" "write_ms" "write_tps" "lag_s" "recon+ " "fail+ " "retry+ " "result"
  printf "%-26s %-8s %-5s %-9s %-10s %-9s %-8s %-8s %-8s %-12s\n" \
    "--------------------------" "--------" "-----" "---------" "----------" "---------" "--------" "--------" "--------" "------------"

  local tmp_lag tmp_tps
  tmp_lag="$(mktemp)"
  tmp_tps="$(mktemp)"
  trap 'rm -f "$tmp_lag" "$tmp_tps"' EXIT

  local f
  for f in "${files[@]}"; do
    jq -r '
      [
        (.run_id // "n/a"),
        (.parameters.write_count // 0),
        (.parameters.concurrency // 0),
        (.metrics.write_duration_ms // 0),
        (.metrics.write_tps // 0),
        (.metrics.sentinel_lag_seconds // -1),
        (.metrics.reconcile_count_delta // 0),
        (.metrics.connect_failures_delta // 0),
        (.metrics.connect_retries_delta // 0),
        (.result // "n/a")
      ] | @tsv' "$f" | while IFS=$'\t' read -r run_id writes conc write_ms write_tps lag_s recon_delta fail_delta retry_delta result; do
      printf "%-26s %-8s %-5s %-9s %-10s %-9s %-8s %-8s %-8s %-12s\n" \
        "$run_id" "$writes" "$conc" "$write_ms" "$write_tps" "$lag_s" "$recon_delta" "$fail_delta" "$retry_delta" "$result"
      if [[ "$lag_s" =~ ^-?[0-9]+$ ]] && [[ "$lag_s" -ge 0 ]]; then
        echo "$lag_s" >>"$tmp_lag"
      fi
      echo "$write_tps" >>"$tmp_tps"
    done
  done

  echo
  print_stats_from_values_file "sentinel_lag_seconds" "$tmp_lag"
  print_stats_from_values_file "write_tps" "$tmp_tps"
}

MODE="${1:-help}"
if [[ $# -gt 0 ]]; then
  shift
fi

PRIMARY_ADDR="${PRIMARY_ADDR:-}"
PRIMARY_TOKEN="${PRIMARY_TOKEN:-}"
SECONDARY_ADDR="${SECONDARY_ADDR:-}"
SECONDARY_TOKEN="${SECONDARY_TOKEN:-}"
PRIMARY_CACERT="${PRIMARY_CACERT:-}"
SECONDARY_CACERT="${SECONDARY_CACERT:-}"
PRIMARY_SKIP_VERIFY="false"
SECONDARY_SKIP_VERIFY="false"
KV_MOUNT="kv"
KEY_PREFIX="dr-stress"
RUN_ID="drload-$(date -u +%Y%m%dT%H%M%SZ)"
WRITE_COUNT=2000
CONCURRENCY=24
PAYLOAD_BYTES=1024
SAMPLE_KEYS_CSV=""
POLL_INTERVAL=1
MAX_WAIT_SECONDS=300
MONITOR_INTERVAL=2
OUTPUT_DIR="$(pwd)/dr-stress-results"
ENSURE_KV="false"

case "$MODE" in
  run)
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --primary-addr) PRIMARY_ADDR="$2"; shift 2 ;;
        --primary-token) PRIMARY_TOKEN="$2"; shift 2 ;;
        --secondary-addr) SECONDARY_ADDR="$2"; shift 2 ;;
        --secondary-token) SECONDARY_TOKEN="$2"; shift 2 ;;
        --primary-cacert) PRIMARY_CACERT="$2"; shift 2 ;;
        --secondary-cacert) SECONDARY_CACERT="$2"; shift 2 ;;
        --primary-skip-verify) PRIMARY_SKIP_VERIFY="true"; shift ;;
        --secondary-skip-verify) SECONDARY_SKIP_VERIFY="true"; shift ;;
        --kv-mount) KV_MOUNT="$2"; shift 2 ;;
        --key-prefix) KEY_PREFIX="$2"; shift 2 ;;
        --run-id) RUN_ID="$2"; shift 2 ;;
        --write-count) WRITE_COUNT="$2"; shift 2 ;;
        --concurrency) CONCURRENCY="$2"; shift 2 ;;
        --payload-bytes) PAYLOAD_BYTES="$2"; shift 2 ;;
        --sample-keys) SAMPLE_KEYS_CSV="$2"; shift 2 ;;
        --poll-interval) POLL_INTERVAL="$2"; shift 2 ;;
        --max-wait-seconds) MAX_WAIT_SECONDS="$2"; shift 2 ;;
        --monitor-interval) MONITOR_INTERVAL="$2"; shift 2 ;;
        --output-dir) OUTPUT_DIR="$2"; shift 2 ;;
        --ensure-kv) ENSURE_KV="true"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) die "unknown option for run: $1" ;;
      esac
    done
    run_mode
    ;;
  analyze)
    local_args=()
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --output-dir) OUTPUT_DIR="$2"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) local_args+=("$1"); shift ;;
      esac
    done
    analyze_mode "${local_args[@]}"
    ;;
  help|-h|--help|"")
    usage
    ;;
  *)
    die "unknown mode: $MODE"
    ;;
esac
