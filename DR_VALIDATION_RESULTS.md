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

## Secondary Transaction-Pressure Baseline

The current optimization baseline is `drmixed-20260531T214747Z`, the curated
15-minute HA mixed-load run with primary handoff pressure. It passed exhaustive
verification, but showed high secondary write amplification:

| Node | Stream Txns | Avg Entries/Txn | Avg Commit ms | Max Commit ms | Cursor Writes | Snapshot Persists | Snapshot Skips | Local KID Updates |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| secondary1 | 6,764 | 19.64 | 100.55 | 5,687.65 | 6,768 | 175 | 6,593 | 132,799 |
| secondary2 | 6,716 | 19.75 | 100.52 | 5,466.99 | 6,724 | 179 | 6,545 | 132,512 |

The local KID-index update count was effectively one write per replicated
physical entry. The implemented optimization target is therefore to make the
local repair index store stable key identity only and recompute VIDs from local
storage only when indexed repair loads a mismatched bucket. This keeps the
flat accumulator as the authoritative value digest while reducing steady-state
secondary-local write pressure for hot-key updates. Value-only batches also
avoid advancing the local KID-index metadata; that metadata now tracks the
key-set index and may lag the accumulator value index.

The first validation run after that optimization is `drmixed-20260531T231905Z`.
It preserves the correctness signal and shows the intended steady-state shape:

| Node | Stream Txns | Physical Entries | Avg Entries/Txn | Avg Commit ms | Max Commit ms | Cursor Writes | Snapshot Persists | Snapshot Skips | Local KID Updates | Indexed Ranges | Indexed Entries Loaded | Fallback Scans |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| secondary1 | 9,713 | 201,556 | 20.77 | 61.70 | 1,376.58 | 9,719 | 180 | 9,539 | 139,416 | 558 | 12,760 | 0 |
| secondary2 | 9,803 | 202,283 | 20.65 | 61.70 | 1,266.54 | 9,809 | 181 | 9,628 | 139,880 | 298 | 6,824 | 0 |

The run had more replicated physical entries than the baseline, so the absolute
local KID-index write count is still high. The important directional result is
that the index no longer tracks every value mutation: local KID-index updates
were about 69% of physical entries while indexed repair still completed without
local full scans.

The first adaptive stream batching validation run is `drmixed-20260531T235152Z`.
Both secondaries adapted from the 25ms baseline to a 100ms effective wait
window (`stream_batch_adaptive_level=2`, six adjustments observed live after
the run). This roughly halved the number of secondary stream transactions and
cursor writes:

| Node | Stream Txns | Physical Entries | Avg Entries/Txn | Avg Commit ms | Max Commit ms | Cursor Writes | Snapshot Persists | Snapshot Skips | Local KID Updates | Indexed Ranges | Fallback Scans |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| secondary1 | 4,361 | 188,852 | 43.37 | 92.58 | 2,239.52 | 4,367 | 175 | 4,192 | 130,955 | 0 | 1 |
| secondary2 | 4,401 | 190,505 | 43.36 | 92.29 | 2,238.07 | 4,407 | 177 | 4,230 | 132,102 | 372 | 0 |

The transaction-pressure result was positive, but the single secondary #1
`bucket_mismatch` fallback scan made reconciliation-after-handoff pressure the
next follow-up target.

The follow-up HA smoke is `drmixed-20260601T114049Z`. It keeps the adaptive
batching shape and validates the standby key-transition fix at the same time:

| Node | Stream Txns | Physical Entries | Avg Entries/Txn | Avg Commit ms | Max Commit ms | Cursor Writes | Snapshot Persists | Snapshot Skips | Local KID Updates | Indexed Ranges | Bucket Loads | Entries Loaded | Fallback Scans | Proof Mismatches |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| secondary1 | 4,425 | 164,252 | 37.24 | 87.18 | 1,944.17 | 4,432 | 174 | 4,258 | 164,231 | 1,024 | 10,405 | 12,703 | 0 | 0 |
| secondary2 | 4,392 | 163,021 | 37.24 | 86.54 | 2,203.60 | 4,399 | 172 | 4,227 | 163,016 | 1,024 | 11,072 | 13,183 | 0 | 0 |

This closes the previous `bucket_mismatch` follow-up for the current 15-minute
HA smoke shape. The remaining transaction-pressure target is secondary commit
latency and local metadata write amplification, especially while the primary
stream ring is saturated under adversarial write load.

The current full-duration Go-harness mixed-load baseline is
`drmixed-20260602T075024Z`. It uses the same 900s / 48-worker / 300s-stepdown
shape as the earlier 15-minute HA smokes, but terminal secondary proof now uses
strict checkpoint verification instead of secondary API reads:

| Node | Stream Txns | Physical Entries | Avg Entries/Txn | Avg Commit ms | Max Commit ms | Cursor Writes | Snapshot Persists | Snapshot Skips | Local KID Updates | Indexed Ranges | Bucket Loads | Entries Loaded | Fallback Scans | Proof Mismatches |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| secondary1 | 4,061 | 115,478 | 28.51 | 105.61 | 1,438.99 | 4,068 | 177 | 3,891 | 115,309 | 490 | 645 | 606 | 0 | 0 |
| secondary2 | 4,039 | 114,702 | 28.48 | 105.06 | 1,439.49 | 4,046 | 175 | 3,871 | 114,441 | 749 | 1,279 | 1,061 | 0 | 0 |

Compared with the older `drmixed-20260601T114049Z` evidence, this run has lower
total operation count but cleaner terminal proof semantics: primary API
verification passed across 7,094 truth-log keys and both secondaries passed
checkpoint verification with 1,024/1,024 matched ranges, no scan fallback, and
no optimizer reseed.

The current dynamic-tuning HA evidence point is
`tuning-ha-load-20260602T071619Z`. It does not replace the generic mixed-load
transaction-pressure baseline because it uses 36 workers and a 90s primary
stepdown cadence, but it validates runtime tuning writes under active HA
movement. Both strict secondaries completed terminal checkpoint verification
with no physical scan, no optimizer reseed, and zero range mismatches.

The curated set does not prove:

- Production availability SLOs under active handoff.
- Rolling upgrade safety.
- Dependency-backed engine profiles that require external services.
- WAN latency, packet loss, or proxy/LB behavior.
- Very large keyspaces such as 1B+ keys.

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
- Add explicit reconcile-budget and primary checkpoint-pressure validation
  targets. The current stress results show promising convergence behavior, but
  they are not yet a worst-case model for fragmented reconnects or maliciously
  expensive checkpoint drill-down.
- Add a current 1-hour no-stepdown stream-apply baseline with the latest code.
- Add dependency-backed engine profiles when their services are available in the
  local compose topology.
