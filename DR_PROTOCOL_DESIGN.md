# DR Replication Protocol Design

This note contains the data-plane and reconciliation design for native DR
replication. The top-level RFC is [RFC_DR_Replication.md](RFC_DR_Replication.md).

## Scope

The initial protocol target is integrated-storage/Raft-backed OpenBao clusters.
The design assumes:

- ordered primary physical mutation indexes
- durable secondary apply
- local transaction support for replicated writes plus DR-local metadata
- a primary that is trusted as the data authority for its active relationships
- an untrusted network protected by mTLS and relationship authorization

Other storage backends are future work unless they can provide equivalent
ordering, replay, and transaction boundaries.

## Optimization Summary

The protocol uses several optimizations, but none of them replace the
checkpoint correctness boundary. Accelerators can reduce scan, fetch, or commit
pressure only when their relationship, checkpoint, range, and proof metadata
validate.

| Optimization | Reduces | Protocol role |
|---|---|---|
| KID/VID projection | Path-specific comparison and raw key exposure | Converts physical storage into deterministic key/value identifiers for range math, digests, accumulators, local indexes, and seed manifests. |
| Fixed KID range plane | Initial comparison cost | Divides sorted KID space into 1024 top-level ranges for coarse checksum comparison before drill-down. |
| Top-level range checksums | Digest and fetch work for matching ranges | Lets reconciliation finalize at Phase A when every range count/checksum matches the checkpoint. |
| Mandatory digest drill-down | Full-bucket fetches for localized divergence | Splits only mismatched ranges into child spans with stronger KID/VID digest descriptors. |
| Bounded drill-down fanout | Unbounded digest RPCs under fragmented divergence | Caps child-digest RPCs per mismatched top-level range, then fetches coarser proof-bearing spans when further splitting would exceed the cap. |
| Flat accumulator | O(N) secondary local scans on common reconnects | Maintains top-level range count/checksum incrementally during stream apply and persists snapshots/deltas for restart restore. |
| Local KID index | Full local scans for mismatched buckets | Maps local KIDs back to physical keys so indexed repair can load and delete affected local entries without scanning unrelated storage. |
| In-memory stream buffer | Reconciliation after short disconnects | Replays recent ordered mutations directly from primary memory when coverage from the secondary cursor is proven. |
| Disk-backed stream journal | Reconciliation after longer reconnects or HA handoff | Replays retained ordered mutations from primary journal segments when the in-memory buffer no longer covers the cursor. |
| Credit-based streaming | Unbounded primary buffering | Lets the secondary pace ingress by granting credits after durable apply. |
| Stream transaction batching | Secondary commit and metadata write pressure | Commits multiple ordered stream entries per secondary transaction within configured entry, byte, and wait bounds. |
| Duplicate mutation coalescing | Redundant physical writes inside a batch | Materializes only the final mutation for a physical key inside an atomic secondary transaction. |
| Adaptive stream batch wait | Commit pressure under backlog | Temporarily stretches the secondary wait window under pressure, then decays it when quiet. |
| Dirty bitmap | Time to reach likely divergent ranges | Prioritizes dirty top-level ranges, but range selection remains complete; bitmap false negatives cannot skip verification. |
| Checkpoint artifact store | Live-storage races and repeated fetch scans | Serves `FetchEntries` and pre-seed segments from immutable checkpoint artifacts instead of live storage. |
| Fetch batching and parallel artifact reads | Fetch RPC and artifact-read overhead | Sends bounded response batches and parallelizes range artifact reads. |
| Reconcile apply batching/workers | Repair apply wall time | Applies fetched puts/deletes in bounded batches with limited worker concurrency. |
| Segmented pre-seed | Initial full reconciliation for old or large primaries | Lets operators import a relationship-bound base copy into a disabled secondary, then catch up through journal replay or reconciliation. |
| Pre-seed optimizer baseline | Post-import local scan | Persists flat accumulator and local KID-index baseline from the verified seed set. |

Performance evidence and remaining scale work live in
[DR_PERFORMANCE_NOTES.md](DR_PERFORMANCE_NOTES.md).

## Steady Streaming

The normal path is ordered physical mutation streaming:

```text
primary Raft-applied physical changes
  -> filter cluster-local paths
  -> append to DR stream journal
  -> active primary fans out batches to secondary subscribers
  -> secondary writes ciphertext bytes to local physical storage
  -> secondary advances lastAppliedIndex after durable apply
```

The primary has two replay horizons:

- an in-memory ring buffer for short reconnects
- a disk-backed stream journal for longer reconnects and HA leader handoff

Primary HA followers append the filtered stream journal as they apply Raft
entries. After leadership moves, the new active primary can replay journal
coverage even if its in-memory stream buffer is empty.

Flow control is credit-based. The secondary opens `StreamChanges` with its
current `lastAppliedIndex` and initial window. The primary sends only while
credits are available. The secondary replenishes credits after durable apply,
so stream throughput is bounded by secondary apply progress instead of
unbounded primary buffering.

If a primary Raft batch contains only cluster-local paths, the primary sends an
index-advance marker. The marker lets the secondary advance lag accounting
without writing replicated storage.

For transactional physical backends, secondary stream apply coalesces duplicate
mutations for the same physical key inside one streamed transaction. Only the
final put or delete for a key is materialized. This is safe because the batch
is applied atomically and intermediate states are not externally visible.

The secondary apply worker also adapts its local flush cadence. The configured
stream batch wait remains the low-load baseline. When the worker observes
backlog or commit pressure, it can temporarily stretch the wait window up to a
bounded cap so more received stream batches coalesce into each secondary
transaction. Under quiet conditions it decays back toward the baseline. This
reduces cursor, accumulator, and local index write amplification without
changing ordering or crash-atomic apply semantics.

```mermaid
flowchart TB
    A["Primary Raft-applied physical mutation"] --> B{"Replicated path?"}
    B -->|"No: cluster-local path"| C["Emit index-advance marker"]
    B -->|"Yes"| D["Append filtered mutation<br/>to DR stream journal"]
    C --> E["Fan out to secondary subscribers"]
    D --> E
    E --> F["Secondary receives StreamChanges batch"]
    F --> G["Spend flow-control credits"]
    G --> H["Coalesce duplicate mutations<br/>within transaction"]
    H --> I["Write ciphertext entries<br/>to secondary storage"]
    I --> J["Update local DR metadata<br/>cursor / accumulator / KID index"]
    J --> K{"Durable commit succeeds?"}
    K -->|"No"| L["Fail closed<br/>do not advance lastAppliedIndex"]
    K -->|"Yes"| M["Advance lastAppliedIndex"]
    M --> N["Refresh affected runtime state"]
    N --> O["Remain in streaming state"]
```

## Replay Failure

Streaming fails closed. A lagging subscriber is disconnected instead of
silently dropping entries. On reconnect, the primary tries journal and buffer
replay from the secondary cursor. If replay coverage cannot be proven, the
secondary must enter checkpoint reconciliation.

The in-memory stream ring being full is a pressure signal, not by itself a
correctness failure. Correctness depends on whether the primary can still prove
journal or buffer coverage from the secondary cursor. If it cannot, the stream
path terminates and reconciliation becomes mandatory.

Stream journal retention is finite. The credit protocol protects an online
stream from overrunning the secondary, but it does not imply that a primary
retains days of mutation history for an offline secondary. If the secondary's
cursor falls behind the oldest retained journal/buffer entry, replay must fail
closed and the secondary must use checkpoint reconciliation, resnapshot, or an
operator pre-seeded base copy.

```mermaid
flowchart TD
    A["Secondary connection lost"] --> B["Reconnect with lastAppliedIndex"]
    B --> C{"Primary can prove<br/>journal or buffer coverage<br/>from cursor?"}
    C -->|"Yes"| D["Replay missing stream entries"]
    D --> E["Durably apply replay"]
    E --> F["Advance lastAppliedIndex"]
    F --> G["Resume streaming"]
    C -->|"No"| H["Enter checkpoint-fenced reconciliation"]
    H --> I["Request immutable checkpoint"]
    I --> J["Compare and repair ranges"]
    J --> K{"All reconcile phases succeeded?"}
    K -->|"No"| L["Fail closed<br/>keep previous committed state"]
    K -->|"Yes"| M["Finalize checkpoint<br/>advance lastAppliedIndex"]
    M --> G
```

## Checkpoints

A checkpoint is an immutable primary-side reconciliation target with a
relationship-bound commit-index high-water mark.

Checkpoint identity is:

- checkpoint ID
- checkpoint commit index
- relationship ID

Every reconciliation RPC is bound to that tuple. The secondary rejects
checksum, digest, fetch, or delete work that does not match its active
checkpoint tuple.

The primary serves `FetchEntries` from checkpoint artifacts rather than live
storage. This prevents live write races from changing the target while the
secondary is repairing against it.

## Compatibility Gates

The current prototype is a greenfield protocol, so compatibility is strict:
missing or unknown protocol metadata fails closed instead of being interpreted
as a legacy-compatible default.

The main versioned boundaries are:

| Boundary | Version metadata | Current rule |
|---|---|---|
| Live checkpoint reconciliation | `CheckpointResponse.range_plan_version` | Secondary accepts only the current deterministic range-plan version before verification, resnapshot, or reconciliation work starts. A missing zero value is unsupported. |
| Pre-seed manifest | manifest version, range-plan version, range bits/count, checksum algorithm, value domain, accumulator snapshot version, local KID-index version, bundle format, integrity algorithm | Import accepts only matching metadata for the active relationship and activation token. |
| Persisted flat accumulator | snapshot, cursor, delta, stream-applied-index versions plus relationship, cluster, range bits, and checksum algorithm | Restore or fast-path use is allowed only when metadata matches the current relationship and local cluster context. |
| Local KID index | meta, entry, and leaf-digest versions plus relationship, cluster, range bits/count, and leaf bits | Indexed repair is allowed only after the local index proves it is checkpoint-aligned and version-compatible. Otherwise reconciliation falls back to a full local scan. |
| `SyncKeyring` crypto envelope | AEAD AAD version | The secondary rejects unknown AAD versions and mismatched relationship/identity binding. |
| DR transport CA trust | active CA plus ordered trusted CA set | Secondary transport verification accepts primary leaves chained to any configured trust anchor during rotation overlap. `SyncKeyring` primary identity is bound to the CA that verified the primary leaf for the current mTLS connection, with active CA fallback only before a verified connection exists. |

DR transport CA rotation uses signed public trust bundles rather than silent
trust replacement. The primary stages a pending CA and returns a staged bundle
signed by the current active CA; operators apply that bundle to secondaries
before activation so the staged CA is trusted for the overlap window. After
activation, the primary promotes the staged CA to active, retains the old public
CA as previous, renews its transport leaf from the new active CA, and returns a
post-activation bundle signed by the new active CA. Secondaries that already
trusted the staged CA can then promote it to their active `primary_ca_cert`.
After the overlap window, the primary retires the previous public CA from
future bundles and secondaries applying the retired bundle remove it from their
trust set. After retirement, secondaries reject stale bundles that would
reintroduce the retired previous CA.

This is not yet a mixed-version rolling-upgrade contract. A production design
still needs an explicit compatibility matrix that states which primary and
secondary versions may stream, reconcile, verify, pre-seed, and promote
together. Until that exists, new protocol, artifact, optimizer, or crypto
formats should add explicit version checks and negative tests before being used
by a checkpoint or relationship.

## Checkpoint Construction Under Live Writes

The current design does not require the primary to expose a historical MVCC
view for every physical value at exactly `commit_index`. Instead, checkpoint
construction is commit-index fenced and artifact based:

1. The primary chooses a checkpoint commit index.
2. The primary waits until its applied-index observer has reached that index.
3. The primary builds or reuses KID/VID metadata for replicated physical
   storage.
4. The primary materializes an immutable checkpoint artifact.
5. Range checksums, child digests, `FetchEntries`, and pre-seed segment export
   are served from that same checkpoint artifact and cache entry.

Live writes can occur while the artifact is being materialized. The artifact is
therefore the reconciliation target, not live storage. If a key disappears
during materialization, it is omitted from the checkpoint live set and the
storage-drift counter records the event. If a value changes during
materialization, the artifact records the materialized value/VID and the
storage-drift counter records the drift. Later stream replay or checkpoint
reconciliation covers subsequent movement beyond the checkpoint high-water
mark.

Completeness checks are scoped to the immutable checkpoint artifact. Delete
inference is allowed only after the secondary has cryptographically verified
that the fetched span is complete for that artifact and checkpoint tuple.

## KID and VID

Reconciliation compares storage through projected metadata:

- KID: keyed identifier derived from storage key and relationship replication
  salt.
- VID: value identifier derived from ciphertext value and seal-wrap metadata,
  or a tombstone marker for explicit point-delete fetches.

OpenBao physical storage is conceptually a key/value table:

```text
storage key                         value bytes
---------------------------------------------------------
auth/userpass/users/alice           ...
secret/data/app/config              ...
identity/entity/id/123              ...
```

Reconciliation projects that table into sorted KID space:

```text
storage key                         KID                 VID
----------------------------------------------------------------------------
auth/userpass/users/alice           0x91ab...           0x4410...
secret/data/app/config              0x04c2...           0xa183...
identity/entity/id/123              0x60fe...           0xff02...
```

The implementation divides the one-dimensional KID space into deterministic
ranges. Range checksums and digests are aggregates over KID/VID pairs.

```mermaid
flowchart LR
    subgraph Storage["Physical storage table"]
        K1["secret/data/app<br/>value bytes"]
        K2["auth/userpass/alice<br/>value bytes"]
        K3["identity/entity/123<br/>value bytes"]
        K4["pki/cert/serial<br/>value bytes"]
    end

    Projection["KID = H(storage key, salt)<br/>VID = H(ciphertext value, flags)"]

    subgraph Ranges["Sorted KID space"]
        R0["Range 0<br/>count + checksum"]
        R1["Range 1<br/>count + checksum"]
        R2["Range 2<br/>count + checksum"]
        R3["Range 3<br/>count + checksum"]
    end

    K1 --> Projection
    K2 --> Projection
    K3 --> Projection
    K4 --> Projection
    Projection --> R0
    Projection --> R1
    Projection --> R2
    Projection --> R3
```

## Reconciliation Flow

The reconciliation flow is:

1. Secondary requests a checkpoint.
2. Secondary obtains local KID/VID state from a validated optimizer fast path
   or a full local scan.
3. Secondary selects ranges requiring verification.
4. Primary and secondary compare top-level range checksums.
5. Mismatched ranges enter mandatory digest drill-down.
6. Primary returns child digests that fully cover each parent span.
7. Secondary continues splitting until ranges converge, reach terminal spans, or
   hit the configured per-range fanout cap.
8. Secondary fetches mismatched spans from checkpoint artifacts. If fanout is
   capped, the fetch spans are coarser, but still carry the primary's digest
   proofs.
9. Secondary cryptographically verifies fetched content against advertised
   digest metadata.
10. Secondary applies fetched primary entries.
11. Secondary deletes local-only keys only after fetched remote spans are
    verified complete.
12. Secondary advances `lastAppliedIndex` only after every phase succeeds.

```mermaid
sequenceDiagram
    autonumber
    participant S as Secondary
    participant P as Primary
    participant CS as Checkpoint store
    participant LS as Secondary storage

    S->>P: RequestCheckpoint(relationship_id)
    P->>CS: Materialize immutable checkpoint
    P-->>S: checkpoint_id + commit_index + relationship_id

    S->>LS: Build or load local KID/VID view
    S->>P: ExchangeRangeChecksums(checkpoint tuple)
    P->>CS: Read checkpoint range checksums
    P-->>S: Top-level range checksums

    S->>S: Select mismatched ranges

    loop For each mismatched range
        S->>P: ExchangeRangeDigests(parent span)
        P->>CS: Read child digest descriptors
        P-->>S: Child span coverage + count + digest
        S->>S: Validate no gaps, overlap, or invalid split
        S->>S: Stop splitting at fanout cap and keep proof-bearing spans
        S->>P: FetchEntries(mismatched spans)
        P->>CS: Fetch checkpoint-scoped entries
        P-->>S: Entry batch + digest metadata
        S->>S: Recompute digest and verify completeness
        S->>LS: Apply fetched primary puts
        S->>LS: Delete local-only keys after verification
    end

    S->>S: Verify all required ranges complete
    S->>LS: Persist checkpoint high-water mark
    S->>S: Advance lastAppliedIndex
```

## Range Checksums

Top-level range checksums are coarse filters. A checksum match can skip a range
only when range selection is complete for the checkpoint. A mismatch always
requires digest drill-down.

Dirty bitmaps are scheduling hints only. A checkpoint cannot advance
`lastAppliedIndex` unless the secondary has verified every relevant range or
has a checkpoint-bound proof that skipped ranges were unchanged. The prototype
uses the conservative rule: verify the complete top-level partition for each
checkpoint reconciliation.

## Strict Secondary Verification

Strict warm-standby secondaries do not serve replicated secret data before
promotion. Terminal correctness for a strict secondary is therefore proven
through the DR control plane:

1. The secondary requests an immutable primary checkpoint for its active
   relationship.
2. The secondary requires its local flat accumulator index to match the
   checkpoint commit index.
3. The secondary requests all top-level range checksums for the checkpoint
   tuple.
4. The secondary compares every checkpoint range with its local accumulator.

The `sys/replication/dr/secondary/verify-checkpoint` endpoint returns only
proof metadata: pass/fail, checkpoint identity, accumulator index, range
counts, and mismatch summaries. It does not expose replicated keys or values.
It is a relationship-local DR control endpoint that does not depend on the
replicated token backend. The handler authenticates the caller with
relationship-local control material captured when the secondary was enabled.
This keeps verification aligned with warm-standby semantics while still giving
the test harness a terminal data-correctness proof for secondaries.

## Mandatory Digest Drill-Down

For each mismatched range, the secondary calls `ExchangeRangeDigests` with a
parent span. The primary returns child digest descriptors containing:

- child span
- count
- XOR of SHA-256 over KIDs
- XOR of SHA-256 over KID/VID pairs
- approximate value bytes for planning

The secondary rejects digest responses that are empty, invalid, overlapping,
outside the parent span, gapful, non-advancing for splittable parents, or
missing required digest fields.

This is a greenfield feature, so unsupported or failing drill-down does not
fall back to an unproven fetch path.

## Fetch Completeness Proof

For every fetched span, the secondary recomputes the digest accumulators over
received entries and compares them to the primary's advertised digest.

The secondary rejects fetched output that:

- has a checkpoint tuple mismatch
- contains `failed_kids`
- contains entries without resolvable KIDs
- contains duplicate KIDs
- contains KIDs outside requested spans
- omits entries required by the span digest
- includes entries that make the recomputed digest differ

Only after digest validation succeeds can the secondary treat the fetched set
as cryptographically verified complete for those spans.

## Delete Inference

Range fetches list primary-present entries. They do not naturally list
secondary-local keys that no longer exist on the primary.

After fetch completeness is cryptographically verified, the secondary compares
local KIDs in the fetched spans with the verified remote KID set. Local KIDs
absent from the remote set become candidate deletes. Deletes are applied after
fetched puts complete and before checkpoint finalization.

Absence is authority only after completeness verification. This is the key
safety boundary for delete inference.

## Apply and Commit Rule

Reconciliation has distinct phases:

1. range comparison and drill-down
2. fetch proof validation
3. fetched put application
4. inferred delete application
5. checkpoint finalization

If any phase fails, the secondary does not advance `lastAppliedIndex`. A later
attempt retries from the previous committed state.

## Flat Accumulator

The secondary maintains a flat accumulator over top-level KID ranges. Each
bucket stores count and checksum for KID/VID pairs in that range. This is not a
Merkle tree: there are no persisted hierarchical nodes, no parent/child
traversal state, and no independent diff authority.

During stream apply, the secondary computes old and new VID contribution for
each replicated physical mutation, XORs the old contribution out of the bucket,
and XORs the new contribution in. For transactional backends, replicated
storage mutations, the stream-applied index marker, flat-accumulator cursor and
deltas, and local KID index create/delete updates are committed in the same
local transaction.

The full accumulator snapshot is persisted on a bounded cadence and graceful
stream shutdown. Between snapshots, the secondary persists local-only delta
batches in the same transaction as stream apply. Empty delta batches are still
written for cursor-only advances, so restart can prove accumulator coverage to
the durable cursor.

On restart, the secondary loads a relationship-bound snapshot and replays
delta batches from snapshot index to cursor index. If coverage is complete and
all deltas validate, the accumulator is restored without scanning local
storage. If any delta is missing, corrupt, incompatible, or fails apply checks,
the secondary deletes stale optimizer state and falls back to scan-based
reconciliation when reconciliation is next required.

```mermaid
flowchart TD
    A["Streamed transaction arrives"] --> B["Read old VID if needed"]
    B --> C["Coalesce repeated mutations<br/>for the same key"]
    C --> D["Apply final physical mutation"]
    D --> E["Update stream-applied index marker"]
    E --> F["Update flat accumulator bucket<br/>XOR old out / new in"]
    F --> G["Update local KID index<br/>KID -> key"]
    G --> H["Commit local transaction"]

    H --> I{"Snapshot cadence reached?"}
    I -->|"Yes"| J["Persist full accumulator snapshot"]
    I -->|"No"| K["Persist local delta batch"]
    J --> L["Prune covered deltas"]
    K --> M["Restart can replay deltas<br/>from snapshot to cursor"]

    M --> N{"Delta coverage complete?"}
    N -->|"Yes"| O["Restore accumulator<br/>without local scan"]
    N -->|"No"| P["Invalidate optimizer state<br/>scan on next reconciliation"]
```

## Local KID Index

The secondary also maintains a local-only bucketed point index:

```text
KID -> physical key
```

The index is scoped to the same top-level buckets as the flat accumulator and
is stored under never-replicated local DR paths. It lets the secondary repair a
non-empty divergent bucket without scanning unrelated storage because
local-only KIDs can be resolved back to physical keys for delete application.
The index deliberately does not persist VID. When indexed repair loads a
bucket, the secondary reads the indexed local physical keys and recomputes VIDs
from current ciphertext values before comparing the bucket checksum against
the flat accumulator. This makes hot-key value updates avoid local KID-index
rewrites; only key creation and deletion need point-index changes. The index
metadata commit index is therefore a key-set index and may lag the accumulator
value index after value-only batches.

The index is an optimization, not authority. If metadata is absent, stale,
relationship-mismatched, cluster-mismatched, or inconsistent with the
accumulator bucket count/checksum, or if an indexed key no longer resolves to
the expected local value, the secondary invalidates the index and falls back to
full local scan.

After indexed repair, the secondary recomputes the repaired bucket's count and
checksum. If it does not exactly match the checkpoint remote checksum, the
secondary performs a bounded full-bucket remote fetch. If that retry still
cannot prove the bucket, the secondary invalidates the optimizer state and
falls back to the full scan path.

## Resnapshot and Pre-Seed

Resnapshot and pre-seed are lifecycle choices for cases where bounded
reconciliation is too expensive or cannot converge.

Resnapshot is online and protocol driven: the secondary replaces its replicated
data plane with checkpoint-scoped entries fetched from the primary, validates
the resulting checkpoint, preserves local-only paths, and returns to streaming.

Pre-seed is operator assisted: the primary creates relationship-bound
checkpoint artifacts, the operator restores them into a disabled secondary,
the secondary validates the manifest and local-only scrub, and normal DR then
catches up from the seed checkpoint through stream replay or checkpoint
reconciliation.

Pre-seed manifests carry primary-issued provenance. The manifest is signed by
the primary DR transport CA after the final bundle integrity hash and segment
metadata are known. The secondary verifies the signature with the DR transport
CA certificate pinned in the activation token before accepting, staging, or
completing an import.

Neither path changes the authority boundary. The secondary still must bind to
fresh relationship material, reject stale lineage, preserve local-only state,
prove checkpoint convergence, and keep optimizer metadata non-authoritative.
The detailed lifecycle, artifact formats, endpoint surface, and current
evidence are in
[DR_PRESEED_RESNAPSHOT_DESIGN.md](DR_PRESEED_RESNAPSHOT_DESIGN.md).

## Resource Controls

The protocol needs bounded:

- checkpoint artifact retention
- checkpoint build concurrency
- checkpoint build CPU and scan cadence on the primary
- stream buffer and journal retention
- stream initial window and credit updates
- reconcile in-flight work
- per-relationship digest/fetch RPC rate
- per-checkpoint secondary compute, byte, entry, retry, and wall-time budgets
- digest split depth and split count
- fetch request selectors
- fetch response bytes
- apply batch entries and bytes
- adaptive stream apply wait cadence
- wall-clock reconciliation time
- primary-side backpressure when secondaries cannot converge

Tuning inputs must be validated before persistence or runtime apply. Cross
field invariants matter: for example, the primary stream buffer must be at
least as large as the secondary's effective initial stream window.

Capacity modeling must separate three costs: primary checkpoint construction,
primary digest/fetch serving, and secondary reconcile execution. The secondary
coordinates reconciliation, but a massively fragmented reconnect can still
consume primary CPU and I/O through checkpoint materialization, child digest
lookups, fetch serialization, and TLS/gRPC overhead. Production defaults should
therefore include per-relationship admission limits and status counters for
primary checkpoint pressure, not only secondary lag.

On the secondary, reconcile uses a first-class budget ledger rather than only
implicit goroutine or timeout limits. When the ledger is exhausted, the
secondary stops with an observable `budget_exhausted` reason, reports the last
phase and consumed RPC budget in DR status, and waits for retry, operator
tuning, resnapshot, or pre-seed instead of continuing until it harms the local
cluster.

The budget ledger should be checkpoint-scoped and relationship-scoped. At
minimum it should account for:

- wall-clock reconcile time
- range checksum and child digest RPC count
- per-range drill-down fanout caps and coarse proof-backed fetch spans
- fetched bytes and fetched entries
- fetched delete candidates
- local KID-index bucket loads and fallback scans
- apply batches, physical mutations, and delete batches
- retry count by phase
- primary checkpoint artifact build attempts caused by the relationship

The primary status surface exposes checkpoint build/admission, range-checksum,
range-digest, fetch-request, and fetch-response budget counters so expensive
reconnect behavior can be diagnosed from the primary side as well as from the
secondary.

Primary-side pressure must be enforced independently from secondary-side
cooperation. A secondary cannot be allowed to request unbounded child digests
or checkpoint artifacts simply because it has local CPU available. The primary
should cap build concurrency, checkpoint rebuild frequency, digest/fetch RPC
rate, response byte budget, and retained checkpoint artifacts per
relationship. When these limits fire, the secondary receives a bounded failure
and reports the phase and budget that stopped progress.

## Observability

Protocol status should distinguish:

- normal streaming
- journal/buffer replay
- checkpoint reconciliation
- resnapshot fallback
- authz failure
- checkpoint tuple mismatch
- digest/fetch proof failure
- apply failure
- delete safety failure
- budget exhaustion
- backpressure
- HA standby key-transition deferral versus active-node fail-closed transition
  failure

The prototype exposes stream transaction shape, flush reasons, apply and commit
timing, flat-accumulator cursor/snapshot/delta state, local KID index load and
fallback counters, journal replay counters, range task counts, drill-down RPC
and coarse-fetch counters, and lag/apply rates so stress runs can measure both
correctness and optimization behavior.

## Protocol Invariants

- Request relationship IDs are selectors, not authority.
- `lastAppliedIndex` advances only after durable apply.
- Stream replay is used only when coverage from the secondary cursor is proven.
- Checkpoint work is bound to checkpoint ID, commit index, and relationship ID.
- Range selection must be complete before checkpoint advancement.
- A checksum mismatch requires digest drill-down.
- Fetched spans must be cryptographically verified complete before delete
  inference.
- Optimizer state can avoid scans only when relationship, cluster, range
  version, algorithm, snapshot, delta, and bucket proofs validate.
- Optimizer failure degrades to scan/reconcile; it must not produce committed
  convergence by itself.
- HA standby runtime/key-transition deferral is not authority. Active and
  activation paths must reload current replicated state before streaming,
  promotion, or any supported data-serving path.
