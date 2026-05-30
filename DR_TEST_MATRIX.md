# DR Replication Manual Test Matrix

This matrix defines manual and automated validation for DR replication in this repository.

## Scope

- Unit and integration tests under `/Users/roelc/projects/secretz/openbao`.
- End-to-end validation against a local multi-cluster Docker test environment.
- Failure-path checks: reconnect/reconcile, revoke, failover, and sustained load.

## Environment Assumptions

- OpenBao repo root: `/Users/roelc/projects/secretz/openbao`
- Tools available: `docker`, `jq`, `bao`, `rg`.
- Local compose topology: `docker-compose.dr-test.yml`
- Local HA compose topology: `docker-compose.dr-ha-test.yml`
- DR endpoints and tokens are exported by `scripts/dr_local_test.sh bootstrap` into `.dr-test.env`:
  - `DR_PRIMARY_ADDR`, `DR_PRIMARY_TOKEN`
  - `DR_SECONDARY1_ADDR`, `DR_SECONDARY1_TOKEN`
  - `DR_SECONDARY2_ADDR`, `DR_SECONDARY2_TOKEN`

---

## 1. Automated Matrix (`go test`)

| ID | Area | Command | Expected |
|---|---|---|---|
| A1 | DR unit suite | `go test ./vault -run 'TestDR' -count=1` | Pass |
| A2 | DR integration suite | `go test ./vault -run 'TestDRIntegration' -count=1 -v` | Pass |
| A3 | DR race subset | `go test -race ./vault -run 'TestDRIntegration_(StreamReplication|DisconnectAndReconcile|ReadOnlyEnforcement|GapDetection|BufferOverflowDetection)' -count=1` | Pass, no races |
| A4 | DR range reconciliation | `go test ./vault -run 'TestDRIntegration_(RangeManifestDeterminism|RangeIBLTDecodeSuccess|RangeIBLTDecodeAdaptiveSplit|RangeRefinementPrefixFallback|ReconcileFailsOnBudgetExceeded|NoIndexAdvanceOnPartialRangeFailure|MultiRelationshipRangeIsolation|RevokeDuringRangeReconcileAborts)' -count=1` | Pass |
| A5 | DR stream cursor/replay correctness | `go test ./vault -run 'TestDRIntegration_(StreamAppliesSameRaftIndexBatchEntries|PrimaryStreamReplayIncludesLastAppliedIndex)' -count=1` | Pass |
| A6 | DR bootstrap/authz hardening | `go test ./vault -run 'TestDR(RelationshipManager_ActivationToken|RelationshipManager_LegacyPlaintextBootstrapTokenMigratesToVerifier|RelationshipManager_BootstrapTokenExpires|RelationshipManager_BootstrapTokenAttemptLockout|RelationshipManager_BootstrapTokenSourceIPBinding|RelationshipManager_BootstrapReplayDoesNotMutateRegisteredRelationship|RelationshipManager_HeartbeatLastSeenWriteThrottle|Primary_RevokeRelationshipTerminatesOnlyMatchingStreams)' -count=1` | Pass |
| A7 | DR manager persistence rollback | `go test ./vault -run 'TestDRRelationshipManager_(EnablePrimary_SaveConfigFailureRollsBackState|EnableSecondary_SaveConfigFailureRollsBackState|UpdateTuningAppliesSecondaryRuntime)' -count=1` | Pass |
| A8 | Sketch package | `go test ./physical/replication/sketch -count=1` | Pass |
| A9 | Reconciler package | `go test ./physical/replication/reconciler -count=1` | Pass |
| A10 | Raft stream hooks | `go test ./physical/raft -run 'Test.*ChangeStream|Test.*HookChangeStream|Test.*ApplyBatch' -count=1` | Pass |
| A11 | Artifact fetch + budget semantics | `go test ./vault -run 'TestDRPrimary_ReadCheckpointEntryChange_(ExpectedVIDMismatch|UsesArtifactNotLiveStorage)|TestDRCheckpointArtifactStore_(EvictsByGlobalBudget|RejectsOversizedArtifact)' -count=1` | Pass |
| A12 | Reconcile failure taxonomy | `go test ./vault -run 'TestClassifyReconcileFailure_(Stalled|CheckpointClasses)' -count=1` | Pass |
| A13 | DR correctness hardening | `go test ./vault -run 'TestDRIntegration_(RangeReconciliationFinalizeFailureIsRetryable|ResnapshotRejectsSilentFetchOmission|CheckpointHighWaterLoadSeedsLastAppliedIndex|RootKeyReplicationFailsClosed)|TestDRCheckpointArtifactStore_DoesNotEvictRetainedArtifact|TestDRSecondary_Start_HeartbeatPermissionDeniedCancelsStream' -count=1` | Pass |
| A14 | DR endpoint/audit boundary | `go test ./vault -run 'TestSystemBackend_DRSpecialPaths|TestSystemBackend_DRSensitiveFields|TestSystemBackend_DRAuditHMACsBootstrapAndRotationMaterial|TestSystemBackend_RootPaths' -count=1` | Pass |
| A15 | DR response disclosure boundary | `go test ./vault -run 'TestDRSystemBackend_StatusDoesNotExposePromotionSecrets|TestDRSystemBackend_RelationshipResponsesDoNotExposeStoredSecrets' -count=1` | Pass |
| A16 | DR unauthenticated error-oracle hardening | `go test ./vault -run 'TestDRSystemBackend_BootstrapRegistrationFailureResponseIsGeneric|TestDRSystemBackend_CredentialRotationFailureResponseIsGeneric' -count=1` | Pass |
| A17 | DR gRPC data-plane authz | `go test ./vault -run 'TestDRPrimary_(StreamChangesRequiresRelationshipAuthorization|RequestCheckpointRequiresRelationshipAuthorization|HeartbeatRequiresRelationshipAuthorization|CheckpointRPCsRequireRelationshipAuthorization|ExchangeDirtyBitmap_RequiresRelationshipAuthorization|SyncKeyringRequiresRelationshipAuthorization)|TestDRIntegration_(MultiRelationshipRangeIsolation|RevokeDuringRangeReconcileAborts)' -count=1` | Pass |
| A18 | DR SyncKeyring crypto binding | `go test ./vault -run 'TestDRPrimary_SyncKeyringRequiresRelationshipAuthorization|TestDRRootKey(WrapAADBindingRejectsMismatches|WrapRequiresCompleteInputs|WrapProducesFreshEnvelopeForRepeatedClientNonce|UnwrapRejectsMalformedEnvelope)|TestDRIntegration_RootKeyReplicationFailsClosed' -count=1` | Pass |
| A19 | DR unauthenticated resource-exhaustion hardening | `go test ./vault -run 'TestDRRelationshipManager_SecondaryCredentialRotation(RejectsRegisteredBeforeParsingCertificate|ConfirmRejectsMissingPendingBeforeParsingCertificate|TwoPhase)|TestDRSystemBackend_CredentialRotationFailureResponseIsGeneric' -count=1` | Pass |
| A20 | DR revocation fail-closed semantics | `go test ./vault -run 'TestDRPrimary_(StreamChangesRequiresRelationshipAuthorization|RequestCheckpointRequiresRelationshipAuthorization|HeartbeatRequiresRelationshipAuthorization|CheckpointRPCsRequireRelationshipAuthorization|ExchangeDirtyBitmap_RequiresRelationshipAuthorization|SyncKeyringRequiresRelationshipAuthorization|SyncKeyringRevocationDuringWrapFailsClosed)|TestFetchEntries_RechecksRelationshipRevocationDuringStream|TestDRRelationshipManager_RevokeClearsPendingCredentialRotationTrust|TestDRIntegration_RevokeDuringRangeReconcileAborts' -count=1` | Pass |
| A21 | DR promotion lineage inheritance | `go test ./vault -run 'TestDRRelationshipManager_(EnableSecondaryRejectsStalePromotionLineage|RepeatedPromotionPreservesStaleLineage|BootstrapRejectsPrePromotionSecondaryFingerprint|LoadConfigSkipsStalePromotionRelationshipCerts)|TestDRSystemBackend_StatusDoesNotExposePromotionSecrets|TestDRFailoverToPrimary' -count=1` | Pass |
| A22 | DR promotion UX and safety contract | `go test ./vault -run 'TestDRFailover_(FromSecondary|ForcedRequiresAcceptDataLoss|ForcedWithAcceptDataLoss|ForcedWithZeroEstimatedLossExplainsCleanProofUnavailable|ToPrimary)|TestDRFailoverToPrimary|TestDRSystemBackend_PromoteResponseExplainsForcedPromotion|TestDRSystemBackend_StatusDoesNotExposePromotionSecrets' -count=1` | Pass |
| A23 | DR promoted-authority reseed cursor reset | `go test ./vault -run 'TestDRRelationshipManager_EnableSecondaryClearsStaleCheckpointCursor' -count=1` | Pass |
| A24 | DR replicated runtime engine state | `go test ./vault -run 'TestDRIntegration_RuntimeStateRefreshReplacesIdentityRoute|TestDRIntegration_RuntimeStateRefreshReloadsIdentityArtifacts|TestDRIntegration_RuntimeStateRefreshLoadsReplicatedNamespace|TestDRIntegration_RuntimeRefreshProtectionAllowsNamespaceSingletons|TestDRIntegration_DRSecondarySetupSkipsExistingProtectedRoutes|TestDRSecondaryApplyFetchedChangeInvalidatesRouteBackedStorage' -count=1` | Pass |
| A25 | DR fetch/reconcile resource bounds | `go test ./vault -run 'TestFetchEntriesRejectsOversizedAndMalformedRequests|TestFetchEntriesSplitsResponseBatchesByByteBudget|TestFetchEntriesRejectsSingleEntryOverResponseByteBudget|TestDRIntegration_RangeTaskEnforcesFetchedValueByteBudget' -count=1` | Pass |
| A26 | DR checkpoint build admission | `go test ./vault -run 'TestDRPrimary_CheckpointBuildAdmissionLimitsCrossRelationshipConcurrency' -count=1` | Pass |
| A27 | DR tuning validation and rollback | `go test ./vault -run 'TestDRRelationshipManager_UpdateTuning|TestDRSystemBackend_DRTuningRejectsInvalidInputs|TestDRPrimary_AllowWriteRequest_BackpressureRejectsNonExempt|TestDRBackpressureExemptPath' -count=1` | Pass |

Recommended compile pre-step:

```bash
go test ./... -run '^$' -count=1
```

---

## 2. Docker E2E Matrix

Set shell environment:

```bash
OPENBAO_REPO_DIR="/Users/roelc/projects/secretz/openbao"
DR_RESULTS_DIR="$OPENBAO_REPO_DIR/dr-stress-results"
cd "$OPENBAO_REPO_DIR"
```

Build OpenBao image and refresh local test clusters:

```bash
make docker-dev
scripts/dr_local_test.sh reset
```

The reset command starts `docker-compose.dr-test.yml`, initializes and unseals the
three single-node Raft clusters, enables DR primary mode, enables both
secondaries, waits for `secondary_state=streaming` and `lag_entries=0`, and writes
tokens to `.dr-test.env`.

For full-feature local validation, use the HA topology:

```bash
make docker-dev
scripts/dr_local_test.sh --topology ha reset
scripts/dr_local_test.sh --topology ha engine-matrix
scripts/dr_local_test.sh --topology ha engine-lifecycle-matrix
scripts/dr_local_test.sh --topology ha reset
scripts/dr_local_test.sh --topology ha smoke --duration 900 --concurrency 48 --stepdown-interval 300
scripts/dr_local_test.sh --topology ha verify --sample 0
scripts/dr_local_test.sh --topology ha smoke --duration 7200 --concurrency 24 --put-percent 55 --get-primary-percent 25 --status-s1-percent 10 --status-s2-percent 10 --max-wait-seconds 600 --progress-interval 30
scripts/dr_local_test.sh --topology ha failover-smoke
scripts/dr_local_test.sh --topology ha promoted-durability-smoke
scripts/dr_local_test.sh --topology ha reseed-secondary-smoke
scripts/dr_local_test.sh --topology ha failover-load-lifecycle
```

The HA topology runs nine containers: three Raft nodes for the primary cluster,
three for secondary #1, and three for secondary #2. This is the local topology
for active stepdown, standby forwarding, Raft peer behavior, DR stream recovery,
promotion tests, promoted-cluster durability checks, and explicit secondary
reseed validation. Run the promoted durability and reseed smokes after
`failover-smoke` without resetting; they intentionally consume the promoted
state created by the failover smoke.

Useful local shortcuts:

```bash
make dr-test-status
make dr-test-smoke
make dr-test-verify
make dr-test-engine-matrix
make dr-test-down
make dr-test-ha-reset
make dr-test-ha-smoke
make dr-test-ha-verify
make dr-test-ha-engine-matrix
make dr-test-ha-engine-lifecycle-matrix
make dr-test-ha-failover-smoke
make dr-test-ha-promoted-durability-smoke
make dr-test-ha-reseed-secondary-smoke
make dr-test-ha-failover-load-lifecycle
make dr-test-ha-down
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
| E8 | Failover path | Hard-stop old primary, promote secondary #1 with `accept_data_loss=true`, restart old primary, and compare key visibility. Use `scripts/dr_local_test.sh --topology ha failover-smoke`. | Forced promotion requires explicit acknowledgement; promoted cluster keeps its own post-promotion writes; resurrected old primary and non-promoted secondary form a separate timeline with no automatic merge. |
| E9 | Promotion lineage fence | `go test ./vault -run 'TestDRRelationshipManager_EnableSecondaryRejectsStalePromotionLineage' -count=1`; also asserted by `scripts/dr_local_test.sh --topology ha failover-smoke`. | After promotion, stale activation tokens from the old primary cluster or old relationship cannot re-enable the promoted cluster as a secondary. Fresh lineage remains possible for explicit rebuild/reconfiguration flows. |
| E10 | Promotion durability | Run after E8: `scripts/dr_local_test.sh --topology ha promoted-durability-smoke`. | Promoted HA cluster remains `mode=disabled` with the same promotion record, keeps all raft voters unsealed, preserves writes across full promoted-cluster restart, rejects stale old-primary tokens after restart, and accepts writes after HA active handoff. |
| E11 | Explicit secondary reseed | Run after E8/E10: `scripts/dr_local_test.sh --topology ha reseed-secondary-smoke`. | Promoted cluster can be explicitly enabled as the new DR primary; secondary2 can be disabled from the old lineage and re-enabled from a fresh promoted-authority token; promoted-only keys replicate, old-primary-only keys are removed, and old primary remains separate. |
| E12 | Failover-under-load lifecycle | Run `scripts/dr_local_test.sh --topology ha failover-load-lifecycle` or `make dr-test-ha-failover-load-lifecycle`. The command resets the HA topology, runs the `scripts/dr-stress` mixed KV workload against the old primary, hard-stops all old-primary nodes mid-run, promotes secondary1 with explicit `accept_data_loss=true`, exhaustively verifies the promoted secondary1 truth log, then runs E10 and E11 without resetting. | No-ack promotion fails closed; accepted promotion reports clean/forced semantics, reason codes/details, acknowledgement state, and data-loss estimate basis; promoted secondary1 has no confirmed missing keys or mismatches; promoted durability passes; secondary2 can be re-seeded from the promoted authority and reaches `streaming` with `lag_entries=0`. |
| E13 | Steady-state HA soak | Reset the HA topology, run `scripts/dr_local_test.sh --topology ha smoke --duration 7200 --concurrency 24 --put-percent 55 --get-primary-percent 25 --status-s1-percent 10 --status-s2-percent 10 --max-wait-seconds 600 --progress-interval 30`, then run `scripts/dr_local_test.sh --topology ha verify <run-dir> --sample 0`. | No put/get/status failures, no dropped stress events, no backpressure rejections, both secondaries remain `streaming` with `lag_entries=0`, final sentinel converges, and exhaustive verification passes on primary, secondary1, and secondary2. |
| E14 | Engine/runtime feature matrix | Run `scripts/dr_local_test.sh --topology ha engine-matrix` or `make dr-test-ha-engine-matrix`. | Primary, secondary1, and secondary2 all verify namespace creation, namespace-scoped KV v2 state, root KV v2 data, KV v1 data, transit key material, PKI CA/role/issued certificate reads, SSH CA/role state, TOTP key state, database engine config with connection verification disabled, userpass/AppRole/cert/JWT auth mount state, token roles, ACL policy replication, service-token self lookup, and identity entity/group/alias replication. This is the default self-contained parity matrix; dependency-backed auth/secret engines still need opt-in profiles. |
| E15 | Engine/runtime lifecycle matrix | From a clean streaming HA topology, run `scripts/dr_local_test.sh --topology ha engine-lifecycle-matrix` or `make dr-test-ha-engine-lifecycle-matrix`. | The E14 engine/runtime matrix passes before failover on both secondaries, still passes on promoted secondary1 after forced failover, and still passes on secondary2 after explicit reseed from the promoted authority. |

### Command Snippets (address-based, no container-name dependency)

Enable DR primary and seed baseline key:

```bash
source .dr-test.env
BAO_ADDR="$DR_PRIMARY_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" bao write -f sys/replication/dr/primary/enable
BAO_ADDR="$DR_PRIMARY_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" bao secrets enable -path=kv kv-v2 || true
BAO_ADDR="$DR_PRIMARY_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" bao kv put kv/dr-baseline msg=hello ts="$(date +%s)"
```

Enable secondary #1:

```bash
ACT1=$(BAO_ADDR="$DR_PRIMARY_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" bao write -f -format=json sys/replication/dr/primary/secondary-token | jq -r '.data.token')
BAO_ADDR="$DR_SECONDARY1_ADDR" BAO_TOKEN="$DR_SECONDARY1_TOKEN" bao write sys/replication/dr/secondary/enable token="$ACT1"
```

Enable secondary #2:

```bash
ACT2=$(BAO_ADDR="$DR_PRIMARY_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" bao write -f -format=json sys/replication/dr/primary/secondary-token | jq -r '.data.token')
BAO_ADDR="$DR_SECONDARY2_ADDR" BAO_TOKEN="$DR_SECONDARY2_TOKEN" bao write sys/replication/dr/secondary/enable token="$ACT2"
```

Live stream check:

```bash
BAO_ADDR="$DR_PRIMARY_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" bao kv put kv/dr-live ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)" src=primary
sleep 2
BAO_ADDR="$DR_SECONDARY1_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" bao read kv/data/dr-live
BAO_ADDR="$DR_SECONDARY2_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" bao read kv/data/dr-live
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
- `journal_replay_attempts_total`, `journal_replay_success_total`, `journal_range_too_old_total` explain reconnect behavior before reconcile.
- `checkpoint_conflicts_storage_drift_total` should remain `0` after artifact-only fetch cutover.
- `secondary_apply_rate_eps`, `primary_write_rate_eps`, and `lag_slope_eps` are the primary convergence signals under sustained load.
- `dr_backpressure_state` and `dr_backpressure_effective_qps_cap` indicate when ingress throttling is active.
- Sustained `dr_backpressure_rejections_total` growth means the cluster is protecting convergence with bounded write admission (expected under overload tests).
- If both secondaries enter long `reconciling` with flat `last_applied_index`, reduce write pressure or increase secondary reconcile/stream batch tuning.
- Strict TLS and relationship authz are fail-closed; stale bootstrap/tokens/certs correctly break reconnect.
- In the HA Docker topology, `api_addr` uses container DNS names. Host-side arbitrary-token lookup against a DR secondary can redirect to those internal names; the engine matrix uses service-token self lookup to verify replicated token usability without relying on host DNS for secondary leader redirects.
- Namespace replication requires runtime refresh rather than generic invalidation. Generic invalidation of `core/namespaces/*` can collide with already-mounted namespace singleton routes; the lifecycle matrix verifies namespace lookup, namespace `sys/mounts`, and namespace KV after failover and reseed.
- Audit devices are declarative/config-managed in OpenBao and cannot be enabled through the API by the default matrix. Audit parity should be validated by a separate topology profile with audit devices predeclared in config.
- The default engine matrix intentionally covers self-contained built-ins. LDAP, Kubernetes, RADIUS, Kerberos, RabbitMQ, and live database credential issuance require external services and should be covered by explicit dependency-backed profiles rather than making the default lifecycle test brittle.

---

## 5. Stress Test Matrix

Use `scripts/dr_local_test.sh` for local compose runs. Lower-level stress
scripts remain available under `/Users/roelc/projects/secretz/openbao/scripts`.

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
| S9 | Backpressure enforcement | S3 with high write concurrency and low secondary apply tuning | Verify bounded ingress on overload | `dr_backpressure_state` reaches `degraded/critical`, `dr_backpressure_rejections_total` increases, secondaries avoid permanent flatline |
| S10 | HA steady-state soak | `scripts/dr_local_test.sh --topology ha smoke --duration 7200 --concurrency 24 --put-percent 55 --get-primary-percent 25 --status-s1-percent 10 --status-s2-percent 10 --max-wait-seconds 600 --progress-interval 30` | Measure non-failover DR behavior under sustained but non-adversarial load | 0 workload failures, 0 dropped events, both secondaries converge with lag 0, and exhaustive verification passes with `scripts/dr_local_test.sh --topology ha verify <run-dir> --sample 0` |
| S11 | Dynamic tuning under HA load | `scripts/dr_local_test.sh --topology ha tuning-load-smoke` | Update primary and secondary DR tuning while mixed load is running, then force HA handoffs | Tuning writes succeed, new active nodes report the updated profile, workload exits cleanly, both secondaries return to `streaming` with lag 0, and exhaustive verification passes on primary and both secondaries |

### Stress Run Examples

Local compose smoke:

```bash
scripts/dr_local_test.sh smoke --duration 120 --concurrency 24
scripts/dr_local_test.sh verify --sample 0
```

Repo-local HA steady-state soak:

```bash
scripts/dr_local_test.sh --topology ha reset
scripts/dr_local_test.sh --topology ha smoke \
  --duration 7200 \
  --concurrency 24 \
  --put-percent 55 \
  --get-primary-percent 25 \
  --status-s1-percent 10 \
  --status-s2-percent 10 \
  --max-wait-seconds 600 \
  --progress-interval 30
scripts/dr_local_test.sh --topology ha verify <run-dir> --sample 0
```

Single secondary:

```bash
bash /Users/roelc/projects/secretz/openbao/scripts/dr_stress_test.sh run \
  --primary-addr "$DR_PRIMARY_ADDR" \
  --primary-token "$DR_PRIMARY_TOKEN" \
  --secondary-addr "$DR_SECONDARY1_ADDR" \
  --secondary-token "$DR_PRIMARY_TOKEN" \
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
  --secondary1-token "$DR_PRIMARY_TOKEN" \
  --secondary2-addr "$DR_SECONDARY2_ADDR" \
  --secondary2-token "$DR_PRIMARY_TOKEN" \
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
BAO_ADDR="$DR_SECONDARY1_ADDR" BAO_TOKEN="$DR_PRIMARY_TOKEN" bao read -format=json sys/replication/dr/status | \
  jq '.data | {secondary_state, last_applied_index, primary_index, secondary_apply_rate_eps, primary_write_rate_eps, lag_entries, lag_slope_eps, fallback_count, fallback_last_reason, checkpoint_conflicts_storage_drift_total, dr_backpressure_state, dr_backpressure_rejections_total}'
```

Mixed workload example:

```bash
bash /Users/roelc/projects/secretz/openbao/scripts/dr_stress_mixed_workload.sh run \
  --primary-addr "$DR_PRIMARY_ADDR" \
  --primary-token "$DR_PRIMARY_TOKEN" \
  --secondary1-addr "$DR_SECONDARY1_ADDR" \
  --secondary1-token "$DR_PRIMARY_TOKEN" \
  --secondary2-addr "$DR_SECONDARY2_ADDR" \
  --secondary2-token "$DR_PRIMARY_TOKEN" \
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
