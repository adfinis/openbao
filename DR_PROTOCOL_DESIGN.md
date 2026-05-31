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

A checkpoint is an immutable primary-side view of replicated storage at a
specific commit index.

Checkpoint identity is:

- checkpoint ID
- checkpoint commit index
- relationship ID

Every reconciliation RPC is bound to that tuple. The secondary rejects
checksum, digest, fetch, or delete work that does not match its active
checkpoint tuple.

The primary serves `FetchEntries` from checkpoint artifacts rather than live
storage. This prevents live write races from corrupting reconciliation. If a
key disappears while a checkpoint artifact is being built, it is omitted from
the checkpoint live range set and can later be deleted as a proven local-only
key.

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
2. Secondary obtains local KID/VID state from a trusted accumulator/index fast
   path or a full local scan.
3. Secondary selects ranges requiring verification.
4. Primary and secondary compare top-level range checksums.
5. Mismatched ranges enter mandatory digest drill-down.
6. Primary returns child digests that fully cover each parent span.
7. Secondary fetches mismatched spans from checkpoint artifacts.
8. Secondary validates fetched content against advertised digest proofs.
9. Secondary applies fetched primary entries.
10. Secondary deletes local-only keys only after fetched remote spans are
    proven complete.
11. Secondary advances `lastAppliedIndex` only after every phase succeeds.

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
        S->>P: FetchEntries(mismatched spans)
        P->>CS: Fetch checkpoint-scoped entries
        P-->>S: Entry batch + digest metadata
        S->>S: Recompute digest and prove completeness
        S->>LS: Apply fetched primary puts
        S->>LS: Delete local-only keys after proof
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
as the complete primary set for those spans.

## Delete Inference

Range fetches list primary-present entries. They do not naturally list
secondary-local keys that no longer exist on the primary.

After fetch completeness is proven, the secondary compares local KIDs in the
fetched spans with the proven remote KID set. Local KIDs absent from the remote
set become candidate deletes. Deletes are applied after fetched puts complete
and before checkpoint finalization.

Absence is authority only after proof. This is the key safety boundary for
delete inference.

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

## Resnapshot

Resnapshot is an explicit fallback for cases where bounded reconciliation
cannot converge or the operator wants a fresh base copy. It still uses
checkpoint-fenced fetches and proof validation. It is not a way to bypass
delete proof or local-only path exclusion.

Before non-atomic reconciliation repair or resnapshot mutation, the secondary
invalidates persisted accumulator/index state. If it crashes mid-repair,
restart cannot trust stale optimizer metadata for partially repaired storage.

## Resource Controls

The protocol needs bounded:

- checkpoint artifact retention
- checkpoint build concurrency
- stream buffer and journal retention
- stream initial window and credit updates
- reconcile in-flight work
- digest split depth and split count
- fetch request selectors
- fetch response bytes
- apply batch entries and bytes
- wall-clock reconciliation time
- primary-side backpressure when secondaries cannot converge

Tuning inputs must be validated before persistence or runtime apply. Cross
field invariants matter: for example, the primary stream buffer must be at
least as large as the secondary's effective initial stream window.

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

The prototype exposes stream transaction shape, flush reasons, apply and commit
timing, flat-accumulator cursor/snapshot/delta state, local KID index load and
fallback counters, journal replay counters, range task counts, and lag/apply
rates so stress runs can measure both correctness and optimization behavior.

## Protocol Invariants

- Request relationship IDs are selectors, not authority.
- `lastAppliedIndex` advances only after durable apply.
- Stream replay is used only when coverage from the secondary cursor is proven.
- Checkpoint work is bound to checkpoint ID, commit index, and relationship ID.
- Range selection must be complete before checkpoint advancement.
- A checksum mismatch requires digest drill-down.
- Fetched spans must prove completeness before delete inference.
- Optimizer state can avoid scans only when relationship, cluster, range
  version, algorithm, snapshot, delta, and bucket proofs validate.
- Optimizer failure degrades to scan/reconcile; it must not produce committed
  convergence by itself.
