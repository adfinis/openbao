# DR Replication Manual Test Matrix

This matrix defines manual and automated validation for DR replication in this repository.

## Scope

- Unit and integration tests under `/Users/roelc/projects/secretz/openbao`.
- End-to-end validation against a local multi-cluster Docker test environment.
- Failure-path checks: reconnect/reconcile, revoke, failover, and sustained load.

## Environment Assumptions

- OpenBao repo root: `/Users/roelc/projects/secretz/openbao`
- DR environment repo path is exported as `DR_ENV_DIR`.
- Tools available: `docker`, `jq`, `bao`, `rg`.
- DR endpoints and tokens are exported (for example via `.envrc`):
  - `DR_PRIMARY_ADDR`, `DR_PRIMARY_TOKEN`
  - `DR_SECONDARY1_ADDR`, `DR_SECONDARY1_TOKEN`
  - `DR_SECONDARY2_ADDR`, `DR_SECONDARY2_TOKEN`
  - `DR_CA_CERT`

---

## 1. Automated Matrix (`go test`)

| ID | Area | Command | Expected |
|---|---|---|---|
| A1 | DR unit suite | `go test ./vault -run 'TestDR' -count=1` | Pass |
| A2 | DR integration suite | `go test ./vault -run 'TestDRIntegration' -count=1 -v` | Pass |
| A3 | DR race subset | `go test -race ./vault -run 'TestDRIntegration_(StreamReplication|DisconnectAndReconcile|ReadOnlyEnforcement|GapDetection|BufferOverflowDetection)' -count=1` | Pass, no races |
| A4 | DR range reconciliation | `go test ./vault -run 'TestDRIntegration_(RangeManifestDeterminism|RangeIBLTDecodeSuccess|RangeIBLTDecodeAdaptiveSplit|RangeRefinementPrefixFallback|ReconcileFailsOnBudgetExceeded|NoIndexAdvanceOnPartialRangeFailure|MultiRelationshipRangeIsolation|RevokeDuringRangeReconcileAborts)' -count=1` | Pass |
| A5 | DR stream cursor/replay correctness | `go test ./vault -run 'TestDRIntegration_(StreamAppliesSameRaftIndexBatchEntries|PrimaryStreamReplayIncludesLastAppliedIndex)' -count=1` | Pass |
| A6 | DR bootstrap/authz hardening | `go test ./vault -run 'TestDR(RelationshipManager_BootstrapTokenExpires|RelationshipManager_BootstrapTokenAttemptLockout|RelationshipManager_BootstrapTokenSourceIPBinding|RelationshipManager_HeartbeatLastSeenWriteThrottle|Primary_RevokeRelationshipTerminatesOnlyMatchingStreams)' -count=1` | Pass |
| A7 | DR manager persistence rollback | `go test ./vault -run 'TestDRRelationshipManager_(EnablePrimary_SaveConfigFailureRollsBackState|EnableSecondary_SaveConfigFailureRollsBackState|UpdateTuningAppliesSecondaryRuntime)' -count=1` | Pass |
| A8 | Sketch package | `go test ./physical/replication/sketch -count=1` | Pass |
| A9 | Reconciler package | `go test ./physical/replication/reconciler -count=1` | Pass |
| A10 | Raft stream hooks | `go test ./physical/raft -run 'Test.*ChangeStream|Test.*HookChangeStream|Test.*ApplyBatch' -count=1` | Pass |

Recommended compile pre-step:

```bash
go test ./... -run '^$' -count=1
```

---

## 2. Docker E2E Matrix

Set shell environment:

```bash
OPENBAO_REPO_DIR="/Users/roelc/projects/secretz/openbao"
DR_ENV_DIR="/path/to/dr-env"
DR_RESULTS_DIR="$OPENBAO_REPO_DIR/dr-stress-results"
cd "$OPENBAO_REPO_DIR"
```

Build OpenBao image and refresh test clusters:

```bash
make docker-dev
cd "$DR_ENV_DIR"
make fresh-bao
```

Configure DR relationships (from DR env repo):

```bash
cd "$DR_ENV_DIR"
./dr-configuration.sh
```

### E2E Scenarios

| ID | Scenario | Steps | Expected |
|---|---|---|---|
| E1 | Baseline DR bootstrap | Enable primary, register/enable secondaries, wait for `streaming`. | Both secondaries: `mode=secondary`, `secondary_state=streaming`. |
| E2 | Initial sync correctness | Verify `kv/` mount and baseline reads on both secondaries. | Baseline values readable on both. |
| E3 | Stream replication correctness | Write new key on primary, read on both secondaries. | Value appears on both secondaries quickly. |
| E4 | Revoke enforcement | Revoke one relationship while stream active. | Only revoked relationship terminated/denied; other remains healthy. |
| E5 | Re-enable after revoke | Disable revoked secondary, issue new token, re-enable. | Secondary returns to `streaming`; new writes replicate. |
| E6 | Disconnect + reconcile | Disrupt primary connectivity, continue writes, restore connectivity. | Secondary transitions through reconcile and converges. |
| E7 | Secondary read-only gate | Attempt data write on secondary and DR control operation. | Data write denied; allowed control operation accepted. |
| E8 | Failover path | Promote secondary with `sys/replication/dr/secondary/promote`. | DR mode disabled on promoted cluster; writes accepted locally. |

### Command Snippets (address-based, no container-name dependency)

Enable DR primary and seed baseline key:

```bash
BAO_ADDR="$DR_PRIMARY_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" BAO_CACERT="$DR_CA_CERT" bao write -f sys/replication/dr/primary/enable
BAO_ADDR="$DR_PRIMARY_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" BAO_CACERT="$DR_CA_CERT" bao secrets enable -path=kv kv-v2 || true
BAO_ADDR="$DR_PRIMARY_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" BAO_CACERT="$DR_CA_CERT" bao kv put kv/dr-baseline msg=hello ts="$(date +%s)"
```

Enable secondary #1:

```bash
ACT1=$(BAO_ADDR="$DR_PRIMARY_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" BAO_CACERT="$DR_CA_CERT" bao write -f -format=json sys/replication/dr/primary/secondary-token | jq -r '.data.token')
BAO_ADDR="$DR_SECONDARY1_ADDR" BAO_TOKEN="$DR_SECONDARY1_TOKEN" BAO_CACERT="$DR_CA_CERT" bao write sys/replication/dr/secondary/enable token="$ACT1"
```

Enable secondary #2:

```bash
ACT2=$(BAO_ADDR="$DR_PRIMARY_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" BAO_CACERT="$DR_CA_CERT" bao write -f -format=json sys/replication/dr/primary/secondary-token | jq -r '.data.token')
BAO_ADDR="$DR_SECONDARY2_ADDR" BAO_TOKEN="$DR_SECONDARY2_TOKEN" BAO_CACERT="$DR_CA_CERT" bao write sys/replication/dr/secondary/enable token="$ACT2"
```

Live stream check:

```bash
BAO_ADDR="$DR_PRIMARY_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" BAO_CACERT="$DR_CA_CERT" bao kv put kv/dr-live ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)" src=primary
sleep 2
BAO_ADDR="$DR_SECONDARY1_ADDR" BAO_TOKEN="$DR_SECONDARY1_TOKEN" BAO_CACERT="$DR_CA_CERT" bao read kv/data/dr-live
BAO_ADDR="$DR_SECONDARY2_ADDR" BAO_TOKEN="$DR_SECONDARY2_TOKEN" BAO_CACERT="$DR_CA_CERT" bao read kv/data/dr-live
```

---

## 3. Log Checks

Secondary logs:

```bash
docker logs --since=10m bao-secondary-1 | rg 'dr-secondary|reconciliation|change stream|revoke|PermissionDenied|gap detected'
docker logs --since=10m bao-secondary-2 | rg 'dr-secondary|reconciliation|change stream|revoke|PermissionDenied|gap detected'
```

Primary logs:

```bash
docker logs --since=10m bao-primary-1 | rg 'dr-replication|checkpoint|change stream subscriber|revoked|terminated|lagging'
```

---

## 4. Known Gotchas

- `stream_buffer_entries` is retained ring-history depth, not ACK backlog.
- `stream_lagging_subscribers_total` is cumulative; use `stream_lagging_subscribers_active` for current pressure.
- `stream_journal_*` shows replay horizon health; if `stream_journal_oldest_index` is ahead of a secondary start index, reconcile is expected.
- `secondary_apply_rate_eps`, `primary_write_rate_eps`, and `lag_slope_eps` are the primary convergence signals under sustained load.
- If both secondaries enter long `reconciling` with flat `last_applied_index`, reduce write pressure or increase secondary reconcile/stream batch tuning.
- Strict TLS and relationship authz are fail-closed; stale bootstrap/tokens/certs correctly break reconnect.

---

## 5. Stress Test Matrix

Use scripts in `/Users/roelc/projects/secretz/openbao/scripts`.

| ID | Profile | Command Flags | Goal | Pass Criteria |
|---|---|---|---|---|
| S1 | Single-secondary baseline | `dr_stress_test.sh run --write-count 10000 --concurrency 24 --payload-bytes 1024` | Establish baseline throughput/lag | Sentinel replicated before timeout; no stuck reconcile loop |
| S2 | Dual-secondary baseline | `dr_stress_dual_secondary.sh run --write-count 20000 --concurrency 32 --payload-bytes 1024` | Compare secondary lag under same workload | Both secondaries complete; bounded lag delta |
| S3 | Dual-secondary heavy | `dr_stress_dual_secondary.sh run --write-count 100000 --concurrency 32 --payload-bytes 1024 --progress-interval 2 --ensure-kv --monitor-interval 2` | Sustained load convergence | No indefinite flat `last_applied_index`; eventual catch-up; journal remains within configured budget |
| S4 | Payload stress | `dr_stress_dual_secondary.sh run --write-count 30000 --concurrency 24 --payload-bytes 8192` | High byte pressure behavior | No crash/panic; explicit fail reasons if budgets exceeded |
| S5 | Churn stress | Run S3 while periodically disrupting secondary network/leader | Reconnect + reconcile robustness | Bounded retries, no silent divergence |
| S6 | Cancellation safety | Start any stress run, press `Ctrl-C`, verify no stray workers | Process hygiene | No lingering `dr_stress*`/`bao kv put` worker processes |
| S7 | Mixed realism profile | `dr_stress_mixed_workload.sh run --duration-seconds 1800 --concurrency 24 --put-percent 55 --get-primary-percent 25 --get-secondary1-percent 10 --get-secondary2-percent 10 --hot-key-percent 80 --payload-small-bytes 512 --payload-large-bytes 8192 --large-payload-percent 15` | More production-like traffic mix | Stable ops rate, bounded errors, no prolonged flat secondary indexes |
| S8 | Convergence-controller trigger | S3 with reduced reconcile throughput tuning (for test) | Verify automatic fallback behavior | `fallback_count` increments, secondaries return to `streaming`, `last_applied_index` resumes growth |

### Stress Run Examples

Single secondary:

```bash
bash /Users/roelc/projects/secretz/openbao/scripts/dr_stress_test.sh run \
  --primary-addr "$DR_PRIMARY_ADDR" \
  --primary-token "$DR_PRIMARY_TOKEN" \
  --secondary-addr "$DR_SECONDARY1_ADDR" \
  --secondary-token "$DR_SECONDARY1_TOKEN" \
  --primary-cacert "$DR_CA_CERT" \
  --secondary-cacert "$DR_CA_CERT" \
  --write-count 10000 \
  --concurrency 24 \
  --payload-bytes 1024 \
  --output-dir "$DR_RESULTS_DIR"
```

Dual secondary:

```bash
bash /Users/roelc/projects/secretz/openbao/scripts/dr_stress_dual_secondary.sh run \
  --primary-addr "$DR_PRIMARY_ADDR" \
  --primary-token "$DR_PRIMARY_TOKEN" \
  --secondary1-addr "$DR_SECONDARY1_ADDR" \
  --secondary1-token "$DR_SECONDARY1_TOKEN" \
  --secondary2-addr "$DR_SECONDARY2_ADDR" \
  --secondary2-token "$DR_SECONDARY2_TOKEN" \
  --primary-cacert "$DR_CA_CERT" \
  --secondary1-cacert "$DR_CA_CERT" \
  --secondary2-cacert "$DR_CA_CERT" \
  --secondary1-name secondary-a \
  --secondary2-name secondary-b \
  --write-count 100000 \
  --concurrency 32 \
  --payload-bytes 1024 \
  --progress-interval 2 \
  --output-dir "$DR_RESULTS_DIR"
```

Analyze all runs:

```bash
bash /Users/roelc/projects/secretz/openbao/scripts/dr_stress_test.sh analyze --output-dir "$DR_RESULTS_DIR"
bash /Users/roelc/projects/secretz/openbao/scripts/dr_stress_dual_secondary.sh analyze --output-dir "$DR_RESULTS_DIR"
bash /Users/roelc/projects/secretz/openbao/scripts/dr_stress_mixed_workload.sh analyze --output-dir "$DR_RESULTS_DIR"
```

Quick convergence signal check:

```bash
BAO_ADDR="$DR_SECONDARY1_ADDR" BAO_TOKEN="$DR_SECONDARY1_TOKEN" BAO_CACERT="$DR_CA_CERT" bao read -format=json sys/replication/dr/status | \
  jq '.data | {secondary_state, last_applied_index, primary_index, secondary_apply_rate_eps, primary_write_rate_eps, lag_entries, lag_slope_eps, fallback_count, fallback_last_reason}'
```

Mixed workload example:

```bash
bash /Users/roelc/projects/secretz/openbao/scripts/dr_stress_mixed_workload.sh run \
  --primary-addr "$DR_PRIMARY_ADDR" \
  --primary-token "$DR_PRIMARY_TOKEN" \
  --secondary1-addr "$DR_SECONDARY1_ADDR" \
  --secondary1-token "$DR_SECONDARY1_TOKEN" \
  --secondary2-addr "$DR_SECONDARY2_ADDR" \
  --secondary2-token "$DR_SECONDARY2_TOKEN" \
  --primary-cacert "$DR_CA_CERT" \
  --secondary1-cacert "$DR_CA_CERT" \
  --secondary2-cacert "$DR_CA_CERT" \
  --duration-seconds 1800 \
  --concurrency 24 \
  --put-percent 55 \
  --get-primary-percent 25 \
  --get-secondary1-percent 10 \
  --get-secondary2-percent 10 \
  --hot-key-count 200 \
  --cold-key-count 20000 \
  --hot-key-percent 80 \
  --payload-small-bytes 512 \
  --payload-large-bytes 8192 \
  --large-payload-percent 15 \
  --output-dir "$DR_RESULTS_DIR"
```

Cancellation check:

```bash
ps -Ao pid,ppid,pgid,command | rg 'dr_stress_dual_secondary\.sh run|dr_stress_test\.sh run|bao kv put .*(dr-stress|dr-stress-dual)'
```

Expected: no matches except the `rg` process itself.
