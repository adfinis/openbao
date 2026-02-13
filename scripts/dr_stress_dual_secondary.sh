#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Run one DR stress workload and compare replication behavior across two secondaries.

Usage:
  scripts/dr_stress_dual_secondary.sh run [options]
  scripts/dr_stress_dual_secondary.sh analyze [paths...] [--output-dir DIR]
  scripts/dr_stress_dual_secondary.sh help

Run options:
  --primary-addr ADDR           Primary BAO_ADDR (required)
  --primary-token TOKEN         Primary BAO_TOKEN (required)
  --secondary1-addr ADDR        Secondary #1 BAO_ADDR (required)
  --secondary1-token TOKEN      Secondary #1 BAO_TOKEN (required)
  --secondary2-addr ADDR        Secondary #2 BAO_ADDR (required)
  --secondary2-token TOKEN      Secondary #2 BAO_TOKEN (required)
  --primary-cacert PATH         Optional primary BAO_CACERT
  --secondary1-cacert PATH      Optional secondary #1 BAO_CACERT
  --secondary2-cacert PATH      Optional secondary #2 BAO_CACERT
  --primary-skip-verify         Set BAO_SKIP_VERIFY=true for primary
  --secondary1-skip-verify      Set BAO_SKIP_VERIFY=true for secondary #1
  --secondary2-skip-verify      Set BAO_SKIP_VERIFY=true for secondary #2
  --secondary1-name NAME        Label in reports (default: secondary1)
  --secondary2-name NAME        Label in reports (default: secondary2)
  --kv-mount NAME               KV v2 mount (default: kv)
  --key-prefix PREFIX           Key prefix (default: dr-stress-dual)
  --run-id ID                   Run identifier (default: drload-dual-<utcstamp>)
  --write-count N               Number of writes (default: 3000)
  --concurrency N               Parallel workers (default: 32)
  --payload-bytes N             Payload bytes per write (default: 1024)
  --sample-keys CSV             Sample key indexes (default: auto)
  --poll-interval SECONDS       Poll interval for sentinel checks (default: 1)
  --max-wait-seconds SECONDS    Sentinel wait timeout (default: 300)
  --monitor-interval SECONDS    Status timeline interval (default: 2)
  --output-dir DIR              Results root (default: ./dr-stress-results)
  --ensure-kv                   Create KV mount on primary if missing

Analyze:
  Pass one or more result.json files or run directories.
  If omitted, analyze scans --output-dir for */result.json.
EOF
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
    secondary1)
      addr="$SECONDARY1_ADDR"
      token="$SECONDARY1_TOKEN"
      cacert="$SECONDARY1_CACERT"
      skip="$SECONDARY1_SKIP_VERIFY"
      ;;
    secondary2)
      addr="$SECONDARY2_ADDR"
      token="$SECONDARY2_TOKEN"
      cacert="$SECONDARY2_CACERT"
      skip="$SECONDARY2_SKIP_VERIFY"
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

json_status_for_role() {
  local role="$1"
  local out
  if out="$(bao_role "$role" read -format=json sys/replication/dr/status 2>/dev/null)"; then
    jq -c '.data // {}' <<<"$out"
  else
    echo '{}'
  fi
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
  local base_path="$2"
  bao_role primary kv put "$base_path/k${idx}" \
    payload="$PAYLOAD" \
    run_id="$RUN_ID" \
    seq="$idx" >/dev/null
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

monitor_status_loop() {
  local timeline_file="$1"
  local stop_file="$2"
  while [[ ! -f "$stop_file" ]]; do
    local ts p s1 s2
    ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    p="$(json_status_for_role primary)"
    s1="$(json_status_for_role secondary1)"
    s2="$(json_status_for_role secondary2)"
    jq -cn \
      --arg ts "$ts" \
      --argjson primary "$p" \
      --argjson secondary1 "$s1" \
      --argjson secondary2 "$s2" \
      '{ts: $ts, primary: $primary, secondary1: $secondary1, secondary2: $secondary2}' >>"$timeline_file"
    sleep "$MONITOR_INTERVAL"
  done
}

run_mode() {
  require_bin bao
  require_bin jq
  require_bin awk
  require_bin sort

  [[ -n "$PRIMARY_ADDR" ]] || die "--primary-addr is required"
  [[ -n "$PRIMARY_TOKEN" ]] || die "--primary-token is required"
  [[ -n "$SECONDARY1_ADDR" ]] || die "--secondary1-addr is required"
  [[ -n "$SECONDARY1_TOKEN" ]] || die "--secondary1-token is required"
  [[ -n "$SECONDARY2_ADDR" ]] || die "--secondary2-addr is required"
  [[ -n "$SECONDARY2_TOKEN" ]] || die "--secondary2-token is required"

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
  local result_file="$run_dir/result.json"
  local timeline_file="$run_dir/status_timeline.ndjson"
  local stop_file="$run_dir/.monitor_stop"
  local data_path="${KV_MOUNT}/${KEY_PREFIX}/${RUN_ID}"

  if [[ "$ENSURE_KV" == "true" ]]; then
    if ! bao_role primary secrets list -format=json | jq -e --arg p "${KV_MOUNT}/" 'has($p)' >/dev/null; then
      echo "Creating KV mount at ${KV_MOUNT}/ on primary"
      bao_role primary secrets enable -path="$KV_MOUNT" kv-v2 >/dev/null
    fi
  fi

  PAYLOAD="$(head -c "$PAYLOAD_BYTES" </dev/zero | tr '\0' 'x')"

  local before_primary before_s1 before_s2
  before_primary="$(json_status_for_role primary)"
  before_s1="$(json_status_for_role secondary1)"
  before_s2="$(json_status_for_role secondary2)"

  : >"$timeline_file"
  rm -f "$stop_file"
  monitor_status_loop "$timeline_file" "$stop_file" &
  local monitor_pid=$!

  local finished=0
  cleanup_run() {
    if [[ "$finished" -eq 0 ]]; then
      touch "$stop_file" 2>/dev/null || true
      wait "$monitor_pid" 2>/dev/null || true
    fi
  }
  trap cleanup_run EXIT

  local start_ms write_done_ms sentinel_written_ms done_ms
  start_ms="$(now_epoch_ms)"

  local worker
  for worker in $(seq 0 $((CONCURRENCY - 1))); do
    (
      local idx=$((worker + 1))
      while [[ "$idx" -le "$WRITE_COUNT" ]]; do
        write_key "$idx" "$data_path"
        idx=$((idx + CONCURRENCY))
      done
    ) &
  done
  wait

  write_done_ms="$(now_epoch_ms)"
  bao_role primary kv put "$data_path/sentinel" done=true count="$WRITE_COUNT" >/dev/null
  sentinel_written_ms="$(now_epoch_ms)"

  local deadline_ms now_ms
  deadline_ms=$((sentinel_written_ms + (MAX_WAIT_SECONDS * 1000)))

  local s1_seen=false s2_seen=false
  local s1_seen_ms=-1 s2_seen_ms=-1
  while true; do
    if [[ "$s1_seen" == "false" ]]; then
      if bao_role secondary1 kv get -format=json "$data_path/sentinel" >/dev/null 2>&1; then
        s1_seen=true
        s1_seen_ms="$(now_epoch_ms)"
      fi
    fi
    if [[ "$s2_seen" == "false" ]]; then
      if bao_role secondary2 kv get -format=json "$data_path/sentinel" >/dev/null 2>&1; then
        s2_seen=true
        s2_seen_ms="$(now_epoch_ms)"
      fi
    fi

    if [[ "$s1_seen" == "true" && "$s2_seen" == "true" ]]; then
      break
    fi
    now_ms="$(now_epoch_ms)"
    if [[ "$now_ms" -ge "$deadline_ms" ]]; then
      break
    fi
    sleep "$POLL_INTERVAL"
  done
  done_ms="$(now_epoch_ms)"

  local s1_lag_seconds=-1 s2_lag_seconds=-1
  if [[ "$s1_seen" == "true" ]]; then
    s1_lag_seconds=$(( ((s1_seen_ms - sentinel_written_ms) + 999) / 1000 ))
  fi
  if [[ "$s2_seen" == "true" ]]; then
    s2_lag_seconds=$(( ((s2_seen_ms - sentinel_written_ms) + 999) / 1000 ))
  fi

  local sample_s1='[]' sample_s2='[]'
  local key_idx
  while read -r key_idx; do
    [[ -n "$key_idx" ]] || continue
    local ok1=true ok2=true
    if ! bao_role secondary1 kv get -format=json "$data_path/k${key_idx}" >/dev/null 2>&1; then
      ok1=false
    fi
    if ! bao_role secondary2 kv get -format=json "$data_path/k${key_idx}" >/dev/null 2>&1; then
      ok2=false
    fi
    sample_s1="$(jq -cn --argjson arr "$sample_s1" --arg key "$key_idx" --argjson ok "$ok1" '$arr + [{key_index:($key|tonumber), ok:$ok}]')"
    sample_s2="$(jq -cn --argjson arr "$sample_s2" --arg key "$key_idx" --argjson ok "$ok2" '$arr + [{key_index:($key|tonumber), ok:$ok}]')"
  done < <(normalize_sample_keys)

  local after_primary after_s1 after_s2
  after_primary="$(json_status_for_role primary)"
  after_s1="$(json_status_for_role secondary1)"
  after_s2="$(json_status_for_role secondary2)"

  touch "$stop_file"
  wait "$monitor_pid" || true
  finished=1
  trap - EXIT

  local write_duration_ms end_to_end_ms
  write_duration_ms=$((write_done_ms - start_ms))
  end_to_end_ms=$((done_ms - start_ms))
  local write_tps=0
  if [[ "$write_duration_ms" -gt 0 ]]; then
    write_tps="$(awk -v c="$WRITE_COUNT" -v ms="$write_duration_ms" 'BEGIN { printf "%.2f", (c * 1000.0) / ms }')"
  fi

  local s1_result="ok" s2_result="ok"
  [[ "$s1_seen" == "true" ]] || s1_result="timeout_waiting_for_sentinel"
  [[ "$s2_seen" == "true" ]] || s2_result="timeout_waiting_for_sentinel"
  local overall_result="ok"
  if [[ "$s1_result" != "ok" || "$s2_result" != "ok" ]]; then
    overall_result="partial_or_timeout"
  fi

  local lag_delta_json="null"
  if [[ "$s1_lag_seconds" -ge 0 && "$s2_lag_seconds" -ge 0 ]]; then
    lag_delta_json=$((s2_lag_seconds - s1_lag_seconds))
  fi

  local s1_entries_before s1_entries_after s1_reconcile_before s1_reconcile_after s1_fail_before s1_fail_after s1_retry_before s1_retry_after
  local s2_entries_before s2_entries_after s2_reconcile_before s2_reconcile_after s2_fail_before s2_fail_after s2_retry_before s2_retry_after
  s1_entries_before="$(extract_num entries_applied "$before_s1")"
  s1_entries_after="$(extract_num entries_applied "$after_s1")"
  s1_reconcile_before="$(extract_num reconcile_count "$before_s1")"
  s1_reconcile_after="$(extract_num reconcile_count "$after_s1")"
  s1_fail_before="$(extract_num connect_failures "$before_s1")"
  s1_fail_after="$(extract_num connect_failures "$after_s1")"
  s1_retry_before="$(extract_num connect_retries "$before_s1")"
  s1_retry_after="$(extract_num connect_retries "$after_s1")"

  s2_entries_before="$(extract_num entries_applied "$before_s2")"
  s2_entries_after="$(extract_num entries_applied "$after_s2")"
  s2_reconcile_before="$(extract_num reconcile_count "$before_s2")"
  s2_reconcile_after="$(extract_num reconcile_count "$after_s2")"
  s2_fail_before="$(extract_num connect_failures "$before_s2")"
  s2_fail_after="$(extract_num connect_failures "$after_s2")"
  s2_retry_before="$(extract_num connect_retries "$before_s2")"
  s2_retry_after="$(extract_num connect_retries "$after_s2")"

  jq -n \
    --arg run_id "$RUN_ID" \
    --arg created_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg data_path "$data_path" \
    --arg primary_addr "$PRIMARY_ADDR" \
    --arg secondary1_addr "$SECONDARY1_ADDR" \
    --arg secondary2_addr "$SECONDARY2_ADDR" \
    --arg secondary1_name "$SECONDARY1_NAME" \
    --arg secondary2_name "$SECONDARY2_NAME" \
    --argjson write_count "$WRITE_COUNT" \
    --argjson concurrency "$CONCURRENCY" \
    --argjson payload_bytes "$PAYLOAD_BYTES" \
    --argjson write_duration_ms "$write_duration_ms" \
    --arg write_tps "$write_tps" \
    --argjson end_to_end_ms "$end_to_end_ms" \
    --arg overall_result "$overall_result" \
    --arg secondary1_result "$s1_result" \
    --arg secondary2_result "$s2_result" \
    --argjson secondary1_lag_seconds "$s1_lag_seconds" \
    --argjson secondary2_lag_seconds "$s2_lag_seconds" \
    --argjson lag_delta "$lag_delta_json" \
    --argjson before_primary "$before_primary" \
    --argjson after_primary "$after_primary" \
    --argjson before_s1 "$before_s1" \
    --argjson after_s1 "$after_s1" \
    --argjson before_s2 "$before_s2" \
    --argjson after_s2 "$after_s2" \
    --argjson sample_s1 "$sample_s1" \
    --argjson sample_s2 "$sample_s2" \
    --arg timeline_file "$timeline_file" \
    --arg result_file "$result_file" \
    --argjson s1_entries_delta "$(calc_delta "$s1_entries_before" "$s1_entries_after")" \
    --argjson s1_reconcile_delta "$(calc_delta "$s1_reconcile_before" "$s1_reconcile_after")" \
    --argjson s1_fail_delta "$(calc_delta "$s1_fail_before" "$s1_fail_after")" \
    --argjson s1_retry_delta "$(calc_delta "$s1_retry_before" "$s1_retry_after")" \
    --argjson s2_entries_delta "$(calc_delta "$s2_entries_before" "$s2_entries_after")" \
    --argjson s2_reconcile_delta "$(calc_delta "$s2_reconcile_before" "$s2_reconcile_after")" \
    --argjson s2_fail_delta "$(calc_delta "$s2_fail_before" "$s2_fail_after")" \
    --argjson s2_retry_delta "$(calc_delta "$s2_retry_before" "$s2_retry_after")" \
    '{
      run_id: $run_id,
      created_at: $created_at,
      result: $overall_result,
      endpoints: {
        primary_addr: $primary_addr,
        secondary1_addr: $secondary1_addr,
        secondary2_addr: $secondary2_addr
      },
      labels: {
        secondary1_name: $secondary1_name,
        secondary2_name: $secondary2_name
      },
      parameters: {
        write_count: $write_count,
        concurrency: $concurrency,
        payload_bytes: $payload_bytes
      },
      data_path: $data_path,
      workload: {
        write_duration_ms: $write_duration_ms,
        write_tps: ($write_tps|tonumber),
        end_to_end_ms: $end_to_end_ms
      },
      comparison: {
        secondary1_lag_seconds: $secondary1_lag_seconds,
        secondary2_lag_seconds: $secondary2_lag_seconds,
        lag_delta_seconds_secondary2_minus_secondary1: $lag_delta
      },
      secondary1: {
        name: $secondary1_name,
        result: $secondary1_result,
        metrics: {
          entries_applied_delta: $s1_entries_delta,
          reconcile_count_delta: $s1_reconcile_delta,
          connect_failures_delta: $s1_fail_delta,
          connect_retries_delta: $s1_retry_delta
        },
        samples: $sample_s1,
        status_before: $before_s1,
        status_after: $after_s1
      },
      secondary2: {
        name: $secondary2_name,
        result: $secondary2_result,
        metrics: {
          entries_applied_delta: $s2_entries_delta,
          reconcile_count_delta: $s2_reconcile_delta,
          connect_failures_delta: $s2_fail_delta,
          connect_retries_delta: $s2_retry_delta
        },
        samples: $sample_s2,
        status_before: $before_s2,
        status_after: $after_s2
      },
      primary_status: {
        before: $before_primary,
        after: $after_primary
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
  echo
  printf "%-18s %-8s %-10s %-9s %-8s %-8s %-8s %-8s\n" \
    "secondary" "result" "lag_s" "entries+" "recon+" "fail+" "retry+" "samples"
  printf "%-18s %-8s %-10s %-9s %-8s %-8s %-8s %-8s\n" \
    "------------------" "--------" "----------" "---------" "--------" "--------" "--------" "--------"
  printf "%-18s %-8s %-10s %-9s %-8s %-8s %-8s %-8s\n" \
    "$SECONDARY1_NAME" "$s1_result" "$s1_lag_seconds" \
    "$(calc_delta "$s1_entries_before" "$s1_entries_after")" \
    "$(calc_delta "$s1_reconcile_before" "$s1_reconcile_after")" \
    "$(calc_delta "$s1_fail_before" "$s1_fail_after")" \
    "$(calc_delta "$s1_retry_before" "$s1_retry_after")" \
    "$(jq -r '[.[] | select(.ok==true)] | length | tostring + \"/\" + (($ARGS.positional|length)|tostring)' --args $(normalize_sample_keys) <<<"$sample_s1")"
  printf "%-18s %-8s %-10s %-9s %-8s %-8s %-8s %-8s\n" \
    "$SECONDARY2_NAME" "$s2_result" "$s2_lag_seconds" \
    "$(calc_delta "$s2_entries_before" "$s2_entries_after")" \
    "$(calc_delta "$s2_reconcile_before" "$s2_reconcile_after")" \
    "$(calc_delta "$s2_fail_before" "$s2_fail_after")" \
    "$(calc_delta "$s2_retry_before" "$s2_retry_after")" \
    "$(jq -r '[.[] | select(.ok==true)] | length | tostring + \"/\" + (($ARGS.positional|length)|tostring)' --args $(normalize_sample_keys) <<<"$sample_s2")"
  if [[ "$lag_delta_json" != "null" ]]; then
    echo
    echo "lag_delta_seconds_secondary2_minus_secondary1=$lag_delta_json"
  fi
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

analyze_mode() {
  require_bin jq
  require_bin sort
  require_bin awk

  local inputs=("$@")
  local files=()
  if [[ "${#inputs[@]}" -eq 0 ]]; then
    [[ -d "$OUTPUT_DIR" ]] || die "no inputs and output-dir does not exist: $OUTPUT_DIR"
    while IFS= read -r f; do
      files+=("$f")
    done < <(find "$OUTPUT_DIR" -maxdepth 2 -type f -name result.json | sort)
  else
    while IFS= read -r f; do
      [[ -n "$f" ]] && files+=("$f")
    done < <(resolve_results "${inputs[@]}")
  fi

  [[ "${#files[@]}" -gt 0 ]] || die "no result.json files found"

  echo "Analyzing ${#files[@]} result file(s)"
  echo
  printf "%-26s %-8s %-5s %-9s %-10s %-10s %-10s %-10s %-14s\n" \
    "run_id" "writes" "conc" "write_ms" "write_tps" "lag_s1" "lag_s2" "delta" "result"
  printf "%-26s %-8s %-5s %-9s %-10s %-10s %-10s %-10s %-14s\n" \
    "--------------------------" "--------" "-----" "---------" "----------" "----------" "----------" "----------" "--------------"

  local tmp_delta
  tmp_delta="$(mktemp)"
  trap 'rm -f "$tmp_delta"' EXIT

  local f
  for f in "${files[@]}"; do
    jq -r '
      [
        (.run_id // "n/a"),
        (.parameters.write_count // 0),
        (.parameters.concurrency // 0),
        (.workload.write_duration_ms // 0),
        (.workload.write_tps // 0),
        (.comparison.secondary1_lag_seconds // -1),
        (.comparison.secondary2_lag_seconds // -1),
        (.comparison.lag_delta_seconds_secondary2_minus_secondary1 // "null"),
        (.result // "n/a")
      ] | @tsv' "$f" | while IFS=$'\t' read -r run_id writes conc write_ms write_tps lag1 lag2 delta result; do
      printf "%-26s %-8s %-5s %-9s %-10s %-10s %-10s %-10s %-14s\n" \
        "$run_id" "$writes" "$conc" "$write_ms" "$write_tps" "$lag1" "$lag2" "$delta" "$result"
      if [[ "$delta" != "null" ]]; then
        echo "$delta" >>"$tmp_delta"
      fi
    done
  done

  if [[ -s "$tmp_delta" ]]; then
    echo
    local n min max avg
    n="$(wc -l <"$tmp_delta" | awk '{print $1}')"
    min="$(sort -g "$tmp_delta" | head -n1)"
    max="$(sort -g "$tmp_delta" | tail -n1)"
    avg="$(awk '{sum+=$1} END{printf "%.2f", sum/NR}' "$tmp_delta")"
    echo "lag_delta_seconds_secondary2_minus_secondary1: n=$n min=$min avg=$avg max=$max"
  fi
}

MODE="${1:-help}"
if [[ $# -gt 0 ]]; then
  shift
fi

PRIMARY_ADDR="${PRIMARY_ADDR:-}"
PRIMARY_TOKEN="${PRIMARY_TOKEN:-}"
PRIMARY_CACERT="${PRIMARY_CACERT:-}"
PRIMARY_SKIP_VERIFY="false"

SECONDARY1_ADDR="${SECONDARY1_ADDR:-}"
SECONDARY1_TOKEN="${SECONDARY1_TOKEN:-}"
SECONDARY1_CACERT="${SECONDARY1_CACERT:-}"
SECONDARY1_SKIP_VERIFY="false"
SECONDARY1_NAME="secondary1"

SECONDARY2_ADDR="${SECONDARY2_ADDR:-}"
SECONDARY2_TOKEN="${SECONDARY2_TOKEN:-}"
SECONDARY2_CACERT="${SECONDARY2_CACERT:-}"
SECONDARY2_SKIP_VERIFY="false"
SECONDARY2_NAME="secondary2"

KV_MOUNT="kv"
KEY_PREFIX="dr-stress-dual"
RUN_ID="drload-dual-$(date -u +%Y%m%dT%H%M%SZ)"
WRITE_COUNT=3000
CONCURRENCY=32
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
        --secondary1-addr) SECONDARY1_ADDR="$2"; shift 2 ;;
        --secondary1-token) SECONDARY1_TOKEN="$2"; shift 2 ;;
        --secondary2-addr) SECONDARY2_ADDR="$2"; shift 2 ;;
        --secondary2-token) SECONDARY2_TOKEN="$2"; shift 2 ;;
        --primary-cacert) PRIMARY_CACERT="$2"; shift 2 ;;
        --secondary1-cacert) SECONDARY1_CACERT="$2"; shift 2 ;;
        --secondary2-cacert) SECONDARY2_CACERT="$2"; shift 2 ;;
        --primary-skip-verify) PRIMARY_SKIP_VERIFY="true"; shift ;;
        --secondary1-skip-verify) SECONDARY1_SKIP_VERIFY="true"; shift ;;
        --secondary2-skip-verify) SECONDARY2_SKIP_VERIFY="true"; shift ;;
        --secondary1-name) SECONDARY1_NAME="$2"; shift 2 ;;
        --secondary2-name) SECONDARY2_NAME="$2"; shift 2 ;;
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
    args=()
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --output-dir) OUTPUT_DIR="$2"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) args+=("$1"); shift ;;
      esac
    done
    analyze_mode "${args[@]}"
    ;;
  help|-h|--help|"")
    usage
    ;;
  *)
    die "unknown mode: $MODE"
    ;;
esac
