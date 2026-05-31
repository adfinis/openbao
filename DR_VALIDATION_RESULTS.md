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
| `verify-*.json` | Independent verification output for primary and secondary clusters. |

Orchestrated smokes add scenario-specific files such as
`orchestrator.log`, `before-*-status.json`, `after-*-status.json`,
`final-*-status.json`, promotion summaries, and restart logs.

## Curated Runs

| Scenario | Run | Result | Evidence Summary |
|---|---|---|---|
| HA steady-state soak | `drsoak-20260530T115719Z` | Pass | 7,200s, 24 workers, 855,321 operations, zero PUT/GET/status failures, zero dropped events, 1s/1s sentinel convergence, exhaustive verification passed on all three clusters across 20,024 truth-log keys. |
| HA mixed load with primary handoff | `drmixed-20260531T214747Z` | Pass with expected client transients | 900s, 48 workers, primary stepdown every 300s, 88,023 operations, zero status failures, zero dropped events, 1s/1s sentinel convergence, exhaustive verification passed on all three clusters across 7,744 truth-log keys. PUT/GET failures were client-facing HA disruption noise. |
| HA mixed load after key-only KID index | `drmixed-20260531T231905Z` | Pass with expected client transients | 900s, 48 workers, primary stepdown every 300s, 133,318 operations, zero status failures, zero dropped events, 1s/1s sentinel convergence, exhaustive verification passed on all three clusters across 10,430 truth-log keys. Indexed repair loaded 558/298 ranges with zero fallback scans or proof mismatches. |
| Indexed repair beyond journal horizon | `indexed-repair-20260531T224314Z` | Pass with expected client transients | Forced secondary #1 outage beyond journal horizon. Primary `journal_range_too_old_total=2`; secondary #1 reached indexed repair total 2, 1,825 indexed ranges, 1,773 bucket loads, and 9,867 loaded KID-index entries with zero fallback scans, proof mismatches, full-bucket fallback, scan failures, or load failures. Exhaustive verification passed on all three clusters across 3,261 truth-log keys. |
| Secondary outage within journal horizon | `secondary-outage-20260531T211438Z` | Pass with expected client transients | 120s, 32 workers, secondary #1 stopped for 40s and recovered via journal replay. No reconciliation, no fallback scan, no journal-too-old increment, zero PUT/status failures, zero dropped events, and exhaustive verification passed on all three clusters across 2,143 truth-log keys. |
| Accumulator cold restart | `accumulator-cold-restart-20260531T205052Z` | Pass | Full secondary #1 restart restored `flat_accumulator_cursor_index=56113` and `flat_accumulator_snapshot_index=56113`, kept `reconcile_count=0`, and observed no scan/fallback/proof-mismatch counters. |
| Quiescent reconnect | `quiescent-reconnect-20260531T204826Z` | Pass | Forced primary active handoff from `http://localhost:8800` to `http://localhost:8802`; both secondaries remained streaming with lag 0 and no reconciliation count increase. |
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
- Persisted flat accumulators can be restored across a secondary-cluster cold
  restart without forcing a local full scan.
- Secondary outage behavior is separated into replay-within-horizon and
  reconcile-beyond-horizon cases.
- Forced promotion semantics require explicit acknowledgement and keep old
  primary and promoted-primary lineages fenced.
- The broad engine/runtime matrix has passed across failover and reseed for the
  current local feature set.

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

- Re-run the curated HA hard smoke and indexed-repair smoke after any secondary
  storage transaction-pressure optimization.
- Add a current dynamic tuning validation run once tuning defaults stabilize.
- Add a current 1-hour no-stepdown stream-apply baseline with the latest code.
- Add dependency-backed engine profiles when their services are available in the
  local compose topology.
