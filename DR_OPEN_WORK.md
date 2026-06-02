# DR Replication Open Work

This document tracks work that remains after the current local DR replication
prototype. It separates protocol correctness from production readiness,
availability, validation, and optimization.

The bug tracker remains [DR_BUG_TRACKER.md](DR_BUG_TRACKER.md). That file is
for discovered bugs and suggested worktree splits. This file is for remaining
product and engineering work.

## Current Posture

The prototype has strong local evidence for terminal data correctness across
streaming, journal replay, out-of-horizon reconciliation, persisted flat
accumulators, local KID-index repair, promotion, promoted-authority reseed,
runtime refresh, security boundaries, and HA active handoff.

The current production posture should still be strict warm standby:

- secondaries apply and verify replicated state;
- pre-promotion secondary data APIs are not part of the supported surface;
- terminal secondary correctness is proven through checkpoint verification;
- promotion is explicit, one-way, and lineage-fenced.

## P0 Before Production Direction

These are the highest-value items before treating the design as production
shape rather than local PoC.

| Area | Status | Why it matters |
|---|---|---|
| Reconcile budget ledger | Unit-gated, clustered HA negative-path smoke passed | A massive fragmented checkpoint repair must stop on explicit byte, entry, RPC, retry, and wall-time budgets instead of relying on implicit timeouts. Primary rejection paths and production default sizing still need more evidence. |
| Primary checkpoint pressure controls | Unit-gated, HA validation open | A secondary should not be able to force unbounded checkpoint construction, digest lookup, fetch serialization, or artifact retention on the primary. |
| Pre-seed artifact provenance | Open | Large base-copy artifacts need signed provenance or equivalent primary authorization, external storage support, and resumable transfer semantics. |
| DR transport CA rotation | Open | Leaf renewal is covered, but production CA rotation without relationship replacement is not finalized. |
| Rolling upgrade/protocol compatibility | Open | Protocol versions, artifact formats, accumulator metadata versions, and range versions need explicit compatibility rules. |
| Production defaults | Open | Journal retention, checkpoint retention, fetch sizes, split depth, adaptive batching, and resource budgets need measured defaults. |

## Security Follow-Up

The P0 security slices implemented so far cover bootstrap token verifier
storage, unauthenticated response oracles, gRPC relationship authz,
`SyncKeyring` crypto binding, resource-exhaustion ordering on public
bootstrap/rotation paths, and revocation fail-closed behavior.

Remaining security work:

- finalize DR transport CA rotation;
- add production artifact signing/provenance for pre-seed;
- extend resource-abuse tests to primary checkpoint and digest/fetch pressure;
- verify audit redaction for any new artifact/provenance endpoints;
- review rolling upgrade behavior for stale credential and stale artifact
  acceptance; and
- run an independent security review before any upstream-quality rewrite.

## Availability and HA

Current HA evidence is good for terminal convergence, but client-facing
transient failures around active handoff remain availability signals outside
the DR correctness boundary.

Remaining availability work:

- define availability SLOs for primary active handoff under DR backlog;
- separate direct-node, HAProxy, and real load-balancer profiles;
- model secondaries offline for days versus stream journal retention;
- add long outage/resnapshot/pre-seed decision guidance;
- validate standby key-transition deferral across more restart and unseal
  permutations; and
- decide whether planned switchover belongs in the first production version.

## Runtime and Engine Coverage

The engine/runtime matrix has covered namespaces, KV v1/v2, transit, PKI, SSH,
TOTP, database, userpass, AppRole, cert, JWT, token roles, policies, and
identity in the local self-contained profile.

Remaining runtime work:

- add dependency-backed engine profiles for services not available in the
  default compose topology;
- add a deterministic audit-device topology profile;
- validate more namespace lifecycle edge cases;
- keep runtime refresh ordered after commit and before promotion/read-serving;
  and
- avoid making secondary API read-serving an implicit requirement.

## Performance and Scale

The current performance direction is in
[DR_PERFORMANCE_NOTES.md](DR_PERFORMANCE_NOTES.md).

The next scale work should focus on:

- primary rejection paths and production resource-budget defaults;
- secondary transaction pressure and local metadata write amplification;
- pre-seed external artifact lifecycle for large existing clusters;
- optimizer seeding after pre-seed/resnapshot;
- larger dataset fixtures beyond the current 100k evidence; and
- clustered validation and default tuning for fragmented drill-down fanout caps.

## Validation Backlog

Recommended next validation targets:

- primary checkpoint/digest/fetch rejection evidence beyond unit tests, if
  those limits become operator-tunable for clustered negative-path smokes;
- default-duration Go-harness outage/reconcile smokes after the next
  implementation slice;
- one-hour no-stepdown stream-apply baseline on current code;
- larger fixture-backed pre-seed with active post-export writes;
- WAN latency and packet-loss profile;
- proxy/load-balancer profile;
- dependency-backed engine matrix; and
- production artifact provenance negative tests once that design exists.

## Review Questions Still Worth Asking

The RFC should keep only the review questions that change design direction:

1. Is strict warm standby the correct first serving model?
2. Should planned switchover ship with the first version or follow disaster
   promotion?
3. What rolling-upgrade compatibility guarantee is required?
4. What is the acceptable DR transport CA rotation lifecycle?
5. What production artifact/provenance model is required for pre-seed?
6. What default resource budgets are acceptable for very large clusters?
