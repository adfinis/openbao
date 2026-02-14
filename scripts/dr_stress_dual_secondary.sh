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
  --primary-tls-server-name N   Optional primary BAO_TLS_SERVER_NAME
  --secondary1-tls-server-name N Optional secondary #1 BAO_TLS_SERVER_NAME
  --secondary2-tls-server-name N Optional secondary #2 BAO_TLS_SERVER_NAME
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
  --progress-interval SECONDS   Console progress interval (default: 5)
  --bao-client-timeout DURATION Per-request BAO client timeout (default: 20s)
  --write-retries N             Retries per write before marking failed (default: 2)
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

sum_worker_progress() {
  local progress_dir="$1"
  local suffix="$2"
  if ! compgen -G "$progress_dir/worker-*.$suffix" >/dev/null; then
    echo 0
    return
  fi
  awk '{s += $1} END {print s + 0}' "$progress_dir"/worker-*."$suffix"
}

latest_timeline_snapshot() {
  local timeline_file="$1"
  if [[ -s "$timeline_file" ]]; then
    tail -n1 "$timeline_file"
  else
    echo '{}'
  fi
}

write_key() {
  local idx="$1"
  local base_path="$2"
  local attempt=0
  local max_attempts=$((WRITE_RETRIES + 1))
  while [[ "$attempt" -lt "$max_attempts" ]]; do
    if bao_role primary kv put "$base_path/k${idx}" \
      payload="$PAYLOAD" \
      run_id="$RUN_ID" \
      seq="$idx" >/dev/null 2>&1; then
      return 0
    fi
    attempt=$((attempt + 1))
    if [[ "$attempt" -lt "$max_attempts" ]]; then
      sleep "0.$((attempt + 1))"
    fi
  done
  return 1
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

progress_loop() {
  local progress_dir="$1"
  local write_failures_file="$2"
  local timeline_file="$3"
  local stop_file="$4"
  local total="$5"
  local start_ms="$6"

  while [[ ! -f "$stop_file" ]]; do
    local now_ms elapsed_s processed started inflight failures succeeded pct rate eta_s remaining
    now_ms="$(now_epoch_ms)"
    elapsed_s=$(( (now_ms - start_ms) / 1000 ))
    processed="$(sum_worker_progress "$progress_dir" done)"
    started="$(sum_worker_progress "$progress_dir" started)"
    if [[ "$started" -gt "$total" ]]; then
      started="$total"
    fi
    if [[ "$processed" -gt "$total" ]]; then
      processed="$total"
    fi
    inflight=$((started - processed))
    if [[ "$inflight" -lt 0 ]]; then
      inflight=0
    fi
    failures="$(wc -l <"$write_failures_file" | awk '{print $1}')"
    succeeded=$((processed - failures))
    if [[ "$succeeded" -lt 0 ]]; then
      succeeded=0
    fi
    pct="$(awk -v p="$processed" -v t="$total" 'BEGIN { if (t <= 0) { printf "100.00"; } else { printf "%.2f", (p * 100.0) / t } }')"
    rate="0.00"
    if [[ "$elapsed_s" -gt 0 ]]; then
      rate="$(awk -v p="$processed" -v e="$elapsed_s" 'BEGIN { printf "%.2f", p / e }')"
    fi
    remaining=$((total - processed))
    eta_s=-1
    if [[ "$remaining" -le 0 ]]; then
      eta_s=0
    elif [[ "$processed" -gt 0 && "$elapsed_s" -gt 0 ]]; then
      eta_s="$(awk -v r="$remaining" -v e="$elapsed_s" -v p="$processed" 'BEGIN { printf "%.0f", (r * e) / p }')"
    fi

    local snapshot s1_state s2_state s1_idx s2_idx p_lag p_buf p_buf_max s1_reason s2_reason
    snapshot="$(latest_timeline_snapshot "$timeline_file")"
    s1_state="$(jq -r '.secondary1.secondary_state // "n/a"' <<<"$snapshot")"
    s2_state="$(jq -r '.secondary2.secondary_state // "n/a"' <<<"$snapshot")"
    s1_idx="$(jq -r '.secondary1.last_applied_index // 0' <<<"$snapshot")"
    s2_idx="$(jq -r '.secondary2.last_applied_index // 0' <<<"$snapshot")"
    s1_reason="$(jq -r '.secondary1.reconcile_fail_reason_last // "n/a"' <<<"$snapshot")"
    s2_reason="$(jq -r '.secondary2.reconcile_fail_reason_last // "n/a"' <<<"$snapshot")"
    p_lag="$(jq -r '.primary.stream_lagging_subscribers_total // 0' <<<"$snapshot")"
    p_buf="$(jq -r '.primary.stream_buffer_entries // 0' <<<"$snapshot")"
    p_buf_max="$(jq -r '.primary.stream_buffer_max_entries // 0' <<<"$snapshot")"

    printf "[progress] writes_done=%s/%s (%s%%) started=%s inflight=%s ok=%s fail=%s rate=%s/s elapsed=%s eta=%s s1=%s(idx=%s,reason=%s) s2=%s(idx=%s,reason=%s) primary_buf=%s/%s lagging=%s\n" \
      "$processed" "$total" "$pct" "$started" "$inflight" "$succeeded" "$failures" "$rate" "$(format_seconds "$elapsed_s")" "$(format_seconds "$eta_s")" \
      "$s1_state" "$s1_idx" "$s1_reason" "$s2_state" "$s2_idx" "$s2_reason" "$p_buf" "$p_buf_max" "$p_lag"
    sleep "$PROGRESS_INTERVAL"
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
  safe_int "write-retries" "$WRITE_RETRIES"
  safe_int "poll-interval" "$POLL_INTERVAL"
  safe_int "max-wait-seconds" "$MAX_WAIT_SECONDS"
  safe_int "monitor-interval" "$MONITOR_INTERVAL"
  safe_int "progress-interval" "$PROGRESS_INTERVAL"
  [[ "$CONCURRENCY" -gt 0 ]] || die "concurrency must be > 0"
  [[ "$PROGRESS_INTERVAL" -gt 0 ]] || die "progress-interval must be > 0"

  mkdir -p "$OUTPUT_DIR"
  local run_dir="$OUTPUT_DIR/$RUN_ID"
  mkdir -p "$run_dir"
  local result_file="$run_dir/result.json"
  local timeline_file="$run_dir/status_timeline.ndjson"
  local stop_file="$run_dir/.monitor_stop"
  local progress_stop_file="$run_dir/.progress_stop"
  local write_failures_file="$run_dir/write_failures.txt"
  local progress_dir="$run_dir/progress"
  local data_path="${KV_MOUNT}/${KEY_PREFIX}/${RUN_ID}"
  mkdir -p "$progress_dir"

  echo "Starting DR dual-secondary stress run"
  echo "  run_id=$RUN_ID"
  echo "  output_dir=$run_dir"
  echo "  write_count=$WRITE_COUNT concurrency=$CONCURRENCY payload_bytes=$PAYLOAD_BYTES retries=$WRITE_RETRIES"
  echo "  monitor_interval=${MONITOR_INTERVAL}s progress_interval=${PROGRESS_INTERVAL}s max_wait_seconds=$MAX_WAIT_SECONDS"
  echo "  secondary1=$SECONDARY1_NAME secondary2=$SECONDARY2_NAME"

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
  : >"$write_failures_file"
  rm -f "$progress_dir"/worker-*.done "$progress_dir"/worker-*.started
  rm -f "$stop_file"
  rm -f "$progress_stop_file"
  monitor_status_loop "$timeline_file" "$stop_file" &
  local monitor_pid=$!
  local progress_pid=0
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

      local pid
      for pid in "${worker_pids[@]}"; do
        kill -TERM "$pid" 2>/dev/null || true
        if command -v pkill >/dev/null 2>&1; then
          pkill -TERM -P "$pid" 2>/dev/null || true
        fi
      done

      if [[ "$progress_pid" -gt 0 ]]; then
        kill -TERM "$progress_pid" 2>/dev/null || true
      fi
      if [[ "$monitor_pid" -gt 0 ]]; then
        kill -TERM "$monitor_pid" 2>/dev/null || true
      fi

      if jobs -pr >/dev/null 2>&1; then
        jobs -pr | xargs -r kill -TERM 2>/dev/null || true
      fi
      sleep 0.2
      if jobs -pr >/dev/null 2>&1; then
        jobs -pr | xargs -r kill -KILL 2>/dev/null || true
      fi

      wait "$monitor_pid" 2>/dev/null || true
      if [[ "$progress_pid" -gt 0 ]]; then
        wait "$progress_pid" 2>/dev/null || true
      fi
    fi
  }
  on_interrupt() {
    interrupted=1
    echo
    echo "Received interrupt; stopping stress run and terminating workers..."
    exit 130
  }
  trap on_interrupt INT TERM HUP QUIT PIPE
  trap cleanup_run EXIT

  local start_ms write_done_ms sentinel_written_ms done_ms
  start_ms="$(now_epoch_ms)"
  progress_loop "$progress_dir" "$write_failures_file" "$timeline_file" "$progress_stop_file" "$WRITE_COUNT" "$start_ms" &
  progress_pid=$!

  local worker
  for worker in $(seq 0 $((CONCURRENCY - 1))); do
    (
      local worker_id="$worker"
      local done_file="$progress_dir/worker-${worker_id}.done"
      local started_file="$progress_dir/worker-${worker_id}.started"
      local done_local=0
      local started_local=0
      echo 0 >"$done_file"
      echo 0 >"$started_file"
      local idx=$((worker + 1))
      while [[ "$idx" -le "$WRITE_COUNT" ]]; do
        started_local=$((started_local + 1))
        echo "$started_local" >"$started_file"
        if ! write_key "$idx" "$data_path"; then
          echo "$idx" >>"$write_failures_file"
        fi
        done_local=$((done_local + 1))
        if [[ $((done_local % 25)) -eq 0 ]]; then
          echo "$done_local" >"$done_file"
        fi
        idx=$((idx + CONCURRENCY))
      done
      echo "$done_local" >"$done_file"
    ) &
    worker_pids+=("$!")
  done
  wait || true
  if [[ "$interrupted" -eq 1 ]]; then
    return
  fi

  write_done_ms="$(now_epoch_ms)"
  touch "$progress_stop_file"
  wait "$progress_pid" || true
  progress_pid=0
  echo "[phase] writes completed in $(format_seconds $(( (write_done_ms - start_ms) / 1000 )))"
  local write_failures_count=0
  write_failures_count="$(wc -l <"$write_failures_file" | awk '{print $1}')"
  local sentinel_write_ok="false"
  local sentinel_attempt=0
  local sentinel_max_attempts=$((WRITE_RETRIES + 1))
  while [[ "$sentinel_attempt" -lt "$sentinel_max_attempts" ]]; do
    if bao_role primary kv put "$data_path/sentinel" done=true count="$WRITE_COUNT" write_failures="$write_failures_count" >/dev/null 2>&1; then
      sentinel_write_ok="true"
      break
    fi
    sentinel_attempt=$((sentinel_attempt + 1))
    if [[ "$sentinel_attempt" -lt "$sentinel_max_attempts" ]]; then
      sleep "0.$((sentinel_attempt + 1))"
    fi
  done
  sentinel_written_ms="$(now_epoch_ms)"

  local deadline_ms now_ms
  deadline_ms=$((sentinel_written_ms + (MAX_WAIT_SECONDS * 1000)))

  local s1_seen=false s2_seen=false
  local s1_seen_ms=-1 s2_seen_ms=-1
  local wait_started_ms="$sentinel_written_ms"
  local wait_last_log_ms=0
  if [[ "$sentinel_write_ok" == "true" ]]; then
    echo "[phase] waiting for sentinel replication on both secondaries"
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
      if [[ "$wait_last_log_ms" -eq 0 || $((now_ms - wait_last_log_ms)) -ge $((PROGRESS_INTERVAL * 1000)) ]]; then
        local s1_status s2_status s1_state s2_state s1_idx s2_idx
        s1_status="$(json_status_for_role secondary1)"
        s2_status="$(json_status_for_role secondary2)"
        s1_state="$(jq -r '.secondary_state // "n/a"' <<<"$s1_status")"
        s2_state="$(jq -r '.secondary_state // "n/a"' <<<"$s2_status")"
        s1_idx="$(jq -r '.last_applied_index // 0' <<<"$s1_status")"
        s2_idx="$(jq -r '.last_applied_index // 0' <<<"$s2_status")"
        printf "[wait] elapsed=%s remaining=%s s1_seen=%s s2_seen=%s s1=%s(idx=%s) s2=%s(idx=%s)\n" \
          "$(format_seconds $(( (now_ms - wait_started_ms) / 1000 )))" \
          "$(format_seconds $(( (deadline_ms - now_ms) / 1000 )))" \
          "$s1_seen" "$s2_seen" "$s1_state" "$s1_idx" "$s2_state" "$s2_idx"
        wait_last_log_ms="$now_ms"
      fi
      if [[ "$now_ms" -ge "$deadline_ms" ]]; then
        break
      fi
      sleep "$POLL_INTERVAL"
    done
  fi
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
  if [[ "$sentinel_write_ok" != "true" ]]; then
    s1_result="sentinel_write_failed"
    s2_result="sentinel_write_failed"
  else
    [[ "$s1_seen" == "true" ]] || s1_result="timeout_waiting_for_sentinel"
    [[ "$s2_seen" == "true" ]] || s2_result="timeout_waiting_for_sentinel"
  fi
  local overall_result="ok"
  if [[ "$s1_result" != "ok" || "$s2_result" != "ok" ]]; then
    overall_result="partial_or_timeout"
  fi
  if [[ "$write_failures_count" -gt 0 ]]; then
    overall_result="partial_write_failures"
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
    --arg bao_client_timeout "$BAO_CLIENT_TIMEOUT_DURATION" \
    --argjson write_retries "$WRITE_RETRIES" \
    --argjson write_failures "$write_failures_count" \
    --arg sentinel_write_ok "$sentinel_write_ok" \
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
        payload_bytes: $payload_bytes,
        bao_client_timeout: $bao_client_timeout,
        write_retries: $write_retries
      },
      write_failures: $write_failures,
      sentinel_write_ok: ($sentinel_write_ok == "true"),
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
  echo "write_failures=$write_failures_count sentinel_write_ok=$sentinel_write_ok"
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
PRIMARY_TLS_SERVER_NAME="${PRIMARY_TLS_SERVER_NAME:-}"
PRIMARY_SKIP_VERIFY="false"

SECONDARY1_ADDR="${SECONDARY1_ADDR:-}"
SECONDARY1_TOKEN="${SECONDARY1_TOKEN:-}"
SECONDARY1_CACERT="${SECONDARY1_CACERT:-}"
SECONDARY1_TLS_SERVER_NAME="${SECONDARY1_TLS_SERVER_NAME:-}"
SECONDARY1_SKIP_VERIFY="false"
SECONDARY1_NAME="secondary1"

SECONDARY2_ADDR="${SECONDARY2_ADDR:-}"
SECONDARY2_TOKEN="${SECONDARY2_TOKEN:-}"
SECONDARY2_CACERT="${SECONDARY2_CACERT:-}"
SECONDARY2_TLS_SERVER_NAME="${SECONDARY2_TLS_SERVER_NAME:-}"
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
PROGRESS_INTERVAL=5
BAO_CLIENT_TIMEOUT_DURATION="20s"
WRITE_RETRIES=2
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
        --primary-tls-server-name) PRIMARY_TLS_SERVER_NAME="$2"; shift 2 ;;
        --secondary1-tls-server-name) SECONDARY1_TLS_SERVER_NAME="$2"; shift 2 ;;
        --secondary2-tls-server-name) SECONDARY2_TLS_SERVER_NAME="$2"; shift 2 ;;
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
        --progress-interval) PROGRESS_INTERVAL="$2"; shift 2 ;;
        --bao-client-timeout) BAO_CLIENT_TIMEOUT_DURATION="$2"; shift 2 ;;
        --write-retries) WRITE_RETRIES="$2"; shift 2 ;;
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
