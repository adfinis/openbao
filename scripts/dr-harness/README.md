# DR Harness

`dr-harness` is the typed orchestration harness for local DR replication
validation. It is intended to replace scenario-sized logic in
`scripts/dr_local_test.sh` while keeping that script as a compatibility wrapper
for existing make targets and operator muscle memory.

## Current Scope

The active Go-backed scenarios are:

```text
scripts/dr_local_test.sh --topology ha smoke
scripts/dr_local_test.sh --topology ha preseed-smoke
scripts/dr_local_test.sh --topology ha quiescent-reconnect-smoke
scripts/dr_local_test.sh --topology ha accumulator-cold-restart-smoke
scripts/dr_local_test.sh --topology ha secondary-outage-smoke
scripts/dr_local_test.sh --topology ha secondary-outage-reconcile-smoke
scripts/dr_local_test.sh --topology ha indexed-repair-smoke
scripts/dr_local_test.sh --topology ha tuning-load-smoke
```

The shell commands now build `bin/dr-harness` and invoke:

```text
bin/dr-harness smoke
bin/dr-harness preseed-smoke
bin/dr-harness quiescent-reconnect-smoke
bin/dr-harness accumulator-cold-restart-smoke
bin/dr-harness secondary-outage-smoke
bin/dr-harness secondary-outage-reconcile-smoke
bin/dr-harness indexed-repair-smoke
bin/dr-harness tuning-load-smoke
```

The topology lifecycle is still subprocess-backed through
`scripts/dr_local_test.sh reset` or `dataset-fixture-restore`. The scenario
bodies are Go: active-node discovery, OpenBao API calls, pre-seed
export/import, bounded KV write pressure, checkpoint verification, HA handoff,
secondary outage/restart orchestration, tuning profile setup, `dr-stress`
foreground process control, passthrough workload flags, and artifact writing.
Docker compose lifecycle is still invoked as a subprocess from typed scenario
code.

## Package Boundaries

- `internal/bao`: typed OpenBao HTTP client and DR/pre-seed API wrappers.
- `internal/topology`: env-file parsing, topology endpoint sets, reset bridge,
  active-node discovery, and status waits.
- `internal/workload`: bounded concurrent workloads using one tuned HTTP client
  instead of shelling out to many `curl` processes.
- `internal/artifact`: run directory creation, JSON output, and orchestrator
  logs.
- `internal/scenario`: scenario orchestration. Each new scenario should own its
  command flags, result schema, and assertions here.

## Artifact Shape

Every scenario writes a run directory under `dr-stress-results/` with:

- `result.json`: typed scenario summary.
- `orchestrator.log`: scenario progress.
- boundary status files such as `before-*-status.json` and
  `after-*-status.json`.
- scenario-specific artifacts such as pre-seed manifests and segment responses,
  restart logs, raft state after restart, or `dr-stress` verification outputs.

`preseed-smoke` additionally writes `summary.txt`, `export.json`,
`manifest.json`, `import-begin.json`, `import.json`,
`segments/segment-*-{export,import}.json`, staged segment payloads,
`seed-bulk-result.json`, `post-export-bulk-result.json`,
`secondary2-status*.json`, and `verify-secondary2*.json`.

The outage scenarios additionally write `harness.out`, `harness.err`,
secondary restart logs, before/after optimizer counters, and
`verify-{primary,secondary1,secondary2}.json`.

`smoke` delegates the workload body to `dr-stress run`; it writes an
`orchestrator.log`, applies HA primary tuning when requested, and preserves
`dr-stress` artifacts such as `config.json`, `result.json`, `events.ndjson`,
`writes.ndjson`, `status_timeline.ndjson`, and `progress.log`.

`tuning-load-smoke` also delegates the workload body to `dr-stress run`; it
writes tuning state snapshots before and after constrained/relaxed updates,
forces HA handoff, preserves the `dr-stress` workload artifacts, writes
`scenario-result.json`, stores terminal checkpoint verification outputs, and
keeps `tuning-*-attempt-*.err` files when transient HA control-plane races are
retried.

The validation docs should prefer `result.json` and checkpoint verification
files over console output.

## Migration Rules

When porting more scenarios:

1. Keep shell wrappers thin and compatibility-focused.
2. Keep scenario state in typed Go structs, not parsed console text.
3. Emit durable JSON artifacts for every assertion boundary.
4. Use bounded Go workloads for write/read pressure.
5. Keep topology reset/bootstrap as the only temporary shell subprocess
   boundary until it is worth moving Docker lifecycle into Go.
6. Preserve old shell bodies as `*_legacy` only while parity is being proven.

Recommended next ports:

1. Failover-load lifecycle orchestration.
2. Engine matrix and lifecycle matrix.
3. Dataset fixture create/restore lifecycle.
4. Cluster reset/bootstrap/configure once Docker lifecycle churn warrants it.
