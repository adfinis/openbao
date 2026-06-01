# OpenBao Native DR Replication: Architecture Overview

OpenBao Native DR is a cross-cluster, active-passive replication protocol designed from first principles. It intentionally bypasses legacy WAL (Write-Ahead Log) shipping and persistent Merkle trees in favor of ordered change streaming backed by checkpoint-fenced, Dynamo-style set reconciliation. It is strictly fail-closed, prioritizing cryptographic proofs over probabilistic guesses.

## 1. Steady-State Streaming

Normal replication operates as an ordered change stream, fully decoupled from internal storage-engine redo logs or WAL files.

* **Ordered Mutations:** The primary streams physical storage mutations ordered by Raft applied indexes.
* **Flow Control:** Streaming is credit-based; the secondary controls ingress based on its durable disk-apply rate.
* **Buffer & Journal:** Reconnects within a short horizon resume from an in-memory ring buffer or a bounded disk-backed stream journal on the primary.

## 2. Optimized Anti-Entropy Reconciliation

When streaming drops and the journal horizon is exceeded, the system falls back to checkpoint-fenced reconciliation. To avoid an O(N) full-database disk scan on every reconnect, OpenBao projects the hierarchical storage into a flat, index-bound cache.

* **KID/VID Projection:** Storage entries are hashed into Key Identifiers (KID) and Value Identifiers (VID) and assigned to a flat array of deterministic buckets (e.g., 1024 or 4096 top-level ranges).
* **Incremental XOR Accumulators:** During secondary apply, the replicated write path computes deterministic old/new KID/VID contributions, reading the current local value when needed. The secondary XORs the old contribution out and the new contribution in, then commits replicated storage, cursor state, accumulator metadata, and local repair-index updates together.
* **Delta Journaling & Epochs:** The secondary periodically persists this flat bucket array alongside its exact `last_applied_index`, backed by a local accumulator delta journal. If the secondary restarts, it replays the delta journal (an O(K) operation) to fast-forward its cache, bypassing a full O(N) disk rebuild.
* **Mandatory Drill-Down:** During reconciliation, the secondary requests a checkpoint and compares its 1024 local buckets against the primary's checkpoint buckets. Mismatches trigger a targeted fetch of precise child spans.

## 3. Zero-Trust Delete Inference

Because the protocol relies on flat bucket ranges rather than a hierarchical diff tree, deletes must be inferred from the absence of a key.

* **Fetch-Completeness Proofs:** A secondary will not infer a delete purely because a key is missing from a payload. It must first calculate an XOR-of-SHA-256 digest over the fetched span and mathematically prove it perfectly matches the primary's advertised digest.
* **Fail-Closed Finalization:** Only after the fetched set is proven complete are local-only keys safely deleted. The secondary's `lastAppliedIndex` strictly advances only after all apply and delete phases succeed for the locked checkpoint.

## 4. Explicit Promotion Contract

Failover in OpenBao DR is treated as a one-way safety boundary, not an operational black box.

* **Clean vs. Forced Promotion:** The protocol distinguishes between a "clean" promotion (lag 0, stable streaming across a quiesce window) and a "forced" promotion (lagging, disconnected, or reconciling).
* **Quantified Data Loss:** If the secondary is forced to promote while reconciling, the API explicitly requires operators to pass `accept_data_loss=true` and surfaces the estimated missing entries based on index gaps.
* **Lineage Fencing:** There is no automatic failback or bidirectional merge. Promoted clusters generate a new DR lineage and cryptographically reject stale activation tokens or cert fingerprints from the old primary.

---

**Summary:** The design achieves high-throughput steady-state replication via ordered change streaming, narrows gap repair through flat index-bound XOR accumulators, and protects data integrity through strictly proven, checkpoint-fenced boundaries.
