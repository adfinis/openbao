# DR Replication Failover Design

This note contains promotion, failover, and reseed semantics for native DR
replication. The top-level RFC is [RFC_DR_Replication.md](RFC_DR_Replication.md).

## Authority Model

DR is single-primary while a relationship is active. The primary is the write
authority. Secondaries are warm standby copies that apply replicated state and
remain read-only for replicated data.

Promotion changes authority. A promoted secondary becomes a standalone write
authority for future writes. The old relationship to the original primary is
terminated and must not be resumed or merged automatically.

If the old primary later becomes reachable, it is a separate authority on a
possibly divergent timeline. The protocol must reject stale relationship
material, but operators must still fence client traffic and decide whether to
decommission, rebuild, or reseed the old site.

## Promotion Classes

The design separates three operator situations:

1. **Planned switchover**: the primary is reachable, writes are quiesced, the
   secondary drains stream/reconcile work, and authority transfer can happen
   with a final proof.
2. **Clean disaster promotion**: the primary is unreachable, but the secondary
   still has a stable lag-zero streaming proof across the promotion quiesce
   window.
3. **Forced disaster promotion**: the primary is unreachable and the secondary
   cannot prove clean catch-up. This requires explicit data-loss
   acknowledgement.

The current prototype primarily models clean versus forced promotion. The
planned-switchover wording is useful because many "clean" promotions in
production are likely to be operator-planned rather than true disaster events.

## Promotion Preconditions

Every promotion requires `confirm_primary_unreachable=true` or an equivalent
operator confirmation that this cluster should become authority.

Clean promotion additionally requires:

- secondary state is streaming before the quiesce window
- lag is zero
- no reconciliation is active
- `lastAppliedIndex` remains stable across the quiesce window
- secondary state is still streaming after the quiesce window

If the secondary disconnects, reconnects, enters reconciliation, or cannot keep
the stable proof across the quiesce window, promotion is forced even if the
observed primary/secondary index gap is zero.

Forced promotion requires a second acknowledgement such as
`accept_data_loss=true`.

## Promotion Response Contract

Promotion responses should include enough information for operator audit and
automation:

- `promotion_class`
- `clean_promotion_eligible`
- `clean_promotion_proof_available`
- `forced_promotion_requires_acknowledgement`
- `forced_promotion_reason_codes`
- `forced_promotion_reason_details`
- `data_loss_accepted`
- `last_applied_index`
- `last_known_primary_index`
- `estimated_data_loss_entries`
- `estimated_data_loss_entries_basis`

`estimated_data_loss_entries` is an observed gap estimate, not a clean
promotion proof. A zero estimate still requires forced-promotion
acknowledgement if clean proof is unavailable.

## Promotion Effects

Promotion must:

- stop DR stream and reconciliation activity
- cancel active reconcile contexts
- clear secondary read-only enforcement
- remove the active secondary runtime
- persist promotion lineage
- prevent stale old-primary relationship state from being reused
- leave the promoted cluster with local ownership of future writes

Promotion must not:

- preserve an active relationship to the old primary
- reconnect to the old primary
- merge old-primary writes into the promoted timeline
- transfer non-promoted secondary relationships automatically
- imply automatic failback

## Promotion Lineage

The promoted cluster persists lineage metadata so stale pre-promotion material
cannot revive old authority.

Lineage should include:

- promotion ID
- promotion timestamp
- old primary cluster ID
- old relationship ID
- old secondary certificate fingerprint
- inherited stale primary cluster IDs
- inherited stale relationship IDs
- inherited stale secondary fingerprints
- local cluster ID at promotion time
- last applied index
- last known primary index
- promotion class
- data-loss estimate and basis
- whether data-loss acknowledgement was accepted
- forced-promotion reason codes and details

The lineage record must survive restart, HA active handoff, and repeated
promotion/reseed cycles.

## Stale Material Rejection

A promoted cluster must reject:

- activation tokens referencing stale old-primary cluster IDs
- relationship IDs recorded in stale lineage
- secondary certificate fingerprints recorded in stale lineage
- attempts to reuse revoked or previous relationship credentials

This applies to bootstrap registration and later relationship creation. The
fence is about authority lineage, not merely current relationship state.

## Non-Promoted Secondaries

When one secondary is promoted, other secondaries remain tied to the old
relationship lineage. They do not automatically follow the promoted cluster.

To make a non-promoted secondary follow the promoted authority, operators must
enable DR primary mode on the promoted cluster and explicitly re-enable or
reseed the secondary from a fresh activation token issued by that promoted
authority.

Re-enabling from the promoted authority is a new lineage. The secondary must
clear old replication cursor state, including checkpoint high-water marks, so
it cannot accidentally treat the old relationship as resumable.

## Old Primary Fencing

Promotion is a protocol boundary, not a complete operational fence.

| Scenario | Protocol behavior | Operator responsibility |
| --- | --- | --- |
| Old primary is gone | Promote secondary | Reroute clients |
| Old primary returns after promotion | No auto-merge; stale lineage rejected | Fence, decommission, or rebuild |
| Non-promoted secondary remains on old lineage | It does not follow promoted authority | Reseed from promoted authority |
| Both old and promoted clusters accept writes | Divergent authorities | Choose one authority and rebuild the other |

The protocol should make divergent-authority risk visible. It should not try to
resolve it automatically.

## Reseed From Promoted Authority

Promoted-authority reseed is the supported path to attach another secondary to
the new authority.

The flow is:

1. Promote secondary.
2. Enable DR primary mode on the promoted cluster.
3. Generate a fresh activation token from the promoted cluster.
4. Re-enable or reseed the target secondary.
5. Clear old relationship cursor state on the target.
6. Reconcile from the promoted authority.
7. Verify promoted-only data replicated and old-primary-only data did not
   merge.

## Data-Loss Semantics

Forced promotion is an operator decision to prefer availability of the
secondary timeline over waiting for more proof from the old primary. The
protocol reports the best observed gap, but it cannot prove absence of data
loss once the primary is unavailable and clean proof is missing.

The data-loss estimate basis should be explicit, for example:

- known primary index from last heartbeat/status
- secondary `lastAppliedIndex`
- whether reconciliation was active
- whether checkpoint proof was active or incomplete
- whether stream was connected at promotion time

## Validation Focus

Failover validation should cover:

- clean promotion with stable lag-zero proof
- forced promotion while lagging
- forced promotion while reconciling
- forced promotion with zero estimated gap but no clean proof
- promotion lineage persistence across restart
- stale activation-token rejection after promotion
- stale secondary certificate rejection after promotion
- old-primary-only data not merging after promoted-authority reseed
- promoted-only data replicating to reseeded secondaries
- non-promoted secondary behavior after another secondary is promoted
- active stream/reconcile cancellation during promotion

Current results are summarized in
[DR_VALIDATION_RESULTS.md](DR_VALIDATION_RESULTS.md).
