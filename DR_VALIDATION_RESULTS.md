# DR Replication Validation Results

This document is the curated validation evidence index for the local DR
replication prototype. It intentionally separates stable artifact structure from
claims about current behavior.

Validation claims are based on selected scenario runs, not on whichever artifact
is newest in `dr-stress-results/`.

## Evidence Policy

- Use [DR_TEST_MATRIX.md](DR_TEST_MATRIX.md) for the scenario catalog and pass
  criteria.
- Use [DR_VALIDATION_RUNS.json](DR_VALIDATION_RUNS.json) as the machine-readable
  manifest of selected runs.
- Treat unlisted `dr-stress-results/` directories as historical scratch data
  unless they are promoted into the manifest.
- Treat client-facing PUT/GET failures during forced HA disruption or hard
  primary loss as availability signals, not replicated-data correctness
  failures, unless verification shows missing/mismatched data.
- Do not use old proof-mismatch fallback runs as current indexed-repair evidence.
  They remain useful regression context only.

## Artifact Schema

Each `dr-stress` workload run is expected to emit:

| Artifact | Purpose |
|---|---|
| `config.json` | Workload configuration, topology endpoints, seed, and runtime metadata. |
| `result.json` | Normalized run summary, operation counts, sentinel convergence, and before/after DR status snapshots. |
| `events.ndjson` | Per-operation event ledger, including control events such as stepdown. |
| `writes.ndjson` | Truth ledger for terminal key/value verification. |
| `status_timeline.ndjson` | Periodic DR status snapshots for lag, buffer, reconciliation, and throughput analysis. |
| `progress.log` | Human-readable workload progress stream. |
| `verify-*.json` | Independent verification output. Primary/promoted clusters use API reads; strict secondaries use checkpoint verification. |

Orchestrated smokes add scenario-specific files such as
`orchestrator.log`, `before-*-status.json`, `after-*-status.json`,
`final-*-status.json`, promotion summaries, and restart logs.

## Curated Runs

| Scenario | Run | Result | Evidence Summary |
|---|---|---|---|
| HA steady-state soak | `drsoak-20260530T115719Z` | Pass | 7,200s, 24 workers, 855,321 operations, zero PUT/GET/status failures, zero dropped events, 1s/1s sentinel convergence, exhaustive verification passed on all three clusters across 20,024 truth-log keys. |
| HA mixed load with primary handoff | `drmixed-20260531T214747Z` | Pass with expected client transients | 900s, 48 workers, primary stepdown every 300s, 88,023 operations, zero status failures, zero dropped events, 1s/1s sentinel convergence, exhaustive verification passed on all three clusters across 7,744 truth-log keys. PUT/GET failures were client-facing HA disruption noise. |
| HA mixed load after key-only KID index | `drmixed-20260531T231905Z` | Pass with expected client transients | 900s, 48 workers, primary stepdown every 300s, 133,318 operations, zero status failures, zero dropped events, 1s/1s sentinel convergence, exhaustive verification passed on all three clusters across 10,430 truth-log keys. Indexed repair loaded 558/298 ranges with zero fallback scans or proof mismatches. |
| HA mixed load after adaptive stream batching | `drmixed-20260531T235152Z` | Pass with expected client transients and one optimizer fallback | 900s, 48 workers, primary stepdown every 300s, 126,106 operations, zero status failures, zero dropped events, 1s/1s sentinel convergence, exhaustive verification passed on all three clusters across 10,184 truth-log keys. Stream transaction count dropped by roughly 55%, but secondary #1 hit one `bucket_mismatch` local KID-index fallback scan during reconciliation. This is now regression context rather than the current optimizer result. |
| HA mixed load after adaptive batching and standby key-transition deferral | `drmixed-20260601T114049Z` | Pass for HA convergence and optimizer behavior; secondary API verification blocked | 900s, 48 workers, primary stepdown every 300s, 111,169 operations, zero status failures, zero dropped events, 1s/1s sentinel convergence, both secondaries ended streaming with lag 0, both reconciled through indexed repair over 1,024 ranges, zero local KID fallback scans, zero indexed proof mismatches, and standby keyring-missing transitions deferred without sealed/fatal logs. Primary API verification passed across 9,192 truth-log keys. Secondary API verification returned `transaction is read-only` for all sampled keys, which is expected under strict warm-standby semantics and should be replaced by checkpoint verification in new runs. |
| Indexed repair beyond journal horizon | `indexed-repair-20260531T224314Z` | Pass with expected client transients | Forced secondary #1 outage beyond journal horizon. Primary `journal_range_too_old_total=2`; secondary #1 reached indexed repair total 2, 1,825 indexed ranges, 1,773 bucket loads, and 9,867 loaded KID-index entries with zero fallback scans, proof mismatches, full-bucket fallback, scan failures, or load failures. Exhaustive verification passed on all three clusters across 3,261 truth-log keys. |
| Secondary outage within journal horizon | `secondary-outage-20260531T211438Z` | Pass with expected client transients | 120s, 32 workers, secondary #1 stopped for 40s and recovered via journal replay. No reconciliation, no fallback scan, no journal-too-old increment, zero PUT/status failures, zero dropped events, and exhaustive verification passed on all three clusters across 2,143 truth-log keys. |
| Accumulator cold restart | `accumulator-cold-restart-20260531T205052Z` | Pass | Full secondary #1 restart restored `flat_accumulator_cursor_index=56113` and `flat_accumulator_snapshot_index=56113`, kept `reconcile_count=0`, and observed no scan/fallback/proof-mismatch counters. |
| 100k fixture pre-seed with journal catch-up | `preseed-smoke-20260601T225805Z` | Pass | Single-node topology restored the reusable `primary-100k` fixture, exported 63,346 checkpoint-bound entries across 54 segments at checkpoint 100,047, wrote 20,000 post-export keys with 64-way concurrency, enabled secondary #2 from the accepted baseline, skipped initial reconciliation, replayed the retained journal from the pre-seed cursor, and passed checkpoint verification at index 120,070. Secondary #2 ended `streaming` with `lag_entries=0`, `reconcile_count=0`, zero indexed repair, zero local KID-index fallback scans, zero scan failures, and verification `physical_scan_used=false` / `optimizer_reseeded=false`. |
| HA pre-seed with journal catch-up and post-accept handoff | `preseed-smoke-20260601T231801Z` | Pass | HA topology exported 151 checkpoint-bound entries across 8 segments at checkpoint 113, wrote 5,000 post-export keys with 64-way concurrency while importing the bundle, enabled secondary #2 from the accepted baseline, skipped initial reconciliation, replayed the retained journal to checkpoint 5,127, and passed checkpoint verification with `physical_scan_used=false` / `optimizer_reseeded=false`. The smoke then forced primary and secondary #2 active handoff, after which secondary #2 returned to `streaming` with `lag_entries=0`, `reconcile_count=0`, zero indexed repair, zero local KID-index fallback scans, zero scan failures, and post-handoff checkpoint verification passed at index 5,135 with no physical-scan fallback. |
| Go-harness HA pre-seed wrapper parity | `preseed-smoke-20260601T234900Z` | Pass | The new `scripts/dr-harness` implementation drove the existing `scripts/dr_local_test.sh --topology ha preseed-smoke` command through a thin wrapper. It exported 151 checkpoint-bound entries across 8 segments at checkpoint 113, wrote 500 post-export keys with 16-way Go-native concurrency, enabled secondary #2 from the accepted baseline, skipped initial reconciliation, replayed to checkpoint 618, and passed checkpoint verification with `physical_scan_used=false` / `optimizer_reseeded=false`. After primary and secondary #2 active handoff, checkpoint verification passed again at index 626 with no physical-scan fallback. This run is primarily harness parity evidence. |
| Go-harness HA quiescent reconnect parity | `quiescent-reconnect-20260602T001152Z` | Pass | The active shell entrypoint ran through `scripts/dr-harness`, forced primary active handoff from `http://localhost:8800` to `http://localhost:8802`, and both secondaries stayed streaming with lag 0 and no reconciliation count increase. This is harness parity evidence. |
| Go-harness HA accumulator cold restart parity | `accumulator-cold-restart-20260602T001327Z` | Pass | The active shell entrypoint ran through `scripts/dr-harness`, restarted all secondary #1 HA nodes, restored `flat_accumulator_cursor_index=66` and `flat_accumulator_snapshot_index=66`, and observed no new reconciliation, scan failures, or local KID fallback scans. This is harness parity evidence. |
| Go-harness HA secondary outage within horizon parity | `secondary-outage-20260602T001401Z` | Pass | The active shell entrypoint ran through `scripts/dr-harness`, stopped secondary #1 for 20s during a 75s workload, followed the new active secondary node after restart, recovered through journal replay without reconciliation or `journal_range_too_old`, and passed terminal checkpoint verification. This is harness parity evidence, not a throughput benchmark. |
| Go-harness HA indexed repair/out-of-horizon parity | `indexed-repair-20260602T001934Z` | Pass | The active `indexed-repair-smoke` entrypoint ran through `scripts/dr-harness`, stopped secondary #1 beyond the shrunken journal horizon, incremented primary `journal_range_too_old_total`, ran indexed repair without local KID fallback scans, and passed terminal checkpoint verification. The harness retries only transient accumulator/checkpoint index skew during checkpoint verification. |
| Clustered HA fragmented reconnect after S20/S21 observability | `indexed-repair-20260602T122921Z` | Pass | 180s, 48 workers, secondary #1 outage beyond journal horizon. Primary reported `journal_range_too_old_total=3`, `range_checksum_requests_total=55`, `range_digest_requests_total=21219`, `fetch_requests_total=2745`, and zero range/fetch rejections or scan failures. Secondary #1 returned to `streaming` with `lag_entries=0`, `reconcile_count=3`, `flat_accumulator_indexed_repair_total=3`, 2,715 indexed repair ranges, zero local KID fallback scans, zero full-bucket fallback, zero indexed proof mismatches, zero reconcile-budget exhaustion, and strict checkpoint verification passed on both secondaries with 1,024/1,024 matched ranges and `physical_scan_used=false`. |
| Clustered HA fragmented fanout cap | `fragmented-fanout-20260602T134143Z` | Pass with expected client transients | 180s, 48 workers, secondary #1 outage beyond journal horizon with `reconcile_max_range_drilldown_rpcs=1` only on secondary #1. Primary `journal_range_too_old_total` increased by 3; primary range-digest requests increased by 2,810 instead of the 17,740-request delta seen in the earlier default-cap smoke. Secondary #1 returned to `streaming` with `lag_entries=0`, `reconcile_count=3`, indexed repair total 3, 2,810 indexed repair ranges, 2,730 coarse fetches covering 4,234 ranges, zero local KID fallback scans, zero full-bucket fallback, zero indexed proof mismatches, zero scan failures, and strict checkpoint verification passed on both secondaries with 1,024/1,024 matched ranges and `physical_scan_used=false`. |
| Clustered HA reconcile budget pressure | `reconcile-budget-20260602T130034Z` | Pass | 120s, 32 workers, secondary #1 outage beyond journal horizon with `reconcile_max_rpc_bytes=1` only on secondary #1. The secondary hit `budget_exhausted` six times in `range_checksums` for `rpc_bytes`, reported `reconcile_fail_reason_last=budget_exceeded`, remained `reconciling` with lag instead of falsely completing repair, and had zero scan failures. The primary reported `journal_range_too_old_total=1`, range-checksum requests increased from 22 to 39, range-digest requests from 32 to 42, fetch requests from 32 to 33, and all primary rejection counters stayed zero. Primary API verification passed across 2,540 truth-log keys; secondary #2 checkpoint verification passed with 1,024/1,024 matched ranges and `physical_scan_used=false`. Secondary #1 verification was intentionally skipped because failure to converge is the expected fail-closed behavior. |
| Go-harness HA mixed-load smoke parity | `drmixed-20260602T055824Z` | Pass | The active `smoke` entrypoint ran through `scripts/dr-harness`, applied the constrained HA primary tuning profile, delegated the workload body to `dr-stress`, and completed a shortened 45s, 8-worker mixed workload with 5,475 operations, zero PUT/GET/status failures, zero dropped events, and 1s/1s sentinel convergence. Terminal verification passed primary API checks across 820 truth-log keys and active-secondary checkpoint verification on both secondaries with 1,024/1,024 matched ranges, zero missing/mismatched ranges, `physical_scan_used=false`, and `optimizer_reseeded=false`. This is harness parity evidence, not a throughput benchmark. |
| Go-harness HA full mixed-load smoke | `drmixed-20260602T075024Z` | Pass with expected client transients | 900s, 48 workers, primary stepdown every 300s, constrained HA tuning, 79,053 operations, zero status failures, zero dropped events, and 1.0s/1.0s sentinel convergence. Both secondaries briefly entered reconciliation under saturated-ring pressure, returned to `streaming`, and ended with `lag_entries=0`. Terminal primary API verification passed across 7,094 truth-log keys, and both strict secondaries passed checkpoint verification with 1,024/1,024 matched ranges, zero missing/mismatched ranges, `physical_scan_used=false`, and `optimizer_reseeded=false`. Indexed repair ran without local KID fallback scans, full-bucket fallback, proof mismatches, load failures, or scan failures. PUT/GET failures were client-facing HA disruption noise. |
| Go-harness HA dynamic tuning load | `tuning-ha-load-20260602T071619Z` | Pass with expected client transients | 900s, 36 workers, primary stepdown every 90s, constrained tuning at 60s, relaxed tuning at 180s, forced primary and secondary active handoff, 86,564 operations, zero status failures, zero dropped events, and sentinel convergence in 1.0s/59.0s. Terminal primary API verification passed across 7,663 truth-log keys, and both strict secondaries passed checkpoint verification with 1,024/1,024 matched ranges, zero missing/mismatched ranges, `physical_scan_used=false`, and `optimizer_reseeded=false`. The relaxed primary tuning write collided with HA active movement and succeeded after harness retry. PUT/GET failures were client-facing HA disruption noise. |
| Quiescent reconnect | `quiescent-reconnect-20260531T204826Z` | Pass | Forced primary active handoff from `http://localhost:8800` to `http://localhost:8802`; both secondaries remained streaming with lag 0 and no reconciliation count increase. |
| Fixture-backed async pre-seed smoke | `preseed-smoke-20260601T195421Z` | Pass | Single-node topology restored a reusable primary dataset fixture, configured fresh secondaries, created a fresh relationship, completed async segmented export planning, exported and staged 281 checkpoint-bound entries across 16 segments, wrote 64 post-export keys, enabled secondary #2 from checkpoint 178, and passed checkpoint verification at index 248 with streaming lag 0. |
| Segmented pre-seed smoke | `preseed-smoke-20260601T184840Z` | Pass | Single-node topology wrote 64 seed keys, created a segmented pre-seed export plan for a fresh relationship, exported and staged 151 checkpoint-bound entries across 7 segments, completed import on disabled secondary #2, enabled from checkpoint 110, replayed the post-export delta, and passed checkpoint verification at index 113 with zero missing or mismatched ranges. Secondary #2 ended `streaming` with `lag_entries=0`, `last_applied_index=113`, zero scan failures, and zero local KID-index fallback scans. |
| HA pre-seed with post-accept handoff | `preseed-smoke-20260601T181124Z` | Pass | HA topology exported a checkpoint-bound pre-seed bundle for a fresh relationship, disabled secondary #2, imported the bundle, enabled the secondary from the accepted baseline, replayed the post-export delta, and passed checkpoint verification at index 52. The smoke then forced primary and secondary #2 active handoff, after which secondary #2 returned to `streaming` with `lag_entries=0`, `last_applied_index=58`, `primary_index=58`, zero scan failures, zero local KID-index fallback scans, and post-handoff checkpoint verification passed with zero missing or mismatched ranges. |
| Engine/runtime lifecycle matrix | `engine-matrix-20260530161139` | Pass | Covered namespaces, KV v1/v2, transit, PKI, SSH, TOTP, database, userpass, AppRole, cert, JWT, token roles, policies, and identity across pre-failover, promoted secondary, and reseeded secondary phases. |
| Failover under load lifecycle | `failover-load-20260530T114237Z` | Pass with expected primary loss | Hard-stopped old primary mid-run, required explicit forced-promotion acknowledgement, verified promoted secondary truth log with zero confirmed missing/mismatched keys, then passed promoted durability and promoted-authority reseed follow-up smokes. Client PUT/GET failures are expected after the old authority is removed. |

## Current Interpretation

The curated set supports these current claims:

- Streaming, replay, reconciliation, and checkpoint repair preserve terminal data
  correctness in the tested local HA topology.
- The indexed-repair path is now covered by a targeted out-of-horizon smoke,
  rather than inferred from generic HA stepdown load.
- The latest clustered fragmented-reconnect smoke exercised the indexed-repair
  path after S20/S21 observability landed. It did not exhaust resource budgets,
  but it confirmed the pressure counters are visible during a real HA reconnect
  and that the successful repair path stayed scan-free.
- The clustered fragmented-fanout smoke now covers the low-cap path: secondary
  #1 stopped per-range child digest drill-down at the configured cap, switched
  to coarser proof-backed fetch spans, reduced primary range-digest fanout
  materially for the same outage shape, and still passed terminal checkpoint
  verification.
- The clustered reconcile-budget smoke now covers the negative path: a
  deliberately starved secondary fails closed with explicit
  `budget_exhausted` status while the primary and the healthy control secondary
  continue to validate terminal correctness.
- The key-only local KID index avoided fallback scans during HA mixed load while
  retaining exhaustive terminal data correctness.
- The latest HA smoke cleared the earlier adaptive-run `bucket_mismatch`
  fallback under the same 15-minute handoff shape: both secondaries used
  indexed repair over all top-level ranges with zero local KID fallback scans
  and zero indexed proof mismatches.
- Persisted flat accumulators can be restored across a secondary-cluster cold
  restart without forcing a local full scan.
- The pre-seed lifecycle now has an HA evidence point covering accepted
  baseline application, delta catch-up, primary active handoff, secondary
  active handoff, and checkpoint verification after the handoff.
- The pre-seed artifact model now validates an explicit artifact format,
  deterministic segment descriptors, checkpoint-bound segment payloads, and
  durable secondary-side segment staging across manager restart. Larger
  datasets, external artifact storage, and signed provenance remain future
  validation targets.
- Fixture-backed pre-seed can now take the stream-first catch-up path when the
  primary still retains journal coverage from the accepted checkpoint baseline.
  The current 100k fixture smoke caught up a 20k-key post-export delta with
  `reconcile_count=0` and strict checkpoint verification stayed on the O(1)
  accumulator path.
- The HA pre-seed smoke now also validates the stream-first catch-up path under
  active-node handoff: the secondary remained scan-free before and after the
  handoff, and checkpoint verification stayed on the O(1) accumulator path.
- The local orchestration for mixed-load smoke, pre-seed, quiescent reconnect,
  accumulator cold restart, within-horizon secondary outage, out-of-horizon
  indexed repair, and dynamic tuning load
  has been ported to `scripts/dr-harness` and validated through the same shell
  entrypoints. This reduces harness risk by replacing large scenario bodies in
  shell with typed Go clients, bounded workers, structured result artifacts,
  active-node discovery, and explicit retry logic only around known transient
  control-plane races.
- The generic full-duration HA mixed-load path now has current Go-harness
  evidence with strict-secondary checkpoint verification. The run saturated the
  primary stream ring, forced primary stepdowns, briefly entered reconciliation,
  and still converged with no local KID fallback scans, no full-bucket fallback,
  and no indexed proof mismatches.
- The dynamic tuning path now has current full-duration HA evidence. The
  validated run updated constrained and relaxed tuning profiles while mixed
  load, periodic primary stepdowns, and forced active handoff were in progress,
  then proved terminal correctness through primary API verification and
  strict-secondary checkpoint verification without physical-scan fallback.
- Read-only HA standbys can see transient keyring-missing state during DR
  key-transition refresh. The current implementation defers that standby-local
  transition instead of sealing, while the active path remains fail-closed.
- Secondary outage behavior is separated into replay-within-horizon and
  reconcile-beyond-horizon cases.
- Forced promotion semantics require explicit acknowledgement and keep old
  primary and promoted-primary lineages fenced.
- The broad engine/runtime matrix has passed across failover and reseed for the
  current local feature set.
- Secondary API read-serving semantics are intentionally outside the strict
  warm-standby surface. Terminal secondary correctness should be cited through
  checkpoint verification, or through API verification after promotion.

## Invariants, Evidence, and Gaps

| Protocol invariant | Current evidence | Open gap |
|---|---|---|
| `lastAppliedIndex` advances only after durable apply | HA steady-state soak, HA mixed-load smoke, outage/reconnect smokes, and terminal verification passed in the curated runs. | Production availability under active handoff is not an SLO yet. |
| Stream replay is used only when journal or buffer coverage is proven | Within-horizon secondary outage recovered through journal replay without reconciliation; beyond-horizon outage forced reconcile. | Retention defaults for offline secondaries over long periods need production sizing. |
| Checkpoint repair is bound to checkpoint tuple and immutable artifact state | Indexed repair, strict-secondary checkpoint verification, pre-seed smokes, and the low-cap fragmented-fanout smoke passed without physical-scan fallback in current evidence. | Larger keyspaces, WAN/proxy profiles, and production fanout defaults remain unmeasured. |
| Delete inference happens only after completeness verification | Range reconciliation and fetch proof tests cover duplicate, missing, failed, out-of-span, and digest-mismatch cases. | Larger keyspace and WAN/proxy profiles remain untested. |
| Optimizers are not authority | Flat accumulator restore, local KID-index repair, dirty bitmap hinting, and pre-seed baseline runs all fall back or verify before advancing. | Local KID-index metadata write pressure and optimizer seeding after resnapshot remain optimization targets. |
| Strict secondaries are warm standbys, not read replicas | Current HA smokes use checkpoint verification for secondary terminal correctness. | Supported pre-promotion read-serving is a separate future product decision. |
| Relationship authorization guards every DR data-plane RPC | Security test slices cover gRPC authz, revocation, bootstrap, rotation, and `SyncKeyring` binding. | DR transport CA rotation and independent security review remain open. |
| Primary and secondary resource usage must be bounded independently | Tuning validation, checkpoint artifact admission tests, S20 secondary budget exhaustion, S21 primary checkpoint/digest/fetch pressure unit gates, and clustered HA reconcile-budget pressure smoke exist. | Primary rejection/fetch-response budget paths remain unit-gated; production defaults still need measured sizing. |

## Performance Evidence

Performance and scale interpretation now lives in
[DR_PERFORMANCE_NOTES.md](DR_PERFORMANCE_NOTES.md). The validation claim here is
narrower: selected runs show terminal correctness, scan-free accumulator/indexed
repair in the tested paths, and promising secondary transaction-pressure
improvements. They do not yet prove production scale or availability SLOs.

The curated set does not prove:

- Production availability SLOs under active handoff.
- Rolling upgrade safety.
- Dependency-backed engine profiles that require external services.
- WAN latency, packet loss, or proxy/LB behavior.
- Very large keyspaces such as 1B+ keys.

## Package Test Caveat

The broad OpenBao vault package test was attempted after the S20/S21 slice:

```bash
go test ./vault -count=1
go test ./vault -count=1 -timeout=30m
```

Both runs timed out. The 30-minute timeout stack showed unrelated broad package
tests waiting in parallel scheduling around `vault/core_cache_invalidate_test.go`,
not an S20/S21 assertion failure. The targeted S20/S21 unit gates and the
clustered indexed-repair smoke remain the current validation evidence for this
slice.

## Historical Artifacts

The results directory contains many exploratory runs. Two examples are
specifically non-representative for current claims:

- `drmixed-indexed-repair-20260531T222252Z`: DR-correct 15-minute HA stepdown
  run that did not exercise indexed repair because journal replay succeeded.
- `drmixed-20260531T145022Z`: historical proof-mismatch fallback run from before
  the current indexed-repair hardening.

Keep those runs as regression context. Do not cite them as current expected
behavior without explaining their age and purpose.

## Next Evidence Targets

- Run the default-duration Go-harness-backed outage/reconcile smokes after the
  next implementation slice; the current harness parity runs intentionally used
  shortened durations.
- Keep secondary read-serving as an explicit future product decision rather
  than an implicit correctness requirement for strict warm standbys.
- Extend pre-seed evidence from local segmented and fixture-backed smoke
  coverage to production-scale artifact behavior: external artifact storage,
  provenance/signature handling, larger datasets than the current 100k fixture,
  and HA validation of stream-first catch-up. The local HA smoke already covers
  segmented post-seed delta catch-up, checkpoint verification, and post-accept
  HA handoff.
- Extend S21 primary pressure evidence beyond unit gates if we add first-class
  tuning knobs for clustered primary rejection/fetch-response budget paths. The
  current clustered budget smoke proves primary pressure counters move during a
  real reconnect, but rejection semantics remain unit-gated.
- Extend fragmented-fanout evidence to larger fixture profiles and production
  default sizing. The current clustered smoke proves the fallback path and
  correctness boundary, not billion-key scale.
- Add a current 1-hour no-stepdown stream-apply baseline with the latest code.
- Add dependency-backed engine profiles when their services are available in the
  local compose topology.
