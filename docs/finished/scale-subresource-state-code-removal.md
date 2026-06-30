# Removing the dead scale-subresource *state* code

> Status: implemented.
> Date: 2026-06-30.
> Scope: pure dead-code deletion — no behavior change. `task fmt`, `task vet`,
> `task lint` (0 issues), and `task test` pass.
> Related: [Data Sources](../architecture.md#data-sources),
> [Optional Attribution](../architecture.md#optional-attribution),
> [Partial-object audit events](./partial-object-audit-event-handling.md) (describes the
> now-removed audit-as-state consumer).

## Summary

When the project moved to **watch-first ingestion**, watch became *the only object-state
source* and the old **audit-as-state** pipeline (`internal/queue/redis_audit_consumer.go`
— `extractObject` → `routeAuditEvent` → `RouteToGitTargetEventStream`) was deleted. A
`/scale` write used to be interpreted by that consumer: it read the accepted replica count
out of the `Scale` response object and wrote it onto the parent's `.spec.replicas`. Once
watch owns state that interpretation is unnecessary — a `kubectl scale` mutates the parent
Deployment's `spec.replicas` *in place* and bumps its `resourceVersion`, so the parent's
own watch event already carries the new value.

The consumer was removed, but its **scale helpers were left behind** as orphans. This
change deletes them.

## What was removed, and why each was dead

| Symbol | File | Why it was dead |
|---|---|---|
| `BuiltinScaleReplicasPath(group, resource)` + its two tests | `internal/auditutil/subresource_policy.go`, `_test.go` | Zero production callers. It returned the parent replica field path (`["spec","replicas"]`) for the deleted consumer to write. Watch carries the value now, so nothing asks for the path. |
| `SplitFieldPath(path)` + `TestSplitFieldPath` | `internal/typeset/scale.go`, `scale_test.go` | Sole purpose was to bridge a `ScaleBinding`'s dotted path string into the writer's `[]string` — its doc said so. Its only caller was `BuiltinScaleReplicasPath`. Removing that orphaned it. |
| The whole `subresource_policy.go` / `subresource_policy_test.go` | `internal/auditutil/` | After the two functions above were gone, the file held only `IsScaleSubresource(s) == (s == "scale")`, used in exactly one place: `shouldForwardSubresource` in the webhook. The literal was inlined there (its comment already documents the policy), so the file — and a package-level essay for a one-liner — was deleted. |

`auditutil` survives: the webhook still uses `auditutil.VerbToOperation`.

## What survives, and why

The `/scale` special-case in
[`shouldForwardSubresource`](../../internal/webhook/audit_handler.go) stays — but it is now
purely an **attribution forwarding gate**, not a state path. A `/scale` write surfaces in
the audit stream only under the parent's `…/scale` subresource, so the handler forwards
just that one subresource to name *who* scaled the parent (joined to the parent watch event
by `resourceVersion`); every other subresource — `status`, `exec`, `log`, `proxy`, … — is
dropped before the attribution index. With attribution disabled, a scale still mirrors
correctly through the parent watch; the commit is simply committer-authored.

`typeset.BuiltinScale` also survives: the followability funnel
([`scaleCheck`](../../internal/typeset/funnel.go)) reads its `Enabled` / `Usable` flags to
decide whether a scalable parent is followable. It no longer reads the *path*.

## Things learned

1. **A scale never needs subresource interpretation under watch.** The apiserver's `/scale`
   handler writes `spec.replicas` onto the parent object itself and bumps its
   `resourceVersion`. The operator watches `apps/v1/deployments`, receives the full
   `MODIFIED` parent, sanitizes, diffs, and commits. The dedicated e2e proves exactly this —
   its title is "mirrors kubectl scale **through the parent Deployment watch**"
   ([deployment_scale_subresource_e2e_test.go](../../test/e2e/deployment_scale_subresource_e2e_test.go)).

2. **Removing one orphan cascades.** Deleting `BuiltinScaleReplicasPath` made
   `SplitFieldPath` dead, which in turn left `ScaleBinding`'s richer fields write-only. The
   honest cleanup follows the chain rather than stopping at the first symbol.

3. **`ScaleBinding` is now mostly write-only — a noted follow-up, left intact here.** In
   production only `Enabled` and `Usable` are read (by the followability funnel). The
   reporting/state fields — `SpecReplicasPath`, `ResponseGVK`, `Source`,
   `StatusReplicasPath`, `SelectorPath`, `SelectorKind` — are set by `BuiltinScale` and
   asserted by typeset unit tests, but no production code reads them. Collapsing
   `ScaleBinding` to `{Enabled, Usable}` is a reasonable simplification, but it touches the
   typeset "fact" model and its corpus expectations, so it was deliberately **not** done in
   this behavior-neutral pass.

4. **The surviving `/scale` gate's one purpose — attribution — had no e2e coverage, now
   added.** This surfaced while checking the cleanup, and the gap is closed by
   [deployment_scale_author_attribution_e2e_test.go](../../test/e2e/deployment_scale_author_attribution_e2e_test.go):
   - **Unit:** covered. `audit_handler_test.go` asserts a `/scale` event forwards, a
     non-scale subresource is dropped, and a captured real `kubectl scale deployment`
     recording is recorded as an attribution fact
     (`TestAuditHandler_ForwardsRealScaleSubresourceRecording`).
   - **e2e (was a gap):** the dedicated scale e2e asserts only the *replica value* in git,
     never the commit author; the attribution e2e
     ([commit_author_attribution_e2e_test.go](../../test/e2e/commit_author_attribution_e2e_test.go))
     never scales. So the end-to-end join *scale fact → resolver → commit authored by the
     scaler* was never exercised. The new spec runs a literal `kubectl scale deployment …
     --replicas=3 --as=<human> --as-group=system:masters` and asserts both that the parent
     manifest reaches `replicas: 3` (proving state flows through the parent watch) **and**
     that the resulting commit is authored by the human who scaled.
   - The e2e **infrastructure already supported** it: the audit policy
     ([cluster/audit/policy.yaml](../../test/e2e/cluster/audit/policy.yaml)) captures a human
     `*/scale` at `RequestResponse` (the rule-3 catch-all) and drops the HPA controller's
     `apps/*/scale` at `level: None` (rule 2).
   - One constraint worth recording: kubectl's CLI cannot set `user.extra`, so the new spec
     attributes a **bare username** (`grace-hopper` → `grace-hopper@noreply.cluster.local`
     via `ConstructSafeEmail`), not the richer OIDC display-name/email form. The
     claims-enrichment path stays covered by the impersonation-based ConfigMap attribution
     e2e, which uses client-go's `ImpersonationConfig` to carry the claims kubectl can't.

## References

- [internal/auditutil/](../../internal/auditutil/) — `subresource_policy.go` removed;
  `VerbToOperation`, `identity`, `objectref` remain.
- [internal/typeset/scale.go](../../internal/typeset/scale.go) — `SplitFieldPath` removed;
  `BuiltinScale` / `ScaleBinding` remain.
- [internal/webhook/audit_handler.go](../../internal/webhook/audit_handler.go) —
  `shouldForwardSubresource`, the attribution-only `/scale` gate.
- [docs/architecture.md](../architecture.md#data-sources) — watch is the only object-state
  source; audit is optional attribution.
