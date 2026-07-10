# Bi-Directional Usage Guide

## Summary

GitOps Reverser can participate in a bi-directional workflow, but only if the write authority is explicit.

This area is still experimental. The first results are promising, but it needs careful design and
operational discipline before it can be treated as a normal default workflow.

The safe default is:

- cluster changes happen through the Kubernetes API
- GitOps Reverser writes those changes back to Git
- Flux or another GitOps controller applies that exact revision in a controlled acknowledgment step

What is not safe is letting GitOps Reverser and a normal always-on GitOps loop continuously reconcile the
same resources at the same time.

It also requires some form of alignment with the normal GitOps operator in the loop, whether that is
Flux, Argo CD, or another controller. Without that coordination model, the system is very easy to
make noisy or unstable.

## Recommended Modes

Use one of these operating models:

### 1. Audit only

- GitOps Reverser captures live changes into Git
- no other controller writes the same path back into the cluster

Best for:

- audit trails
- brownfield discovery
- teams that are not ready for strict GitOps yet

### 2. Human in the loop

- users make a live change in the cluster
- GitOps Reverser commits it
- a human reviews, merges, or promotes the change later

Best for:

- hotfix capture
- migration from cluster-first operations to Git workflows

### 3. Split ownership

- shared application resources stay API-first
- infra or platform resources stay Git-first
- the same fields are not owned by both loops

Best for:

- teams that want both GitOps and interactive operations without shared write ownership

### 4. Controlled bi-directional mode

- GitOps Reverser writes a commit
- the GitOps controller is triggered deliberately
- GitOps Reverser waits until that exact revision is acknowledged

Best for:

- advanced teams that need shared-path workflows and are willing to accept operational complexity
- experiments and tightly controlled rollouts, not broad default adoption yet

## What To Avoid

Do not treat this as:

- "turn on Flux and GitOps Reverser for the same path and walk away"
- "Git is desired state and the cluster is also desired state"
- "two controllers can compete and sanitization will solve it"

Sanitization helps with metadata noise. It does not solve stale desired-state replay or concurrent writers.

## Why Shared Automatic Ownership Breaks Down

The core problem is causality, not YAML formatting.

Example failure mode:

1. Flux has already applied revision `A`.
2. A user changes the live object in the cluster.
3. GitOps Reverser writes revision `B` with that new intent.
4. Before Flux has refreshed its source to `B`, it reconciles again from revision `A`.
5. Flux replays stale desired state into the cluster.
6. GitOps Reverser sees that replay as another live change and may commit again.

That produces:

- confusing history
- stale reverts
- extra commits
- possible loops

This is why the system needs one write path and one acknowledgment path, not two fully autonomous loops.

## Flux Recommendation

For shared paths, treat Flux as a manually triggered applier.

Recommended shape:

- keep the Flux `GitRepository`
- suspend the Flux `Kustomization` for the shared path
- refresh the source on demand
- reconcile the `Kustomization` on demand
- wait until Flux reports that it applied the expected commit SHA
- suspend the `Kustomization` again if that is the steady-state policy

This turns the workflow into:

- API is the interactive write path
- GitOps Reverser is the Git publisher
- Flux is the explicit acknowledgment step

That is a much safer model than two always-on reconcilers.

## Argo CD Recommendation

Argo CD behaves differently enough from Flux that the Flux advice does not carry
over unchanged. Everything below is exercised against a real Argo CD in
[`../test/e2e/argocd_bi_directional_e2e_test.go`](../test/e2e/argocd_bi_directional_e2e_test.go).

### Never enable `selfHeal` on a shared path

Argo CD runs on two clocks:

- **self-heal** reacts to live drift on a **~2 second** backoff
- a **new Git revision** is only noticed on the timed refresh, **120 seconds** by default

So on a shared path, `syncPolicy.automated.selfHeal: true` does not merely risk a
stale replay — it guarantees one. Argo will overwrite the cluster from its cached
revision long before it ever looks at what GitOps Reverser has just committed. The
interactive change is destroyed, and Git history records the change followed by its
own revert.

This is the causality failure described [above](#why-shared-automatic-ownership-breaks-down),
with the timing weighted heavily against you.

### Prefer `ignoreDifferences` for split ownership

The clean way to share a resource with Argo CD is to hand Argo an explicit list of
fields it does not own:

```yaml
spec:
  syncPolicy:
    automated:
      selfHeal: true          # safe now — the field below is invisible to drift detection
    syncOptions:
      - RespectIgnoreDifferences=true
  ignoreDifferences:
    - group: example.com
      kind: IceCreamOrder
      jsonPointers:
        - /spec/scoops        # owned by the Kubernetes API, published by GitOps Reverser
```

The ignored field never registers as drift, so self-heal never fires on it, and
GitOps Reverser is free to publish the live value to Git. `RespectIgnoreDifferences=true`
additionally stops a *legitimate* sync — triggered by some unrelated Git change —
from resetting the field on its way past.

This is [split ownership](#3-split-ownership), made enforceable by the tool rather
than by convention.

### Argo CD writes bookkeeping onto your objects

Argo CD's default resource-tracking method is `annotation`. Its repo-server stamps
every non-CRD object it applies with:

```yaml
metadata:
  annotations:
    argocd.argoproj.io/tracking-id: my-app:example.com/IceCreamOrder:my-ns/order-1
```

That annotation is controller state, not intent, and GitOps Reverser strips it
before writing to Git — as it already did for Flux's `kustomize.toolkit.fluxcd.io/*`
labels and for `kubectl.kubernetes.io/last-applied-configuration`.

This matters more than it looks. Argo CD **never validates a tracking-id against
the object that carries it**. If a manifest carrying a foreign tracking-id is
applied to a cluster by anything other than Argo's own repo-server — `kubectl apply`,
Flux, a promotion pipeline — then the next Argo CD Application to manage that object
sees it as belonging to someone else, raises `SharedResourceWarning`, and **fails to
sync**. Committing the annotation to Git is what arms that trap, which is why it is
stripped.

Sibling annotations under the same prefix — `sync-wave`, `sync-options`,
`compare-options`, `hook` — *are* user intent and are preserved.

### Keep Argo CD on `annotation` tracking

If you set `application.resourceTrackingMethod` to `label` or `annotation+label`,
Argo CD stamps the label `app.kubernetes.io/instance` instead. That key is
indistinguishable from the standard recommended label that Helm and Kustomize set
for entirely legitimate reasons, so GitOps Reverser cannot strip it — and it will be
committed to Git.

For any Reverser-managed path, leave Argo CD on its default `annotation` tracking.

### No encrypted Secrets with Argo CD

Flux `Kustomization` has native `spec.decryption.provider: sops`, so an encrypted
Secret can round-trip: written by GitOps Reverser, decrypted and applied by Flux.

Argo CD has no built-in SOPS decryption; it requires a Config Management Plugin
(ksops, argocd-vault-plugin, …). Encrypted-Secret round-trip is therefore a
**Flux-only capability** in this repository today.

## Practical Platform Guidance

If you are evaluating this as a platform engineer, start with these rules:

- prefer audit-only or split-ownership mode first
- do not let shared resources have two autonomous reconciliation loops
- make the authoritative write path obvious to operators
- document who is allowed to make live changes and when
- keep rollback expectations clear: Git history is useful, but replay timing still matters
- test remote-moved, delete, and controller-restart scenarios before calling the workflow production-ready

Questions to answer before rollout:

- which resources are API-first and which are Git-first?
- who approves live-cluster hotfixes?
- what should happen when the remote branch advances outside GitOps Reverser?
- how will operators detect a stuck or timed-out acknowledgment?
- what metrics and alerts prove the workflow is healthy?

## Practical Engineering Guidance

If you are evaluating this as a Go or controller engineer, focus on:

- idempotency across repeated watch events
- semantic comparison rather than YAML text comparison
- sanitization of controller-added operational metadata
- explicit tracking of pending acknowledgments
- deterministic handling of "remote moved" conditions
- timeout, status, and degraded-mode behavior when Flux does not acknowledge a revision

Good implementation boundaries:

- keep Flux-specific coordination isolated from generic Git write logic
- expose handshake progress in status instead of only in logs
- make replay suppression depend on observed revisions, not on timing guesses alone

## Current Status In This Repository

Today the repository already supports the foundation for this direction:

- reverse writes are optimized for frequent live changes
- the Git path notices when the tracked branch moved externally
- e2e coverage exists for a controlled shared-resource scenario, against both
  Flux and Argo CD
- known Flux **and** Argo CD operational metadata is sanitized so tests focus on
  meaningful diffs

What is not complete yet:

- a stable, well-shaped product story for bi-directional usage
- a finished first-class product surface for manual Flux acknowledgment
- an equivalent acknowledgment surface for Argo CD (the `ignoreDifferences`
  recipe above is a configuration pattern, not a product feature)
- alignment patterns for GitOps operators other than Flux and Argo CD
- HA support for the controller
- full production hardening for all shared-ownership edge cases

## Suggested Rollout Path

Adopt the feature in this order:

1. Run GitOps Reverser in audit-only mode.
2. Move to human-in-the-loop hotfix capture.
3. Use split ownership for real environments.
4. Add controlled bi-directional mode only for paths that truly need it.

This keeps the simplest and most understandable workflows as the default.

## References

For the concrete exercised behavior, see:

- [`../test/e2e/flux_bi_directional_e2e_test.go`](../test/e2e/flux_bi_directional_e2e_test.go)
- [`../test/e2e/argocd_bi_directional_e2e_test.go`](../test/e2e/argocd_bi_directional_e2e_test.go)

Both live in the opt-in bi-directional e2e corner — the only place Argo CD is
installed. Run them with `task test-e2e-bi-directional`, and browse the resulting
Argo CD state with `task argocd-ui`. See:

- [`design/e2e-bi-directional-corner.md`](design/e2e-bi-directional-corner.md)

For broader controller and repository lifecycle background, see:

- [`design/gittarget-lifecycle-and-repo-architecture.md`](design/gittarget-lifecycle-and-repo-architecture.md)
