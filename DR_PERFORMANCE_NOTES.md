# DR Replication Performance and Scale Notes

This note tracks scalability observations and optimization direction for the
local DR replication prototype. It is not a benchmark report. Curated evidence
remains in [DR_VALIDATION_RESULTS.md](DR_VALIDATION_RESULTS.md), and open
production work remains in [DR_OPEN_WORK.md](DR_OPEN_WORK.md).

## Current Scale Model

The design separates four costs:

1. steady ordered mutation streaming;
2. primary stream journal and checkpoint artifact retention;
3. checkpoint digest/fetch serving on the primary; and
4. secondary reconcile execution and local apply pressure.

The target shape is:

- use stream replay for short outages;
- use persisted flat accumulators and local KID indexes to avoid secondary
  full scans during common reconnects;
- use checkpoint-fenced drill-down only for mismatched ranges;
- cap per-range drill-down fanout and switch to coarser proof-backed fetch spans
  under highly fragmented divergence;
- use pre-seed or resnapshot when the retained journal cannot cover a large
  base-copy gap; and
- bound primary and secondary work independently.

## Secondary Transaction Pressure

The important optimization signal from the 15-minute HA mixed-load runs is not
only total throughput. It is secondary commit pressure: stream transaction
count, cursor writes, accumulator snapshot/delta writes, local KID-index
updates, and max commit latency under active handoff.

The older baseline `drmixed-20260531T214747Z` passed correctness verification
but showed high write amplification:

| Node | Stream Txns | Avg Entries/Txn | Avg Commit ms | Max Commit ms | Cursor Writes | Snapshot Persists | Snapshot Skips | Local KID Updates |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| secondary1 | 6,764 | 19.64 | 100.55 | 5,687.65 | 6,768 | 175 | 6,593 | 132,799 |
| secondary2 | 6,716 | 19.75 | 100.52 | 5,466.99 | 6,724 | 179 | 6,545 | 132,512 |

The key-only local KID-index optimization reduced steady-state value-update
index writes while preserving indexed repair:

| Run | Node | Physical Entries | Local KID Updates | Indexed Ranges | Fallback Scans |
|---|---|---:|---:|---:|---:|
| `drmixed-20260531T231905Z` | secondary1 | 201,556 | 139,416 | 558 | 0 |
| `drmixed-20260531T231905Z` | secondary2 | 202,283 | 139,880 | 298 | 0 |

Adaptive stream batching then roughly halved stream transaction/cursor pressure
in `drmixed-20260531T235152Z`, but that first run exposed one
`bucket_mismatch` local KID-index fallback. The follow-up
`drmixed-20260601T114049Z` kept the adaptive shape and cleared that issue:

| Node | Stream Txns | Physical Entries | Avg Entries/Txn | Avg Commit ms | Max Commit ms | Indexed Ranges | Fallback Scans | Proof Mismatches |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| secondary1 | 4,425 | 164,252 | 37.24 | 87.18 | 1,944.17 | 1,024 | 0 | 0 |
| secondary2 | 4,392 | 163,021 | 37.24 | 86.54 | 2,203.60 | 1,024 | 0 | 0 |

The current Go-harness full HA smoke `drmixed-20260602T075024Z` has cleaner
terminal proof semantics because strict secondaries are verified by checkpoint
proof rather than pre-promotion API reads:

| Node | Stream Txns | Physical Entries | Avg Entries/Txn | Avg Commit ms | Max Commit ms | Indexed Ranges | Fallback Scans | Proof Mismatches |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| secondary1 | 4,061 | 115,478 | 28.51 | 105.61 | 1,438.99 | 490 | 0 | 0 |
| secondary2 | 4,039 | 114,702 | 28.48 | 105.06 | 1,439.49 | 749 | 0 | 0 |

The next pressure target is secondary commit latency and metadata write
amplification while the primary stream ring is saturated.

## Reconciliation Scale

Flat accumulators make top-level comparison cheap, but drill-down still has a
worst-case cost when divergence is highly fragmented. The design must account
for both sides:

- the primary pays checkpoint construction, child digest lookup, fetch
  serialization, TLS/gRPC overhead, and artifact retention;
- the secondary pays digest coordination, local KID-index bucket loads,
  possible fallback scans, apply/delete batches, and runtime refresh.

The secondary doing more coordination work does not by itself protect the
primary. Primary-side admission control is required for checkpoint build
concurrency, rebuild frequency, digest/fetch request rate, and response bytes.

The prototype now bounds secondary-requested child digest fanout per mismatched
top-level range. When the cap is reached, the secondary stops splitting and
fetches the remaining child spans at their current granularity. This reduces
primary digest RPC pressure in adversarial fragmentation, while preserving the
same checkpoint correctness boundary because each coarser span still carries a
primary digest proof and the fetch verifier recomputes it before applying
remote entries or inferring local-only deletes.

The paired clustered smokes `indexed-repair-20260602T142940Z` and
`fragmented-fanout-20260602T142058Z` exercised that path under the same
300-second, 64-worker HA outage shape on the same rebuilt binary. With the
default cap of 32, primary range-digest request delta was 39,301 and fetch
request delta was 3,974. With `reconcile_max_range_drilldown_rpcs=1` applied
only to the disrupted secondary, primary range-digest request delta dropped to
3,877 and fetch request delta to 3,877. The low-cap secondary used 3,865 coarse
fetches over 6,411 ranges, kept fallback/proof/load failure counters at zero,
and checkpoint verification remained scan-free on both secondaries.

## Large Existing Clusters

For an old primary with a very large dataset, the intended initial-sync path is
pre-seed:

1. create relationship material;
2. export checkpoint-bound seed artifacts;
3. transfer/import into a disabled secondary;
4. accept the baseline cursor and optimizer metadata;
5. replay the post-seed delta from the stream journal; and
6. reconcile only if replay coverage is gone.

This is the main way to reduce first-sync cost without changing the correctness
model. The 100k fixture smoke is a positive signal because it validated seed
import plus 20k active post-export writes with stream-first catch-up and no
physical scan. It is not yet a billion-key proof.

## Optimization Backlog

The strongest next optimizations are:

- first-class reconcile budget ledger on the secondary;
- primary checkpoint/digest/fetch admission control;
- external, signed pre-seed artifact storage with resumable segment transfer;
- optimizer seeding from verified checkpoint artifacts after pre-seed and
  resnapshot;
- production default tuning for bounded drill-down fanout under larger
  fragmented reconnects;
- more aggressive coalescing of secondary local metadata writes;
- measured defaults for stream journal retention and adaptive batching; and
- larger fixture profiles that validate key-count scaling separately from
  adversarial HA availability.

Dynamic top-level bucket sizing remains deferred. The current range versioning
can describe different `range_bits`, but changing bucket count affects
protocol compatibility. Keep the default stable until measurements show that
top-level bucket pressure, rather than drill-down/fetch/admission control, is
the bottleneck.
