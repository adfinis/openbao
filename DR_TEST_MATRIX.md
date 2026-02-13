# DR Replication Manual Test Matrix

This matrix is for manually validating DR replication in the current codebase.

## Scope

- Unit/integration test commands in this repository.
- Manual end-to-end checks with the `openbao-dr` docker environment.
- Failure scenarios: revoke, disconnect/reconcile, and failover behavior.

## Environment Assumptions

- Repo root: this directory.
- DR docker env repo exists (example): `/Users/roelc/projects/work/rws/openbao-dr`.
- `docker`, `jq`, and `bao` are available.

---

## 1. Automated Matrix (`go test`)

| ID | Area | Command | Expected |
|---|---|---|---|
| A1 | DR unit suite | `go test ./vault -run 'TestDR' -count=1` | Pass |
| A2 | DR integration suite | `go test ./vault -run 'TestDRIntegration' -count=1 -v` | Pass |
| A3 | DR race subset | `go test -race ./vault -run 'TestDRIntegration_(StreamReplication|DisconnectAndReconcile|ReadOnlyEnforcement|GapDetection|BufferOverflowDetection)' -count=1` | Pass, no race |
| A4 | DR range reconciliation | `go test ./vault -run 'TestDRIntegration_(RangeManifestDeterminism|RangeIBLTDecodeSuccess|RangeIBLTDecodeAdaptiveSplit|RangeRefinementPrefixFallback|ReconcileFailsOnBudgetExceeded|NoIndexAdvanceOnPartialRangeFailure|StreamAppliesSameRaftIndexBatchEntries|MultiRelationshipRangeIsolation|RevokeDuringRangeReconcileAborts)' -count=1` | Pass |
| A5 | DR bootstrap/authz hardening | `go test ./vault -run 'TestDR(RelationshipManager_BootstrapTokenExpires|RelationshipManager_BootstrapTokenAttemptLockout|RelationshipManager_BootstrapTokenSourceIPBinding|Primary_RevokeRelationshipTerminatesOnlyMatchingStreams)' -count=1` | Pass |
| A6 | Sketch package | `go test ./physical/replication/sketch -count=1` | Pass |
| A7 | Reconciler package | `go test ./physical/replication/reconciler -count=1` | Pass |
| A8 | Raft stream hooks | `go test ./physical/raft -run 'Test.*ChangeStream|Test.*HookChangeStream|Test.*ApplyBatch' -count=1` | Pass |

Recommended pre-step:

```bash
go test ./... -run '^$' -count=1
```

This verifies compile/link without running tests.

---

## 2. Docker E2E Matrix (`openbao-dr`)

Set environment:

```bash
DR_ENV_DIR=/Users/roelc/projects/work/rws/openbao-dr
cd "$DR_ENV_DIR"
```

Build + refresh clusters:

```bash
cd /Users/roelc/projects/secretz/openbao
make docker-dev

cd "$DR_ENV_DIR"
make fresh-bao
PT=$(sed -n 's/^export BAO_TOKEN=//p' .envrc | head -n1)
```

Initialize secondary once per fresh run:

```bash
ST=$(docker exec rws-bao-10 bao operator init -format=json | jq -r '.root_token')
```

### E2E scenarios

| ID | Scenario | Steps | Expected |
|---|---|---|---|
| E1 | Baseline DR bootstrap | Enable primary, generate token, enable secondary, wait for `streaming`. | Secondary `mode=secondary`, `secondary_state=streaming`. |
| E2 | Initial sync correctness | Verify secondary mounts include `kv/`; read `kv/dr-baseline` after sync. | `kv/` present and baseline value readable. |
| E3 | Stream replication correctness | Write new key on primary (KV v2), read on secondary. | Value appears on secondary quickly. |
| E4 | Revoke enforcement | Revoke relationship on primary while stream is active. | Primary state becomes `revoked`, stream terminated, secondary denied on DR RPCs. |
| E5 | Re-enable after revoke | Disable secondary, generate new token, enable again. | Secondary returns to `streaming`; new writes replicate. |
| E6 | Disconnect + reconcile | Stop primary node briefly, write during disruption, recover primary. | Secondary transitions through reconnect/reconcile and converges. |
| E7 | Failover path | Promote secondary with `sys/replication/dr/secondary/promote`. | Secondary leaves DR secondary mode and serves as standalone primary. |

### Command snippets

Enable DR primary and seed KV:

```bash
docker exec -e BAO_TOKEN="$PT" rws-bao-01 bao write -f sys/replication/dr/primary/enable
docker exec -e BAO_TOKEN="$PT" rws-bao-01 bao secrets enable -path=kv kv-v2 || true
docker exec -e BAO_TOKEN="$PT" rws-bao-01 bao kv put kv/dr-baseline msg=hello ts="$(date +%s)"
```

Enable DR secondary:

```bash
ACT=$(docker exec -e BAO_TOKEN="$PT" rws-bao-01 bao write -f -format=json sys/replication/dr/primary/secondary-token | jq -r '.data.token')
docker exec -e BAO_TOKEN="$ST" rws-bao-10 bao write sys/replication/dr/secondary/enable token="$ACT"
```

Wait for streaming:

```bash
for i in $(seq 1 90); do
  S=$(docker exec -e BAO_TOKEN="$PT" rws-bao-10 bao read -format=json sys/replication/dr/status | jq -r '.data.secondary_state // empty')
  [ "$S" = "streaming" ] && break
  sleep 1
done
docker exec -e BAO_TOKEN="$PT" rws-bao-10 bao read sys/replication/dr/status
```

Live stream verification:

```bash
docker exec -e BAO_TOKEN="$PT" rws-bao-01 bao kv put kv/dr-live ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)" src=primary
sleep 2
docker exec -e BAO_TOKEN="$PT" rws-bao-10 bao read kv/data/dr-live
```

Revoke scenario:

```bash
REL_ID=$(docker exec -e BAO_TOKEN="$PT" rws-bao-01 bao read -format=json sys/replication/dr/primary/relationships | jq -r '.data.relationships[0].relationship_id')
docker exec -e BAO_TOKEN="$PT" rws-bao-01 bao write -f sys/replication/dr/primary/relationships/$REL_ID/revoke
docker exec -e BAO_TOKEN="$PT" rws-bao-01 bao read sys/replication/dr/primary/relationships/$REL_ID/status
docker exec -e BAO_TOKEN="$PT" rws-bao-10 bao read sys/replication/dr/status
```

Re-enable after revoke:

```bash
docker exec -e BAO_TOKEN="$PT" rws-bao-10 bao write -f sys/replication/dr/secondary/disable
ACT=$(docker exec -e BAO_TOKEN="$PT" rws-bao-01 bao write -f -format=json sys/replication/dr/primary/secondary-token | jq -r '.data.token')
docker exec -e BAO_TOKEN="$PT" rws-bao-10 bao write sys/replication/dr/secondary/enable token="$ACT"
```

Primary outage simulation:

```bash
docker stop rws-bao-01
docker start rws-bao-01
docker exec -e BAO_TOKEN="$PT" rws-bao-10 bao read sys/replication/dr/status
```

Failover simulation:

```bash
docker exec -e BAO_TOKEN="$PT" rws-bao-10 bao write -f sys/replication/dr/secondary/promote
docker exec -e BAO_TOKEN="$PT" rws-bao-10 bao read sys/replication/dr/status
```

---

## 3. Log Checks

Secondary DR logs:

```bash
docker logs --since=10m rws-bao-10 | rg 'dr-secondary|reconciliation|change stream|revok|PermissionDenied|gap detected'
```

Primary DR logs:

```bash
docker logs --since=10m rws-bao-01 | rg 'dr-replication|checkpoint|change stream subscriber|revoked|terminated'
```

---

## 4. Known Gotchas During Manual Runs

- Use `docker exec` commands for deterministic API access if host-side LB is noisy.
- After relationship revoke, secondary may stay in reconcile retry loop until disabled/re-enabled.
- If primary cert identity changes in your test env, strict TLS verification will correctly fail reconnect until re-registration with a fresh activation token.

---

## 5. Stress Harness Script

Use `/Users/roelc/projects/secretz/openbao/scripts/dr_stress_test.sh` to run repeatable load tests and analyze results.

Single run example:

```bash
bash /Users/roelc/projects/secretz/openbao/scripts/dr_stress_test.sh run \
  --primary-addr https://localhost:8200 \
  --primary-token "$PT" \
  --secondary-addr https://localhost:8300 \
  --secondary-token "$ST" \
  --primary-cacert /Users/roelc/projects/work/rws/openbao-dr/certs/ca.pem \
  --secondary-cacert /Users/roelc/projects/work/rws/openbao-dr/certs/ca.pem \
  --write-count 5000 \
  --concurrency 32 \
  --payload-bytes 2048 \
  --output-dir /Users/roelc/projects/secretz/openbao/dr-stress-results
```

Analyze all saved runs:

```bash
bash /Users/roelc/projects/secretz/openbao/scripts/dr_stress_test.sh analyze \
  --output-dir /Users/roelc/projects/secretz/openbao/dr-stress-results
```

Dual-secondary comparison run (same workload, side-by-side lag delta):

```bash
bash /Users/roelc/projects/secretz/openbao/scripts/dr_stress_dual_secondary.sh run \
  --primary-addr https://localhost:8200 \
  --primary-token "$PT" \
  --secondary1-addr https://localhost:8300 \
  --secondary1-token "$ST1" \
  --secondary2-addr https://localhost:8400 \
  --secondary2-token "$ST2" \
  --primary-cacert /Users/roelc/projects/work/rws/openbao-dr/certs/ca.pem \
  --secondary1-cacert /Users/roelc/projects/work/rws/openbao-dr/certs/ca.pem \
  --secondary2-cacert /Users/roelc/projects/work/rws/openbao-dr/certs/ca.pem \
  --secondary1-name secondary-a \
  --secondary2-name secondary-b \
  --write-count 8000 \
  --concurrency 40 \
  --payload-bytes 2048 \
  --output-dir /Users/roelc/projects/secretz/openbao/dr-stress-results
```

Analyze dual-secondary runs:

```bash
bash /Users/roelc/projects/secretz/openbao/scripts/dr_stress_dual_secondary.sh analyze \
  --output-dir /Users/roelc/projects/secretz/openbao/dr-stress-results
```
