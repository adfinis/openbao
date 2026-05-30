# DR Replication Validation Results

**Status**: internal PoC validation memo.

**Scope**: summarize the DR replication validation runs performed against the
historical multi-cluster environment in `/Users/roelc/projects/work/rws/openbao-dr`.
New validation runs should use the repo-local `docker-compose.dr-test.yml` and
`scripts/dr_local_test.sh` harness unless a larger external topology is needed.

## Executive Summary

The current PoC shows a strong data-correctness signal under adversarial stress.
Across the most important runs, the secondaries recovered from full stream
buffer pressure, active handoffs, reconciliation, and client-visible
availability failures without final data divergence.

The clearest result is the 15-minute HAProxy stress run:

- 141,696 operations with forced primary stepdowns every 5 minutes.
- Both secondaries entered reconciliation and later returned to streaming.
- Sentinel convergence completed after the workload stopped.
- Exhaustive verification of all 11,572 terminal keys passed on primary,
  secondary1, and secondary2.

The main unresolved issue is availability during active handoff under sustained
write and DR backlog pressure. Clients observed transient 500/503 responses and
forwarded RPC/TLS errors. This was reproduced both when pinned to a direct node
and when routed through HAProxy. This is tracked separately as `HA-005` in
`DR_BUG_TRACKER.md`.

Repo-local HA failover-under-load reruns have produced confirmed-write
correctness results on the promoted cluster and kept the promoted HA secondary
healthy: all secondary1 nodes remained unsealed and raft retained all three
voters after promotion. The earlier promoted-cluster HA recovery failure is
tracked as fixed under `DR-007`.

The latest repo-local HA failover-under-load rerun on 2026-05-30 rebuilt
`openbao:dev`, reset the nine-node topology, hard-stopped the old primary under
load, and promoted secondary1 with the new promotion UX contract. The no-ack
promotion path failed closed, the accepted promotion returned explicit forced
reason codes/details and the data-loss estimate basis, exhaustive verification
of the promoted cluster passed, and all promoted secondary1 HA nodes remained
unsealed with three Raft voters.

A follow-up promotion durability smoke run restarted all promoted secondary1 HA
nodes, verified the promotion lineage record survived, confirmed stale
old-primary activation tokens still fail closed after restart, forced an HA
active handoff, and wrote successfully on the new active.

The explicit reseed smoke then made the promoted cluster the new DR primary,
disabled secondary2 from the old lineage, re-enabled secondary2 with a fresh
token from the promoted authority, and confirmed old-primary-only keys were not
merged into the promoted timeline.

A 2026-05-30 repo-local HA lifecycle rerun rebuilt `openbao:dev`, reset the
nine-node topology, repeated forced failover, repeated promoted-cluster
durability, and repeated explicit secondary reseed. The rerun passed end to end
and confirmed the current working tree still preserves the failover/reseed
semantics after the latest security and lineage hardening.

A later 2026-05-30 reseed rerun exposed and fixed `DR-020`: secondary2 retained
the old lineage's checkpoint high-water cursor when re-enabled against the
promoted authority. If the promoted authority's checkpoint index was lower,
secondary2 stayed in `reconciling` and rejected the new checkpoint as a rollback.
The fix clears the local checkpoint cursor on secondary disable/enable. A fresh
rebuilt-image lifecycle rerun passed failover smoke, promoted durability, and
explicit secondary2 reseed after the fix.

A stronger post-fix sequence then repeated failover under load and reused that
load-generated promoted state for durability and reseed validation. The run
hard-stopped the old primary under a 48-worker workload, required explicit
`accept_data_loss=true` for forced promotion, exhaustively verified promoted
secondary1 with no confirmed missing keys or mismatches, restarted and handed
off the promoted HA cluster, and re-seeded secondary2 from the promoted
authority with `lag_entries=0`.

The sequence is now captured as a first-class repo-local target:
`make dr-test-ha-failover-load-lifecycle`. A default-profile run of that target
on 2026-05-30 passed end to end.

A 2-hour repo-local HA steady-state soak on 2026-05-30 also passed. The run
used a non-failover profile with 24 workers, 55% writes, 25% primary reads, and
20% DR status checks. It completed 855,321 operations with zero put, get, or
status failures, both secondaries converged in about one second, and exhaustive
verification of 20,024 terminal keys passed on primary, secondary1, and
secondary2.

A repo-local HA engine/runtime matrix on 2026-05-30 now broadens validation
beyond KV stress traffic. It verifies a self-contained parity set: KV v2, KV
v1, transit, PKI, SSH, TOTP, database engine configuration, userpass, AppRole,
cert auth, JWT auth, token roles, namespaces with namespace-scoped KV v2, ACL
policies, service-token authentication, and identity entity/group/alias state on
the primary and both secondaries. The expanded matrix exposed several DR
runtime correctness gaps: route-backed backend caches needed explicit
invalidation after replicated storage apply; the identity singleton route
needed to be replaced from replicated mount state during runtime refresh, even
when the mount table had already refreshed independently; and identity artifacts
needed to be reloaded from replicated storage after route/namespace refresh.
Namespace lifecycle validation also exposed that replicated namespace state must
be loaded during runtime refresh and that namespace-scoped singleton routes must
be refreshed from the replicated mount table. Those gaps were fixed and covered
by unit/integration tests and the HA lifecycle matrix.

A focused resource-control pass on 2026-05-30 tightened the checkpoint fetch
path. `FetchEntries` response streams now split batches by approximate payload
bytes as well as entry count, reject a single entry that cannot fit inside the
response byte budget, and the secondary reconcile budget now accounts the actual
bytes fetched from checkpoint artifacts instead of a per-entry placeholder. A
follow-up pass also added primary-side checkpoint build admission: requests from
the same relationship share the in-flight build while cross-relationship build
storms fail fast once the global build limit is reached.

A DR tuning validation pass on 2026-05-30 now rejects invalid tuning before
persistence or runtime apply. This closes signed-to-unsigned API conversion
hazards for byte/entry budgets, enforces cross-field budget and backpressure
relationships, and verifies manager/API rollback so rejected updates cannot
leave partial in-memory tuning behind.

## Test Environment

- OpenBao repository: `/Users/roelc/projects/secretz/openbao`

Historical external HA environment:

- DR environment: `/Users/roelc/projects/work/rws/openbao-dr`
- Topology: one primary HA Raft cluster and two secondary HA Raft clusters
- Primary cluster nodes: `rws-bao-01`, `rws-bao-02`, `rws-bao-03`
- Secondary1 nodes: `rws-bao-10`, `rws-bao-11`, `rws-bao-12`
- Secondary2 nodes: `rws-bao-20`, `rws-bao-21`, `rws-bao-22`
- Direct primary node endpoint used in one run: `https://localhost:8201`
- HAProxy cluster endpoints:
  - primary: `https://localhost:8200`
  - secondary1: `https://localhost:8300`
  - secondary2: `https://localhost:8400`

Repo-local environments:

- Single-node compose topology: `docker-compose.dr-test.yml`
- HA compose topology: `docker-compose.dr-ha-test.yml`
- Harness: `scripts/dr_local_test.sh`
- Runtime environment file: `.dr-test.env`
- HA topology: nine containers, with three Raft voters each for primary,
  secondary1, and secondary2.

The stress profile is intentionally adversarial. It combines sustained writes,
hot-key contention, large payloads, forced active stepdowns, full DR stream
buffer pressure, and secondary reconciliation.

## Current Correctness Result

The strongest correctness result is from:

`/Users/roelc/projects/work/rws/openbao-dr/dr-stress-results/drmixed-15m-proxy-stepdown-20260529T191508Z`

Workload:

| Metric | Value |
| --- | ---: |
| Duration | 900s |
| Concurrency | 48 workers |
| Stepdown interval | 300s |
| Total operations | 141,696 |
| Throughput | 143.06 ops/s |
| Successful puts | 63,327 |
| Failed puts | 443 |
| Successful primary gets | 35,481 |
| Failed primary gets | 62 |
| Successful DR status checks | 42,383 |
| Failed DR status checks | 0 |
| Dropped stress events | 0 |

Replication convergence:

| Secondary | Converged | Final lag | Final applied index | Primary index | Sentinel wait |
| --- | --- | ---: | ---: | ---: | ---: |
| secondary1 | yes | 0 | 199,350 | 199,350 | 84.0s |
| secondary2 | yes | 0 | 199,356 | 199,350 | 1.0s |

Exhaustive verification:

| Target | Keys checked | Matches | Missing | Mismatches | Errors | Result |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| primary | 11,572 | 11,572 | 0 | 0 | 0 | PASS |
| secondary1 | 11,572 | 11,572 | 0 | 0 | 0 | PASS |
| secondary2 | 11,572 | 11,572 | 0 | 0 | 0 | PASS |

Verification artifacts:

- `/Users/roelc/projects/work/rws/openbao-dr/dr-stress-results/drmixed-15m-proxy-stepdown-20260529T191508Z/verify-primary-all.json`
- `/Users/roelc/projects/work/rws/openbao-dr/dr-stress-results/drmixed-15m-proxy-stepdown-20260529T191508Z/verify-secondary1-all.json`
- `/Users/roelc/projects/work/rws/openbao-dr/dr-stress-results/drmixed-15m-proxy-stepdown-20260529T191508Z/verify-secondary2-all.json`

Interpretation:

- Final replicated KV data matched exactly on both secondaries.
- Reconciliation recovered after stream buffer pressure and active handoff.
- Client-visible write/read availability failures did not translate into final
  data divergence.

## Repo-Local Failover Under Load

Latest fixed run artifact:

`/Users/roelc/projects/secretz/openbao/dr-stress-results/failover-load-fixed-20260529T212724Z`

Profile:

| Metric | Value |
| --- | ---: |
| Warm-up before failover | 75s |
| Concurrency | 48 workers |
| Put mix | 70% |
| Total operations | 11,167 |
| Throughput | 126.55 ops/s |
| Successful puts | 6,810 |
| Failed puts | 1,008 |
| Successful primary gets | 1,959 |
| Failed primary gets | 290 |
| Successful DR status checks | 1,100 |
| Failed DR status checks | 0 |
| Dropped stress events | 0 |

Pre-failover replication status:

| Secondary | State | Lag | Last applied index | Primary index | Primary write rate |
| --- | --- | ---: | ---: | ---: | ---: |
| secondary1 | streaming | 0 | 6,832 | 6,763 | 70.20 eps |
| secondary2 | streaming | 0 | 6,836 | 6,763 | 66.55 eps |

Promotion result:

| Field | Value |
| --- | --- |
| Promoted cluster | secondary1 |
| Promotion class | clean |
| Data loss accepted | false |
| Estimated data loss entries | 0 |
| Last applied index | 6,895 |
| Last known primary index | 6,847 |
| Promotion duration | 2.08s |

Exhaustive verification on promoted secondary1:

| Keys checked | Matches | Missing | Mismatches | Errors | Uncertain missing | Result |
| ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 1,447 | 1,439 | 0 | 0 | 0 | 8 | PASS |

Interpretation:

- No confirmed-write data loss was observed.
- The 8 uncertain missing keys came from commit-uncertain tail writes after the
  primary was stopped and are not counted as confirmed data loss.
- The promoted secondary accepted a post-promotion marker write.
- The promoted HA cluster stayed healthy after promotion: secondary1-1,
  secondary1-2, and secondary1-3 all reported `sealed=false`, and raft retained
  all three voters.
- The known promoted-cluster failure markers were absent from secondary1 logs:
  no fatal invalidation dispatch, read-only post-unseal failure, OIDC default
  write failure, duplicate route creation, or invalidation dispatch failure.
- Earlier run `failover-load-20260529T210509Z` produced the same
  confirmed-write correctness signal but degraded the promoted HA cluster when
  standby nodes sealed during the key transition. That issue is fixed as
  `DR-007`.

Earlier rebuilt-image run artifact:

`/Users/roelc/projects/secretz/openbao/dr-stress-results/failover-load-20260530T102316Z`

Profile:

| Metric | Value |
| --- | ---: |
| Warm-up before failover | 75s |
| Concurrency | 48 workers |
| Put mix | 70% |
| Total operations | 12,575 |
| Throughput | 80.15 ops/s |
| Successful puts | 6,971 |
| Failed puts | 1,824 |
| Successful primary gets | 1,967 |
| Failed primary gets | 512 |
| Successful DR status checks | 1,301 |
| Failed DR status checks | 0 |
| Dropped stress events | 0 |

Pre-failover replication status:

| Secondary | State | Lag | Last applied index | Primary index | Primary write rate |
| --- | --- | ---: | ---: | ---: | ---: |
| secondary1 | streaming | 0 | 6,913 | 6,762 | 99.77 eps |
| secondary2 | streaming | 0 | 6,929 | 6,786 | 106.50 eps |

Promotion result:

| Field | Value |
| --- | --- |
| Promoted cluster | secondary1 |
| Promotion class | forced |
| Data loss accepted | true |
| Estimated data loss entries | 0 |
| Last applied index | 7,041 |
| Last known primary index | 6,985 |
| Promotion duration | 2.09s |
| Warning | secondary state was not stable streaming (`reconciling -> reconciling`) |

Exhaustive verification on promoted secondary1:

| Keys checked | Matches | Missing | Mismatches | Errors | Uncertain missing | Result |
| ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 1,602 | 1,430 | 0 | 0 | 0 | 172 | PASS |

Interpretation:

- No confirmed-write data loss was observed.
- The 172 uncertain missing keys came from commit-uncertain writes around and
  after the hard primary stop and are not counted as confirmed data loss.
- Secondary1 wrote a post-promotion marker successfully.
- All promoted secondary1 HA nodes remained unsealed, and raft retained all
  three voters.
- The run highlights an operational semantic: a hard stop under active load can
  move a lag-0 streaming secondary into reconciling before promotion completes.
  The current guardrails correctly require forced-promotion acknowledgement in
  that state, even when the estimated missing-entry count is zero.

Promotion-contract rerun artifact:

`/Users/roelc/projects/secretz/openbao/dr-stress-results/failover-load-20260530T105445Z`

Profile:

| Metric | Value |
| --- | ---: |
| Warm-up before failover | 75s |
| Concurrency | 48 workers |
| Put mix | 70% |
| Total operations | 19,367 |
| Throughput | 120.73 ops/s |
| Successful puts | 6,608 |
| Failed puts | 6,969 |
| Successful primary gets | 1,898 |
| Failed primary gets | 1,990 |
| Successful DR status checks | 1,902 |
| Failed DR status checks | 0 |
| Dropped stress events | 0 |

Promotion result:

| Field | Value |
| --- | --- |
| Promoted cluster | secondary1 |
| Promotion ID | `5c49f1fc-8773-1d92-ca6a-a782e279544b` |
| Promotion class | forced |
| Clean promotion eligible | false |
| Clean promotion proof available | false |
| Forced acknowledgement required | true |
| Forced reason codes | `secondary_state_not_stable_streaming` |
| Forced reason details | secondary state was not stable streaming (`reconciling -> reconciling`) |
| Data loss accepted | true |
| Estimated data loss entries | 0 |
| Estimated data loss basis | `observed_primary_applied_index_gap` |
| Last applied index | 6,678 |
| Last known primary index | 6,581 |
| Promotion duration | 2.14s |

Exhaustive verification on promoted secondary1:

| Keys checked | Matches | Missing | Mismatches | Errors | Uncertain missing | Result |
| ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 1,404 | 1,350 | 0 | 0 | 0 | 54 | PASS |

Post-promotion health:

- `sys/replication/dr/status` on promoted secondary1 reported
  `mode=disabled`, `last_promotion_class=forced`,
  `last_promotion_forced_reason_codes=["secondary_state_not_stable_streaming"]`,
  `last_promotion_data_loss_accepted=true`, and
  `last_promotion_estimated_data_loss_entries_basis=observed_primary_applied_index_gap`.
- All three promoted secondary1 nodes were initialized, unsealed, and HA
  enabled after promotion.
- Promoted secondary1 Raft retained three servers and three voters.
- A post-promotion marker write on promoted secondary1 read back successfully.
- Sentinel convergence in the stress harness is intentionally not meaningful
  for this run because the old primary remained stopped after promotion.

Interpretation:

- The new promotion UX contract is visible in the real HA failover path, not
  only in unit tests.
- No confirmed-write data loss was observed.
- The 54 uncertain missing keys came from commit-uncertain writes around and
  after the hard primary stop and are not counted as confirmed data loss.
- The same operational semantic remains: hard-stopping the old primary usually
  moves a previously lag-0 streaming secondary into reconciling before the
  quiesce window completes, so forced-promotion acknowledgement is the expected
  path even when the estimated missing-entry count is zero.

## Repo-Local Promotion Semantics Matrix

Run artifact:

`/Users/roelc/projects/secretz/openbao/dr-stress-results/promotion-semantics-20260530T103251Z`

Unit coverage:

```bash
go test ./vault -run 'TestDRFailover_(FromSecondary|ForcedRequiresAcceptDataLoss|ForcedWithAcceptDataLoss|ForcedWithZeroEstimatedLossExplainsCleanProofUnavailable|ToPrimary)|TestDRFailoverToPrimary|TestDRSystemBackend_PromoteResponseExplainsForcedPromotion|TestDRSystemBackend_StatusDoesNotExposePromotionSecrets' -count=1
```

Result: PASS.

Local HA scenarios:

| Scenario | Primary state during promotion | Secondary state | Acknowledgement | Promotion class | Estimated data loss | Result |
| --- | --- | --- | --- | --- | ---: | --- |
| Online clean control | Primary still reachable | `streaming -> streaming`, index stable | not required | clean | 0 | PASS |
| Idle hard-stop | All primary nodes stopped after marker convergence | `reconciling -> reconciling` | required | forced | 0 | PASS |

Interpretation:

- The implementation can produce a clean promotion when the secondary remains
  streaming and `lastAppliedIndex` is stable across the quiesce window.
- Once the primary transport disappears, even without active user workload, the
  secondary enters reconciling before promotion completes. The current
  implementation then requires `accept_data_loss=true`.
- This makes the current clean-promotion class a narrow state: it is reachable
  only if operators have externally fenced the old primary while the secondary
  still observes stable streaming. A conventional hard-stop disaster promotes
  through the forced path, even when the estimated missing-entry count is zero.
- The forced-path result is fail-closed and defensible. The operator UX and
  RFC now make this explicit: "primary unreachable" usually implies forced
  promotion unless the system still has a stable streaming proof during the
  quiesce window. The API reports clean-promotion eligibility, proof
  availability, forced reason codes/details, acknowledgement state, and the
  estimate basis.

## Repo-Local Promotion Durability

Run:

`promoted-durability-20260529T214508Z`

Harness:

`scripts/dr_local_test.sh --topology ha promoted-durability-smoke`

Starting point:

- secondary1 had already been promoted by `failover-smoke-20260529T214044Z`.
- old primary had been restarted and remained on the old primary timeline.
- secondary2 remained a secondary of the old primary timeline.

Assertions:

- All three promoted secondary1 nodes reported `sealed=false`.
- Promoted secondary1 retained three raft voters before and after restart.
- `sys/replication/dr/status` remained `mode=disabled` with the same promotion
  ID on all promoted secondary1 nodes.
- A pre-restart promoted write remained readable after all promoted secondary1
  services were restarted and unsealed.
- A new post-restart write succeeded.
- A freshly minted old-primary activation token was rejected by the promoted
  cluster after restart with the stale promotion-lineage guard.
- `sys/step-down` moved the promoted active node from secondary1-1 to
  secondary1-2.
- A post-handoff write succeeded and was readable through all promoted
  secondary1 nodes.

## Repo-Local Explicit Reseed

Run:

`reseed-secondary-20260529T215004Z`

Harness:

`scripts/dr_local_test.sh --topology ha reseed-secondary-smoke`

Starting point:

- secondary1 was promoted and had passed the promotion durability smoke.
- old primary was still running on the old timeline.
- secondary2 was still following the old primary timeline.

Actions and assertions:

- Created a promoted-only key on secondary1 and an old-primary-only key on the
  old primary before reseed.
- Verified secondary2 initially saw the old-primary-only key and did not see
  the promoted-only key.
- Enabled DR primary mode on promoted secondary1, preserving the promotion
  record while giving it a new DR primary cluster ID.
- Disabled secondary2 from the old primary lineage.
- Generated a fresh activation token from promoted secondary1 and enabled
  secondary2 with that token.
- Waited until secondary2 reported `secondary_state=streaming`, `lag_entries=0`,
  and the same cluster ID as promoted secondary1.
- Verified secondary2 now saw promoted-only keys and no longer saw the
  old-primary-only key.
- Wrote a new key on promoted secondary1 and verified it replicated to
  secondary2 but not to the old primary.

Final topology after the run:

| Cluster | DR mode | DR cluster ID | Timeline |
| --- | --- | --- | --- |
| old primary | primary | `8dd86f5d-ac79-b027-b1b9-6d31d20f4e64` | old primary |
| promoted secondary1 | primary | `d45c3a0c-ea76-8ece-4847-550ae64284ee` | promoted authority |
| secondary2 | secondary | `d45c3a0c-ea76-8ece-4847-550ae64284ee` | promoted authority |

## Repo-Local HA Lifecycle Rerun

Run date:

`2026-05-30`

Commands:

```bash
scripts/dr_local_test.sh --topology ha reset --build
scripts/dr_local_test.sh --topology ha failover-smoke
scripts/dr_local_test.sh --topology ha promoted-durability-smoke
scripts/dr_local_test.sh --topology ha reseed-secondary-smoke
```

Reset result:

- Rebuilt `openbao:dev` from the current working tree.
- Started the HA topology with three Raft voters for primary, secondary1, and
  secondary2.
- Enabled DR primary mode and two secondary relationships.
- Both secondaries reached `secondary_state=streaming` with `lag_entries=0`.

Failover smoke:

- Run ID: `failover-smoke-20260530T100634Z`
- Promotion ID: `cdaf1790-a8af-3dc1-60a7-63ceb26a14d4`
- Promotion class: `forced`
- Data loss acknowledgement: `true`
- Estimated data loss entries: `0`
- Old primary cluster ID:
  `25bfd941-2c8c-faef-02bd-1e04b985b74f`
- Old relationship ID:
  `c91c8d41-3b93-0612-bde6-62305c0a65b3`

Assertions:

- Promotion without explicit data-loss acknowledgement was rejected.
- Promoted secondary1 retained pre-failover data and accepted a
  post-promotion write.
- A stale old-primary activation token was rejected by promoted secondary1.
- Restarted old primary accepted an old-primary-only divergent write.
- Promoted secondary1 did not see old-primary-only data.
- Old primary did not see promoted-only data.
- Secondary2 remained on the old primary lineage until explicit reseed.

Promotion durability smoke:

- Run ID: `promoted-durability-20260530T100738Z`
- Reused promotion ID `cdaf1790-a8af-3dc1-60a7-63ceb26a14d4`.
- Restarted all promoted secondary1 services and unsealed all three nodes.
- Verified promoted writes survived the full promoted-cluster restart.
- Verified stale old-primary activation tokens still failed closed after the
  restart.
- Forced an HA active handoff from `http://localhost:8902` to
  `http://localhost:8900`.
- Wrote successfully after the handoff and verified the write on all promoted
  secondary1 nodes.

Explicit reseed smoke:

- Run ID: `reseed-secondary-20260530T100820Z`
- Enabled DR primary mode on promoted secondary1.
- Disabled secondary2 from the old primary lineage.
- Issued a fresh activation token from promoted secondary1.
- Re-enabled secondary2 against promoted secondary1.
- Secondary2 reached `secondary_state=streaming` with `lag_entries=0`.
- Secondary2's DR cluster ID matched the promoted authority:
  `085ccea8-7f98-9dcc-60bd-d66b70f9248a`.
- Promoted-only data replicated to secondary2.
- Old-primary-only data was removed from secondary2 and did not merge into the
  promoted timeline.

### Promoted-Authority Reseed Cursor Fix Rerun

During the post-failover validation for `failover-load-20260530T105445Z`,
promoted durability passed after fixing the local HA harness to address the
active promoted secondary1 node directly. The subsequent reseed smoke exposed a
DR bug: secondary2 retained the old primary lineage's persisted checkpoint
high-water cursor (`6685`) and rejected the promoted authority's lower
checkpoint index (`2127`) as a rollback. It stayed in `reconciling` with
`reconcile_fail_reason_last=unknown` until the bounded wait timed out.

Fix:

- `DisableSecondary` and fresh `EnableSecondary` now clear
  `core/dr-replication/checkpoint-hwm`.
- Unit coverage:
  `go test ./vault -run 'TestDRRelationshipManager_EnableSecondaryClearsStaleCheckpointCursor' -count=1`.
- The repo-local HA harness now writes promoted-secondary durability/reseed
  checks through the detected active external address instead of assuming
  `localhost:8900` remains active after restart or stepdown.

Rebuilt-image lifecycle rerun after the fix:

- Failover smoke log:
  `/Users/roelc/projects/secretz/openbao/dr-stress-results/failover-smoke-20260530T111647Z-post-cursor-fix.log`
- Promotion ID: `5773632a-1e19-e3b2-fe61-34357d3d50c5`
- Promotion class: `forced`
- Promoted durability log:
  `/Users/roelc/projects/secretz/openbao/dr-stress-results/promoted-durability-20260530T111848Z-post-cursor-fix.log`
- Reseed log:
  `/Users/roelc/projects/secretz/openbao/dr-stress-results/reseed-secondary-20260530T111929Z-post-cursor-fix.log`
- Result: PASS.
- Secondary2 reached `secondary_state=streaming` with `lag_entries=0` after
  being re-enabled from the promoted authority.
- Final promoted primary cluster ID and secondary2 cluster ID both matched:
  `e287ee47-0759-1381-176f-b804a285f7c2`.
- Promoted-only data replicated to secondary2, and old-primary-only data did
  not merge into the promoted timeline.

### Post-DR-020 Failover-Under-Load Lifecycle Rerun

Latest post-fix artifact:

`/Users/roelc/projects/secretz/openbao/dr-stress-results/failover-load-20260530T112706Z`

Profile:

| Metric | Value |
| --- | ---: |
| Warm-up before failover | 75s |
| Concurrency | 48 workers |
| Put mix | 70% |
| Total operations | 22,430 |
| Throughput | 139.88 ops/s |
| Successful puts | 8,867 |
| Failed puts | 6,858 |
| Successful primary gets | 2,649 |
| Failed primary gets | 1,879 |
| Successful DR status checks | 2,177 |
| Failed DR status checks | 0 |
| Dropped stress events | 0 |

The high put/get failure count is expected for this profile because the workload
continues against the old primary endpoint after the old primary HA cluster is
hard-stopped. It is an availability signal for the old endpoint, not a promoted
cluster correctness failure.

Promotion result:

| Field | Value |
| --- | --- |
| Promoted cluster | secondary1 |
| Promotion ID | `4bf1dc72-2908-c1e0-9dc7-1ee302bf40c4` |
| Promotion class | forced |
| No-ack promotion | failed closed |
| Data loss accepted | true |
| Estimated data loss entries | 0 |
| Estimate basis | `observed_primary_applied_index_gap` |
| Last applied index | 8,935 |
| Last known primary index | 8,842 |
| Forced reason code | `secondary_state_not_stable_streaming` |
| Forced reason detail | `secondary state was not stable streaming (reconciling -> reconciling)` |
| Promotion duration | 2.14s |

Exhaustive verification on promoted secondary1:

| Keys checked | Matches | Missing | Mismatches | Errors | Uncertain missing | Result |
| ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 1,820 | 1,682 | 0 | 0 | 0 | 138 | PASS |

Follow-up lifecycle checks on the same promoted state:

- Promoted durability log:
  `/Users/roelc/projects/secretz/openbao/dr-stress-results/promoted-durability-20260530T113032Z-after-load-cursor-fix.log`
- Result: PASS.
- Promoted secondary1 survived full HA restart, stale old-primary token
  rejection after restart, active handoff from `http://localhost:8900` to
  `http://localhost:8902`, and post-handoff write.
- Secondary2 reseed log:
  `/Users/roelc/projects/secretz/openbao/dr-stress-results/reseed-secondary-20260530T113215Z-after-load-cursor-fix.log`
- Result: PASS.
- Secondary2 reached `secondary_state=streaming` with `lag_entries=0` against
  the promoted authority.
- Final promoted primary cluster ID and secondary2 cluster ID both matched:
  `bcfd80dc-4ae3-3f1f-0a08-70057c995024`.
- Promoted-only data replicated to secondary2, and old-primary-only data did
  not merge into the promoted timeline.

Final topology status after the lifecycle:

- Old primary remained a separate `mode=primary` timeline with cluster ID
  `606ed7c6-e20d-fd2d-3398-961ee48d7379`.
- Promoted secondary1 was `mode=primary`, retained promotion ID
  `4bf1dc72-2908-c1e0-9dc7-1ee302bf40c4`, and had one active DR relationship.
- Secondary2 was `mode=secondary` on the promoted cluster ID with
  `lag_entries=0` and `reconcile_retries_total=0`.

First-class target validation:

- Command:
  `make dr-test-ha-failover-load-lifecycle`
- Load run:
  `/Users/roelc/projects/secretz/openbao/dr-stress-results/failover-load-20260530T114237Z`
- Promoted durability log:
  `/Users/roelc/projects/secretz/openbao/dr-stress-results/promoted-durability-20260530T114539Z-after-load.log`
- Secondary2 reseed log:
  `/Users/roelc/projects/secretz/openbao/dr-stress-results/reseed-secondary-20260530T114612Z-after-load.log`
- Result: PASS.
- Workload: 24,009 ops, 9,798 successful puts, 6,963 put failures after old
  primary stop, 2,425 successful DR status checks, and 0 status failures.
- Promotion ID: `86dc9aee-9ab5-2df2-0855-85ca0475c838`.
- Promotion class: `forced`.
- No-ack promotion failed closed; accepted promotion required
  `accept_data_loss=true`.
- Estimated data loss entries: `0`, with basis
  `observed_primary_applied_index_gap`.
- Forced reason code: `secondary_state_not_stable_streaming`.
- Exhaustive promoted-secondary verification checked 1,946 keys with 1,819
  matches, 0 missing, 0 mismatches, and 0 errors.
- Promoted secondary1 survived full HA restart, stale old-primary token
  rejection, active handoff from `http://localhost:8902` to
  `http://localhost:8900`, and post-handoff write.
- Secondary2 re-seeded from the promoted authority, reached
  `secondary_state=streaming` with `lag_entries=0`, and reported the same
  promoted cluster ID as secondary1: `35eec151-264d-5793-3a6f-fee3e194081a`.

## Repo-Local Credential Rotation Under HA Load

Latest fixed run artifact:

`/Users/roelc/projects/secretz/openbao/dr-stress-results/drmixed-20260530T001551Z`

Profile:

| Metric | Value |
| --- | ---: |
| Duration | 90s |
| Concurrency | 24 workers |
| Stepdown interval | 30s |
| Total operations | 15,085 |
| Throughput | 160.17 ops/s |
| Successful puts | 8,337 |
| Failed puts | 0 |
| Successful primary gets | 3,781 |
| Failed primary gets | 0 |
| Successful DR status checks | 2,967 |
| Failed DR status checks | 0 |
| Dropped stress events | 0 |

During the workload, secondary1 rotated its DR client certificate:

| Field | Value |
| --- | --- |
| Operation ID | `3cbbea7c-33f3-9139-dbc8-9454983dec91` |
| Old fingerprint | `4bbf6443ef52d401ca8c6c46e907edff0b83a6bea750c07887229943ad1b282a` |
| New fingerprint | `33552e3401969e2bd8d4b5bda02196c53cec398b6e6435b49d0d4e32d1d2c613` |

Replication convergence:

| Secondary | Converged | Final lag | Final applied index | Primary index | Sentinel wait |
| --- | --- | ---: | ---: | ---: | ---: |
| secondary1 | yes | 0 | 16,738 | 16,737 | 1.0s |
| secondary2 | yes | 0 | 16,738 | 16,562 | 1.0s |

Exhaustive verification:

| Target | Keys checked | Matches | Missing | Mismatches | Errors | Result |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| primary | 1,781 | 1,781 | 0 | 0 | 0 | PASS |
| secondary1 | 1,781 | 1,781 | 0 | 0 | 0 | PASS |
| secondary2 | 1,781 | 1,781 | 0 | 0 | 0 | PASS |

Final DR status showed two active primary relationships and both secondaries in
`streaming` with `lag_entries=0`.

## Repo-Local Steady-State HA Soak

Latest steady-state soak artifact:

`/Users/roelc/projects/secretz/openbao/dr-stress-results/drsoak-20260530T115719Z`

Profile:

| Metric | Value |
| --- | ---: |
| Duration | 7200s |
| Concurrency | 24 workers |
| Put mix | 55% |
| Primary read mix | 25% |
| DR status mix | 20% |
| Forced stepdowns | 0 |
| Total operations | 855,321 |
| Throughput | 118.72 ops/s |
| Successful puts | 469,804 |
| Failed puts | 0 |
| Successful primary gets | 214,053 |
| Failed primary gets | 0 |
| Successful DR status checks | 171,464 |
| Failed DR status checks | 0 |
| Dropped stress events | 0 |

Replication convergence:

| Secondary | Converged | Final lag | Final applied index | Primary index | Sentinel wait |
| --- | --- | ---: | ---: | ---: | ---: |
| secondary1 | yes | 0 | 472,153 | 472,153 | 1.0s |
| secondary2 | yes | 0 | 472,153 | 472,153 | 1.0s |

Exhaustive verification:

| Target | Keys checked | Matches | Missing | Mismatches | Errors | Result |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| primary | 20,024 | 20,024 | 0 | 0 | 0 | PASS |
| secondary1 | 20,024 | 20,024 | 0 | 0 | 0 | PASS |
| secondary2 | 20,024 | 20,024 | 0 | 0 | 0 | PASS |

Interpretation:

- The tested steady-state write/read pressure is still materially above many
  normal OpenBao deployments, especially on sustained writes.
- The profile did not trigger failover, forced stepdown, promotion, or
  intentional reconnect disruption; it is a steady-state replication signal, not
  an adversarial lifecycle signal.
- No client-visible errors, backpressure rejections, dropped events, secondary
  reconcile retries, or fallback events were observed.
- Both secondaries remained in `streaming` with `lag_entries=0` through the
  final sentinel and exhaustive verification.

## Repo-Local Engine/Runtime Feature Matrix

Latest engine lifecycle matrix artifact:

`/Users/roelc/projects/secretz/openbao/dr-stress-results/engine-matrix-20260530161139`

Command:

```bash
scripts/dr_local_test.sh --topology ha engine-lifecycle-matrix
```

Coverage:

| Area | Assertion |
| --- | --- |
| KV v2 | Versioned application config replicated and read on both secondaries. |
| KV v1 | Non-versioned application config replicated and read on both secondaries. |
| Namespaces | Namespace metadata, namespace `sys/mounts`, and namespace-scoped KV v2 state replicated and readable on both secondaries. |
| Transit | Replicated transit mount and AES-GCM key metadata readable on both secondaries; encrypt/decrypt round trip verified on primary before replication checks. |
| PKI | Replicated PKI mount, generated CA, role, and issued certificate readable on both secondaries. |
| SSH | Replicated SSH CA public key and CA role state readable on both secondaries. |
| TOTP | Replicated TOTP key metadata readable on both secondaries. |
| Database | Replicated database config readable on both secondaries with connection verification disabled for a self-contained local test. |
| Auth mounts | Userpass user state, AppRole role ID, cert auth config, JWT config/role, and token role state replicated and readable on both secondaries. |
| ACL policy | Run-specific policy body replicated and visible on both secondaries. |
| Service token | Primary-created service token can authenticate on both secondaries and self-report the replicated policy. |
| Identity | Entity, group membership, and entity alias replicated and readable on both secondaries. |

Lifecycle checks:

| Check | Result |
| --- | --- |
| Pre-failover matrix on primary, secondary1, and secondary2 | PASS |
| Matrix on promoted secondary1 after forced failover | PASS |
| Matrix on secondary2 after explicit reseed from promoted authority | PASS |

Result: PASS.

Important findings from the expanded matrix:

- PKI exposed a route-backed backend cache gap. DR apply writes below the
  barrier, so plugin-backed storage paths did not receive the normal local
  invalidation signal. Replicated storage apply now invalidates affected
  route-backed backend keys after commit/refresh.
- Identity exposed an over-protected singleton route. The secondary correctly
  preserves local token/sys/cubbyhole routes, but identity is replicated DR
  state. Runtime refresh now remounts the identity route from the replicated
  mount table when the router still points at the old local UUID/accessor, and
  updates `core.identityStore`.
- Identity artifacts also need explicit reload after runtime refresh. A
  secondary can have the correct identity route and durable replicated entries
  while serving stale or empty in-memory identity state. Runtime refresh now
  registers missing namespace views, resets the identity memdb, and reloads
  entities, groups, aliases, and OIDC clients in read-only mode.
- Namespace lifecycle validation exposed two related runtime refresh gaps.
  Replicated namespace records must reload before replicated mount/auth tables,
  and namespace-scoped singleton routes such as `sys/`, `token/`, and
  `identity/` must be refreshed for child namespaces even though root singleton
  routes remain protected. The fix avoids generic invalidation for
  `core/namespaces/*` and relies on runtime refresh instead.
- HA host-side arbitrary-token lookup against a DR secondary redirects to the
  configured container `api_addr`. The matrix now checks service-token self
  lookup instead, which directly validates that replicated tokens are usable on
  secondaries without depending on host DNS for container leader addresses.

## Run Summary

| Run | Profile | Result | Notes |
| --- | --- | --- | --- |
| `drmixed-20260529T155609Z` | 2m direct, no stepdown | Not a correctness signal | Early run exposed stress-runner deadline accounting. Put failures were `ctx_deadline` at natural shutdown. |
| `drmixed-20260529T162728Z` | 2m direct, no stepdown | Not a correctness signal | Reproduced the same shutdown-deadline false failure pattern. |
| `drmixed-20260529T182620Z` | 2m direct, no stepdown | Not a correctness signal | Reproduced the same shutdown-deadline false failure pattern. |
| `drreconcile-20260529T182522Z` | Runtime churn/reconcile | PASS | Reconcile preserved runtime mount/auth changes and removed stale state. |
| `drmixed-deadline-check-20260529T183057Z` | 30s direct, no stepdown | PASS | Post-fix harness check: 5,436 ops, 0 put/get/status failures, sentinel convergence around 1s. |
| `drmixed-30m-stepdown-20260529T183352Z` | 30m direct-node with stepdowns | Correctness PASS, availability degraded | 273,872 ops; 402 put failures, 339 get failures, 0 status failures. Both secondaries converged with lag 0 and sampled verification passed. Failures clustered around active handoff. |
| `drmixed-15m-proxy-stepdown-20260529T191508Z` | 15m HAProxy with stepdowns | Correctness PASS, availability degraded | 141,696 ops; 443 put failures, 62 get failures, 0 status failures. Exhaustive verification passed on all clusters. |
| `failover-load-20260529T210509Z` | Repo-local HA failover under load | Confirmed-write correctness PASS, promoted HA degraded | 11,290 ops; clean promotion with estimated data loss 0; exhaustive verification passed on promoted secondary1. Standby nodes in the promoted cluster sealed during key transition (`DR-007`). |
| `failover-load-fixed-20260529T212724Z` | Repo-local HA failover under load after `DR-007` fix | PASS | 11,167 ops; clean promotion with estimated data loss 0; exhaustive verification passed on promoted secondary1; all promoted secondary1 HA nodes stayed unsealed with three raft voters. |
| `failover-load-20260530T102316Z` | Repo-local HA failover under load on rebuilt image | Confirmed-write correctness PASS, forced promotion | 12,575 ops; secondary1 was streaming with lag 0 before primary stop, then reconciling during promotion quiesce; forced promotion with estimated data loss 0; exhaustive verification passed on promoted secondary1; all promoted secondary1 HA nodes stayed unsealed with three raft voters. |
| `failover-load-20260530T105445Z` | Repo-local HA failover under load with promotion UX contract | PASS | 19,367 ops; no-ack promotion failed closed; accepted forced promotion returned clean-proof fields, reason code/details, acknowledgement state, and estimate basis; exhaustive verification passed on promoted secondary1; all promoted secondary1 HA nodes stayed unsealed with three Raft voters. |
| `failover-smoke-20260530T111647Z` | Repo-local HA forced failover after `DR-020` fix | PASS | Rebuilt image after checkpoint-cursor reset fix; forced promotion required acknowledgement, stale old-primary token failed closed, and promoted/old-primary timelines stayed isolated. |
| `promoted-durability-20260530T111848Z` | Repo-local promoted HA durability after active-address harness fix | PASS | Promoted secondary1 survived full restart, stale old-primary token rejection after restart, active handoff from `http://localhost:8900` to `http://localhost:8902`, and post-handoff write. |
| `reseed-secondary-20260530T111929Z` | Repo-local explicit secondary2 reseed after `DR-020` fix | PASS | Secondary2 cleared the old lineage checkpoint cursor, re-enabled from a fresh promoted-authority token, reached streaming with lag 0, replicated promoted-only data, and did not merge old-primary-only data. |
| `failover-load-20260530T112706Z` | Repo-local HA failover under load after `DR-020` fix | PASS | 22,430 ops; no-ack promotion failed closed; accepted forced promotion reported reason code/details and estimate basis; exhaustive verification passed on promoted secondary1 with 0 confirmed missing keys and 0 mismatches. |
| `promoted-durability-20260530T113032Z` | Repo-local promoted HA durability from load-generated promoted state | PASS | Promoted secondary1 survived full restart, stale old-primary token rejection after restart, active handoff from `http://localhost:8900` to `http://localhost:8902`, and post-handoff write. |
| `reseed-secondary-20260530T113215Z` | Repo-local explicit secondary2 reseed from load-generated promoted state | PASS | Secondary2 re-enabled from the promoted authority, reached streaming with lag 0, replicated promoted-only data, and did not merge old-primary-only data. |
| `failover-load-20260530T114237Z` | First-class `dr-test-ha-failover-load-lifecycle` target | PASS | 24,009 ops; no-ack promotion failed closed; accepted forced promotion reported reason code/details and estimate basis; exhaustive verification passed on promoted secondary1 with 0 confirmed missing keys and 0 mismatches; promoted durability and secondary2 reseed passed from the same load-generated promoted state. |
| `drsoak-20260530T115719Z` | Repo-local 2h HA steady-state soak | PASS | 855,321 ops; 469,804 successful puts, 214,053 successful gets, 171,464 successful DR status checks, 0 failures, 0 dropped events; both secondaries converged in about 1s; exhaustive verification passed on primary, secondary1, and secondary2 for all 20,024 terminal keys. |
| `engine-matrix-20260530144744` | Repo-local HA engine/runtime feature matrix | PASS | Verified KV v2, transit, PKI CA/role/cert, userpass, AppRole, ACL policy, service-token self lookup, and identity entity/group/alias state on primary, secondary1, and secondary2. |
| `engine-matrix-20260530152338` | Repo-local HA engine/runtime lifecycle matrix | PASS | Verified namespace metadata, namespace-scoped KV v2, root KV v2, transit, PKI, userpass, AppRole, ACL policy, service-token self lookup, and identity on primary/secondaries before failover; reverified on promoted secondary1 after forced failover; reverified on secondary2 after promoted-authority reseed. |
| `engine-matrix-20260530161139` | Broadened repo-local HA engine/runtime lifecycle matrix | PASS | Verified the self-contained parity set: KV v2, KV v1, transit, PKI, SSH, TOTP, database config, userpass, AppRole, cert auth, JWT auth, token role, ACL policy, service-token self lookup, namespace metadata/KV, and identity entity/group/alias before failover, after forced promotion, and after promoted-authority reseed. |
| `promoted-durability-20260529T214508Z` | Repo-local promoted HA durability smoke | PASS | Full promoted secondary1 restart, stale old-primary token rejection after restart, active handoff from secondary1-1 to secondary1-2, and post-handoff write all passed. |
| `reseed-secondary-20260529T215004Z` | Repo-local explicit secondary2 reseed to promoted authority | PASS | Promoted secondary1 became the new DR primary, secondary2 was re-enabled from a fresh promoted token, promoted-only data replicated, old-primary-only data was removed from secondary2, and old primary stayed separate. |
| `drmixed-20260530T001551Z` | Repo-local HA credential rotation under load | PASS | 15,085 ops, 0 workload failures, secondary1 DR client certificate rotation during load, forced stepdowns every 30s, sentinel convergence in 1s for both secondaries, exhaustive verification passed on all clusters. |
| `failover-smoke-20260530T100634Z` | Repo-local HA forced failover smoke on rebuilt image | PASS | Reset rebuilt `openbao:dev`; forced promotion required explicit acknowledgement, stale old-primary token was rejected, and promoted/old-primary timelines stayed isolated. |
| `promoted-durability-20260530T100738Z` | Repo-local promoted HA durability rerun on rebuilt image | PASS | Promoted secondary1 survived full restart, rejected stale old-primary tokens after restart, moved active from `http://localhost:8902` to `http://localhost:8900`, and accepted post-handoff writes. |
| `reseed-secondary-20260530T100820Z` | Repo-local explicit secondary2 reseed rerun on rebuilt image | PASS | Promoted secondary1 became the new DR primary, secondary2 was re-enabled from a fresh promoted token, secondary2 converged with lag 0, promoted-only data replicated, and old-primary-only data did not merge. |

There is also an older historical artifact,
`drmixed-20260215T150225Z`, from the pre-current environment. It is useful as
background but should not be treated as signoff evidence for the current design.

## Availability Findings

The stress tests produced transient client-visible errors during active handoff:

- Direct-node 30-minute run:
  - 402 failed puts
  - 339 failed primary gets
  - 0 failed status checks
- HAProxy 15-minute run:
  - 443 failed puts
  - 62 failed primary gets
  - 0 failed status checks

The HAProxy run showed two clear failure bursts:

- Around `21:21`: 288 put `503`s and 18 get `500`s.
- Around `21:26`: 91 put `503`s, 64 put `500`s, 41 get `500`s, and 3
  transport errors.

Primary cluster logs around those windows showed active handoff and forwarding
errors such as forwarded RPC failures and TLS internal errors. This suggests an
HA availability problem under forced leadership movement and sustained write
pressure, not a DR data-correctness failure.

Tracked issue:

- `HA-005`: baseline HA/forwarding availability during active handoff under DR
  stress.

## Correctness Assessment

The current design direction looks sound for data correctness:

- The ordered stream path works while secondaries remain inside the replay
  horizon.
- A 2-hour steady-state HA soak sustained materially high write/read pressure
  with zero client-visible failures, no dropped stress events, no secondary
  fallback/reconcile churn, and exhaustive terminal-key matches on both
  secondaries.
- When pressure exceeds the stream buffer horizon, reconciliation is able to
  recover secondaries back to a matching terminal state.
- The range-first reconciliation path did not produce silent omissions in the
  observed stress runs.
- Secondaries returned to `streaming` with lag 0 after the final sentinel.
- Exhaustive verification confirmed all terminal values on both secondaries.
- The engine/runtime matrix now verifies replicated non-KV runtime surfaces,
  including namespaces, namespace-scoped KV state, route-backed PKI state,
  transit key metadata, auth mount state, ACL policies, service-token
  authentication, and identity objects.

The important distinction is:

- **Correctness**: strong signal, no observed final data divergence.
- **Availability**: not clean under adversarial handoff pressure.

## Known Fixed Issues Found During Validation

The validation process found and fixed several issues that should remain split
from the DR protocol work where possible:

- DR secondary certificate stability across restarts.
- Runtime refresh after bootstrap, stream apply, and reconciliation.
- Cluster information bootstrap after protected local state is purged.
- Protected local singleton handling.
- Reconciliation correctness for range fetches and deletes.
- Baseline HA state-lock timeout writer queuing.
- Read-only standby policy store behavior.
- Auto-seal initialization redundant unseal hang.
- HA initialization lock timeout goroutine leak.
- Stress-runner false failures caused by canceling in-flight operations at
  natural workload end.
- Promoted HA secondary recovery across the DR promotion key transition.
- Promotion-lineage fencing for stale old-primary activation tokens.
- Promoted HA secondary durability across full cluster restart and active
  handoff.
- Explicit reseed of a non-promoted secondary to the promoted authority without
  merging old-primary-only writes.
- Primary API address normalization for TLS-disabled repo-local DR bootstrap
  and credential rotation tests.
- Secondary HA reconnect candidate rotation after stale leader hints.
- Bootstrap token verifier storage so pending relationships do not persist
  plaintext bootstrap bearer tokens.
- Endpoint and audit boundary regression coverage for the exact
  unauthenticated DR HTTP surface, root-protected admin paths, sensitive schema
  metadata, and default audit HMAC redaction of bootstrap and rotation material.
- Response disclosure boundary regression coverage for unauthenticated DR
  status and root-protected relationship list/status responses, including
  stored config secrets, stale promotion lineage identifiers, bootstrap
  verifier material, certificate bytes, pending/previous fingerprints, and
  pending rotation operation IDs.
- Unauthenticated error-oracle hardening for bootstrap registration and
  credential rotation. Public failures are generic after request-shape
  validation, while detailed causes remain in logs and root-protected
  relationship status.
- DR gRPC data-plane authorization coverage for StreamChanges,
  RequestCheckpoint, Heartbeat, ExchangeDirtyBitmap, checkpoint-scoped
  checksum/digest/fetch RPCs, and SyncKeyring. RPCs deny missing peer identity,
  mismatched peer fingerprints, mismatched relationships, and revoked
  relationships where applicable. Checkpoint-scoped RPC requests now include
  `relationship_id`; the primary authorizes it before checkpoint lookup and
  then requires the checkpoint's persisted relationship ID to match.
- SyncKeyring crypto-binding hardening. Malformed keyring-sync requests no
  longer activate a relationship, successful sync is the only transition to
  active, replay after activation is denied, and tests unwrap the primary
  response with relationship ID, DR cluster ID, secondary certificate
  fingerprint, primary identity, AAD version, and nonce bindings.
- Unauthenticated resource-exhaustion hardening for credential rotation.
  Rotation now rejects inactive relationships and missing pending confirmations
  before parsing attacker-supplied certificate material, and uses stored
  current or pending certificate material for signature verification where
  possible while preserving the normal two-phase rotation path.
- Revocation fail-closed hardening for primary RPCs. `SyncKeyring` now denies
  if revocation lands during root-key wrapping, and long `FetchEntries` streams
  re-check authorization before continuing after a revoke. Checkpoint, range,
  dirty-bitmap, and heartbeat responses also re-check relationship state before
  returning. Pending credential rotations are denied after revoke, and cached
  current/pending relationship trust is removed.
- Promotion lineage inheritance across repeated failover cycles. Promotion
  records now carry inherited stale primary cluster IDs, relationship IDs, and
  secondary certificate fingerprints, so promote -> reseed -> promote does not
  forget older stale authorities.
- Promoted-authority reseed cursor reset. Fresh secondary activation now clears
  old secondary checkpoint high-water state so explicit reseed from a promoted
  authority can reconcile even when the promoted authority's checkpoint index is
  lower than the old primary lineage's cursor.
- Promotion UX hardening for failover. Promotion responses and persisted
  records now distinguish clean eligibility from zero estimated loss, return
  stable forced-promotion reason codes/details, record data-loss
  acknowledgement state, and preserve the estimate basis for status/audit.
- Stress-runner sentinel write retry during transient HA active handoff.
- Route-backed backend cache invalidation for DR-applied replicated storage.
  PKI exposed that replicated writes applied below the barrier did not notify
  plugin backend caches; apply paths now invalidate affected route-backed keys.
- Identity singleton runtime refresh. The secondary still protects local
  sys/token/cubbyhole routes, but identity is replicated DR state and now gets
  replaced from the replicated mount table during runtime refresh. Runtime
  refresh also handles the case where the in-memory mount table already has the
  primary identity entry but the router still points at the old local route.
- Identity artifact reload after DR runtime refresh. Identity entities, groups,
  aliases, and OIDC clients are cached in namespace-scoped memdb state; runtime
  refresh now resets and reloads those artifacts from replicated storage after
  namespace and route state are refreshed.
- Namespace runtime refresh. Namespace records are replicated DR state and must
  reload before mount/auth tables. Namespace-scoped singleton routes are
  refreshed for child namespaces while root singleton routes stay protected.
- Fetch/reconcile resource bounds. `FetchEntries` response batches now have an
  explicit byte budget, oversized single entries fail closed, and range
  reconciliation accounts fetched value bytes against its RPC budget.
- Checkpoint build admission. Same-relationship checkpoint requests reuse the
  in-flight build result, while cross-relationship checkpoint build concurrency
  is globally bounded and rejected with retryable resource pressure once full.
- DR tuning validation. Invalid API and manager-side tuning changes now fail
  before persistence or runtime apply, signed API integers cannot wrap into
  unsigned byte/entry budgets, and rejected updates roll back without partial
  in-memory mutation.

See `DR_BUG_TRACKER.md` for suggested worktree split planning.

## Recommended Next Steps

1. Keep DR correctness and HA availability as separate work streams.
2. Add an automated exhaustive verifier target for stress results.
3. Add opt-in dependency-backed engine profiles for LDAP, Kubernetes, RADIUS,
   Kerberos, RabbitMQ, and live database credential issuance. The default
   matrix now covers the self-contained built-in parity set.
4. Reproduce `HA-005` in a smaller HA-only or DR-light scenario to determine
   whether the active handoff availability issue is baseline HA behavior,
   HAProxy health-check behavior, DR backlog pressure, or a combination.
5. Add a stress profile with production-like load: lower write ratio, lower
   concurrency, no forced stepdowns, and occasional restart/reconnect events.
6. Preserve all run artifacts before splitting the PoC into reviewable
   worktrees.

## Internal Talking Points

- The PoC is no longer only passing narrow unit/integration tests; it has
  survived adversarial multi-cluster stress without final data divergence.
- The most compelling result is exhaustive terminal-key verification after the
  HAProxy stepdown run.
- The design should continue on the current reconciliation-first correctness
  path.
- The largest open risk is operational availability around active handoff under
  high write pressure, not replicated data correctness.
- Failover data correctness looks defensible when the secondary is caught up,
  and the latest repo-local rerun shows promoted-cluster HA recovery working
  under the tested load profile.
- Longer tests are useful, but the next high-value work is isolating the HA
  availability failure so it does not obscure DR protocol validation.
