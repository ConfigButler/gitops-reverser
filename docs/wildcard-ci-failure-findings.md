# Wildcard re-add — CI failure findings

Investigation of the red `main` builds following commit
`8e1b3ab` ("fix: readds support for wildcards"). Two **distinct** failure
signatures showed up across two consecutive `main` runs. They have different
root causes and should be tracked separately.

| | Run A | Run B |
|---|---|---|
| CI run | [`26848671083`](https://github.com/ConfigButler/gitops-reverser/actions/runs/26848671083) | [`26850118478`](https://github.com/ConfigButler/gitops-reverser/actions/runs/26850118478) |
| Commit | `8e1b3ab` — *fix: readds support for wildcards* | `0945809` — *chore(main): release gitops-reverser 0.27.1 (#162)* |
| Failed job | E2E (full) | E2E (full) |
| Result | 1 failed | 5 failed / 14 skipped |
| Signature | GitTarget isolation regression | Controller-availability cascade (WatchRules never reach `Ready`) |
| Determinism | **Deterministic** — reproduced locally | Looks like a timing/bootstrap race — not yet confirmed deterministic |
| Wildcard *expansion* test itself | **passed** | failed (collateral of the cascade) |

Context: every `main` build before `8e1b3ab` was green; `8e1b3ab` is the first
red one. The wildcard *feature* (expansion across core + custom namespaced APIs)
works in both runs where the controller was healthy — what broke is elsewhere.

---

## Failure A — GitTarget isolation regression (deterministic)

### Symptom

Spec: **`Manager GitTarget Isolation › keeps target A's commits as events while
target B's rules churn`** — [test/e2e/gittarget_isolation_e2e_test.go:143](../test/e2e/gittarget_isolation_e2e_test.go#L143).

```
Timed out after 30s.
target A's commit for iso-cm-N must be a [CREATE] event commit
Expected <string>:  to contain substring <string>: [CREATE]
```

The controller log at the moment of failure shows what target A committed
instead of a `[CREATE]` event commit:

```
git commit created  messageKind=snapshot  events=1  message="reconcile: sync 1 resources"
```

Target A was dragged into **rule-change snapshot mode** even though only
target B's rules changed.

### Reproduced locally

`task test-e2e-full` on a fresh local k3d cluster — same single failure as CI:

```
45 Passed | 1 Failed | 0 Pending | 2 Skipped   (46 of 48 specs, 14m21s)
Summarizing 1 Failure:
  [FAIL] Manager GitTarget Isolation
         keeps target A's commits as events while target B's rules churn
  test/e2e/gittarget_isolation_e2e_test.go:143
```

Extra data point: **CI failed on iteration 1** (target B *removes* `services`
from its watch set), **local failed on iteration 0** (target B *adds* `services`).
Different iteration, identical defect → it is **not** add-vs-remove specific.
*Any* change to target B's effective watch plan churns the **global informer
set** and forces every target to snapshot.

### Root cause (hypothesis)

This is exactly the coupling that
[docs/finished/gittarget-isolation-on-rule-change.md](finished/gittarget-isolation-on-rule-change.md)
claimed was removed. That design says snapshot selection must be driven purely
by a **per-target effective-watch-plan hash** (resolved GVR + scope + unioned
operations + destination), with the global `added`/`removed` `force` flag gone.

The wildcard re-add reintroduced the cross-target coupling. Two candidate
mechanisms, both in `internal/watch`:

1. The global GVR add/remove delta is once again reaching
   `snapshotTargetsNeedingDelivery` for targets whose own plan did not change
   (the old `force` path), **or**
2. `currentRuleSetSnapshots()`
   ([internal/watch/manager.go:1233](../internal/watch/manager.go#L1233)) now
   produces an **unstable per-target plan hash** when the catalog / informer set
   churns — e.g. a plain `"configmaps"` rule resolving to a different GVR set
   across catalog refreshes because of the new wildcard resolution paths.

The wildcard commit's changes to the resolver
([internal/watch/rule_gvr_resolver.go](../internal/watch/rule_gvr_resolver.go) —
`resourceCandidates`, `wildcardResourceCandidates`, `choosePreferredVersions`,
`ambiguityMiss`) are the most likely source of (2): they change how candidates
and preferred versions are chosen, which feeds the resolved GVR set that the
per-target hash is computed over.

### Where to look

- [internal/watch/manager.go:739](../internal/watch/manager.go#L739) — `ReconcileForRuleChange`
- [internal/watch/manager.go:1177](../internal/watch/manager.go#L1177) — `snapshotTargetsNeedingDelivery`
- [internal/watch/manager.go:1233](../internal/watch/manager.go#L1233) — `currentRuleSetSnapshots` (per-target plan hash)
- [internal/watch/rule_gvr_resolver.go](../internal/watch/rule_gvr_resolver.go) — resolution changes from `8e1b3ab`

Suggested first step: assert the per-target plan hash for a target whose own
rules are unchanged is **stable** across an unrelated target's rule change
(unit-level), then trace whether a global delta still reaches snapshot
selection.

---

## Failure B — controller-availability cascade (timing/bootstrap)

### Symptom

5 specs failed, **all with the same assertion** at
[test/e2e/e2e_test.go:194](../test/e2e/e2e_test.go#L194) inside `verifyResourceStatus`:

```
Timed out after 90s.
status.conditions not found
Expected <bool>: false to be true
```

i.e. a `WatchRule`/`ClusterWatchRule` was created but **never got any status
written** within 90s — it never reached `Ready` because nothing reconciled it.

| Failing spec | Resource stuck without status |
|---|---|
| Manager WatchRule … *should expand wildcard resources …* | `watchrule/watchrule-wildcard-expansion-test` |
| Commit Signing … *snapshot commit with custom template* | `watchrule/signing-snapshot-wr` |
| Commit Request `[BeforeAll]` … | `watchrule/commit-request-watchrule-*` |
| Bi Directional … *avoid a commit loop* | `watchrule/bi-watchrule-*` |
| Restart Snapshot Safety … *git mirror intact on restart* | `clusterwatchrule/restart-snapshot-wildcard` |

The `[BeforeAll]` failure in *Commit Request* is what skips ~14 specs (an
ordered container whose `BeforeAll` fails skips all its specs).

### Root cause (evidence points to controller unavailability, not assertion logic)

The early specs **passed** — `Manager Controller Basics` (run successfully,
service exposed, metrics serving, audit webhook events) and several
`Manager CRD Lifecycle` specs all went green at ~21:58. So the controller
(`gitops-reverser-6969c78bb5-bwj9p`) was healthy and reconciling at the start.

Then a **second controller pod replaced the first** mid-run, and the replacement
got stuck on a TLS-bootstrap mount race:

```
Killing   pod/gitops-reverser-6969c78bb5-bwj9p   Stopping container manager
SuccessfulCreate  replicaset/gitops-reverser-6969c78bb5  Created pod: ...-kzpj5
Warning  FailedMount  pod/gitops-reverser-6969c78bb5-...  \
  MountVolume.SetUp failed for volume "audit-webhook-certs": secret "audit-server-cert" not found
```

While the replacement pod could not mount `audit-server-cert` (cert-manager had
not issued/published it yet), the controller was **not reconciling WatchRules**.
Every spec that created a fresh WatchRule during that window timed out at 90s
waiting for `status.conditions`. The same `audit-server-cert` / audit-root-ca
bootstrap warnings also appear (benignly, self-healing) in Run A's event dump,
so this is a pre-existing startup race that simply landed badly in Run B.

Note: the controller log in this run is **flooded** with a benign, unrelated
retry — `Failed to build pending write; dropping open window … get GitProvider:
… "gitprovider-normal" not found` (a branch worker retrying against an
already-cleaned-up namespace). It is noise, not the cause.

### Why this is *not* (clearly) the wildcard code

- The failing specs are a mix — some involve wildcards/CRDs, some don't
  (signing, commit-request). They are unified only by *timing* (all created
  WatchRules during the unavailable window), not by a shared code path.
- The mechanism is "controller wasn't running/ready," not "reconcile produced
  the wrong result."
- The wildcard expansion spec passed in Run A.

It is *possible* the wildcard work lengthens initial reconcile/discovery (a fresh
wildcard plan expands across all served GVRs), widening the vulnerable window —
but that is unconfirmed.

### Where to look / next step

- Confirm determinism: re-run E2E (full) on the same commit. If Run B's cascade
  does **not** reproduce, it is an infrastructure/bootstrap flake to harden, not
  a logic bug.
- Controller startup ordering vs `audit-server-cert`: the manager should not be
  killed/rolled while the cert secret is unpublished, or should tolerate the
  mount gap without dropping reconciliation. See the audit TLS design:
  [docs/design/audit-webhook-tls-design.md](design/audit-webhook-tls-design.md).
- Investigate **why a second controller pod was created mid-run** in Run B
  (rollout / eviction / cert rotation) — that replacement is the trigger.

---

## Failure C — local only: disk-pressure node taints (environment, not code)

Hit while reproducing Failure A locally. Not a product bug — recorded so the
next person does not lose time to it.

### Symptom

`task prepare-e2e` failed at the `portforward-ensure` step:

```
⏳ Waiting for Prometheus pod to be ready...
error: timed out waiting for the condition on pods/prometheus-prometheus-shared-e2e-0
❌ Prometheus pod failed to become ready
NAME                                 READY   STATUS    RESTARTS   AGE
prometheus-prometheus-shared-e2e-0   0/2     Pending   0          4m11s
```

The controller pods were `Pending`/`Error` too, with:

```
FailedScheduling  0/4 nodes are available: 4 node(s) had untolerated taint(s).
```

### Root cause

All four k3d nodes carried `node.kubernetes.io/disk-pressure:NoSchedule`:

```
NAME                                    TAINTS
...-agent-0   [.../disk-pressure ...]
...-server-0  [.../disk-pressure ...]
```

The host Docker overlay was **94% full** (`231G / 248G`), tripping kubelet's
`imagefs`/`nodefs` eviction threshold. `docker system df` showed the hog:
~169 GB reclaimable images and ~25 GB build cache.

### Fix / workaround

```
docker builder prune -f     # reclaimed ~25 GB build cache
docker image prune -f       # dangling images
```

Disk dropped 94% → 82% → 33% free, the `disk-pressure` taints cleared on their
own, the cluster recovered, and `task test-e2e-full` then ran to completion
(surfacing Failure A). A leftover second cluster (`audit-pass-through-e2e`) was
also consuming disk and was a contributing factor.

### Takeaways

- Local full-suite runs need real disk headroom. The failure does **not**
  present as "disk full" — it shows up as `Pending` pods and
  `FailedScheduling … untolerated taint(s)`, which is easy to misread as a
  scheduling/resource-request problem.
- If pods won't schedule, check `kubectl get nodes -o custom-columns=NAME:.metadata.name,TAINTS:.spec.taints`
  for `disk-pressure` before anything else, and `df -h /` + `docker system df`.
- Tear down stale k3d clusters between runs; they hold image/volume disk.

---

## Summary

- **Failure A is the real, deterministic regression from the wildcard re-add** —
  GitTarget isolation no longer holds; an unrelated target's rule change forces
  other targets into snapshot commits. Reproduced locally. Fix lives in
  `internal/watch` snapshot-selection / per-target plan hash.
- **Failure B is a controller-availability cascade** triggered by a mid-run
  controller pod replacement blocked on the `audit-server-cert` mount. It is a
  bootstrap/timing problem (pre-existing race), not an assertion-logic bug, and
  should be confirmed with a re-run before assuming it is caused by the wildcard
  change.
- The wildcard **expansion feature itself is not implicated** in either failure.
- **Failure C is local-environment only** — disk-pressure node taints from a full
  host Docker overlay. Not a product bug; clear disk before running the suite.
