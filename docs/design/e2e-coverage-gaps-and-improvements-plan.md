# E2E Coverage Gaps & Improvements — plan

> Status: PROPOSAL — 2026-06-26. **Architecture-led**: [architecture.md](../architecture.md) is the
> spine. This plan reads the current e2e suite against the architecture's load-bearing guarantees,
> records where the suite is strong, names the gaps, and proposes a small, prioritized set of new
> end-to-end tests. It also captures the open product questions the investigation surfaced — some of
> these are decisions, not just tests.

## Summary

The e2e suite (30+ files under [test/e2e/](../../test/e2e/)) is broad and genuinely good: attribution,
signing, encryption/SOPS, commit windows + CommitRequests, scale subresource, CRD lifecycle,
bi-directional/no-loop with Flux, aggregated API servers, GitTarget isolation/overlap, and restart
safety are all exercised against a real cluster and a real Gitea.

Reading it against the architecture surfaced three gaps, in increasing order of importance:

1. **In-place comment preservation is proven only on the live path**, not through a resync /
   controller-restart, and the encryption boundary (comments dropped on re-encrypt) is unpinned.
   *(This is the "keep comments in an existing file" idea — already mostly covered; small extensions.)*
2. **Mark-and-sweep of a delete that happened while no watch was running has no e2e proof.** The
   architecture calls this "the heart of the system," yet only the *safe* half (don't over-delete on
   restart) is tested. This is the biggest gap.
3. **The operator does not actually refuse an "unsupported folder" today.** The acceptance gate that
   would refuse hard-Kustomize / unsafe content exists but is wired only into the CLI, not the running
   controller, and there is no GitTarget status condition for it. The requested "we refuse" test cannot
   pass until the behavior is implemented — this is an **implementation gap, not just a test gap**.

***

## 1. What the suite already covers (the map)

| Theme | Representative e2e | Status |
|---|---|---|
| Watch ingestion (create/update) | [watchrule_configmap_secret](../../test/e2e/watchrule_configmap_secret_e2e_test.go), [crd_lifecycle](../../test/e2e/crd_lifecycle_e2e_test.go) | ✅ |
| Live delete → file removal | [crd_lifecycle](../../test/e2e/crd_lifecycle_e2e_test.go), [aggregated_apiserver](../../test/e2e/aggregated_apiserver_e2e_test.go) | ✅ |
| Attribution from audit identity | [commit_author_attribution](../../test/e2e/commit_author_attribution_e2e_test.go), [impersonation](../../test/e2e/impersonation_test.go) | ✅ |
| Commit signing (generated + BYOK) | [signing](../../test/e2e/signing_e2e_test.go) | ✅ |
| Encryption / SOPS Secrets | [watchrule_configmap_secret](../../test/e2e/watchrule_configmap_secret_e2e_test.go), [bi_directional](../../test/e2e/bi_directional_e2e_test.go) | ✅ |
| Commit windows + CommitRequest | [commit_window_batching](../../test/e2e/commit_window_batching_e2e_test.go), [commit_request](../../test/e2e/commit_request_e2e_test.go) | ✅ |
| Scale subresource → `spec.replicas` | [deployment_scale_subresource](../../test/e2e/deployment_scale_subresource_e2e_test.go) | ✅ |
| Aggregated API server (LIST fallback) | [aggregated_apiserver](../../test/e2e/aggregated_apiserver_e2e_test.go) | ✅ |
| GitTarget isolation / path overlap | [gittarget_isolation](../../test/e2e/gittarget_isolation_e2e_test.go), [gittarget_overlap](../../test/e2e/gittarget_overlap_e2e_test.go) | ✅ |
| GitProvider validation / rejection | [gitprovider_validation](../../test/e2e/gitprovider_validation_e2e_test.go) | ✅ |
| Bi-directional / no commit loop | [bi_directional](../../test/e2e/bi_directional_e2e_test.go) | ✅ |
| In-place edit + comment preservation (live) | [inplace_edit](../../test/e2e/inplace_edit_e2e_test.go) | ✅ (live only) |
| Restart safety — **don't over-delete** | [restart_reconcile](../../test/e2e/restart_reconcile_e2e_test.go) | ✅ |
| Restart safety — **reconcile a missed delete** | — | ❌ **gap (2)** |
| Comment preservation through resync/restart | — | ❌ **gap (1)** |
| Refuse unsupported / unsafe folder content | — | ❌ **gap (3) — not implemented** |

***

## 2. Findings

### 2.1 In-place comment preservation is already proven — on the live path only

[inplace_edit_e2e_test.go](../../test/e2e/inplace_edit_e2e_test.go) already does the cool thing: it
seeds a hand-authored comment into a committed file and proves the operator edits the value **in place
while preserving the comment**, plus a fixture-folder case with multi-document + nested files where
sibling documents survive untouched.

- Single ConfigMap: [inplace_edit_e2e_test.go:84](../../test/e2e/inplace_edit_e2e_test.go#L84) — seeds
  `# gitops-reverser-e2e: preserve-this-comment`, confirms a comment-only change is treated as
  semantically equal (no rewrite), then `color: blue→green` and asserts comment survives + value
  changes.
- Fixture folder (multi-doc + nested): [inplace_edit_e2e_test.go:211](../../test/e2e/inplace_edit_e2e_test.go#L211)
  — patches one document and asserts sibling documents and a nested file survive, and no canonical
  duplicate is created.

The mechanism is yaml.v3 node-level editing that copies Head/Line/Foot comments
([manifestedit/merge.go:299](../../internal/git/manifestedit/merge.go#L299)) with byte-preserving
document splitting ([manifestedit/split.go](../../internal/git/manifestedit/split.go)). It is also
unit-tested (`internal/git/manifestedit/comments_chomp_test.go`,
`internal/git/inplace_edit_test.go`).

**What is NOT proven end-to-end:**

- **Resync / mark-and-sweep apply path.** The live (M7) path is covered; the resync (M8) apply path
  uses the same merge code but is never exercised with comments e2e.
- **Across a controller restart.** A startup reconcile rebuilds from the `sendInitialEvents` replay; no
  test proves hand-authored comments survive that reconcile.
- **Encryption boundary.** Sensitive resources are re-encrypted wholesale (never patched in place — see
  [plan_flush.go](../../internal/git/plan_flush.go)), so comments are **dropped by design**. That sharp
  edge is currently unpinned by any test, so a future change could silently start/stop preserving them
  without a signal.

### 2.2 Mark-and-sweep of a missed delete — the biggest gap

The architecture is explicit that this is the load-bearing guarantee:

> **Not losing changes.** A dropped or late event must never leave Git *permanently* wrong — in
> particular, a delete that happens while no watch is running must still be reconciled.
> — [architecture.md → Mental Model](../architecture.md#mental-model)

> This mark-and-sweep is **load-bearing and fires only on watch re-establishment** … It is the only
> thing that reconciles a delete that happened while no watch was running.
> — [architecture.md → State Ingestion](../architecture.md#state-ingestion-and-not-losing-deletes)

The e2e suite tests the **safe half only**: [restart_reconcile_e2e_test.go](../../test/e2e/restart_reconcile_e2e_test.go)
proves a plain restart does **not** wrongly delete previously committed files (the "easy-to-miss
failure mode," per its own comment at [restart_reconcile_e2e_test.go:65](../../test/e2e/restart_reconcile_e2e_test.go#L65)).

It does **not** test the other half — the entire reason the design is watch-first with a replay sweep:

> **stop the operator → delete a watched object → restart → the replay + mark-and-sweep must remove the
> now-orphaned Git file.**

Live deletes (operator running, normal `DELETED` event) are covered in
[crd_lifecycle](../../test/e2e/crd_lifecycle_e2e_test.go); the missed-delete-during-downtime path is
not. This is the highest-value test to add.

### 2.3 "Refuse an unsupported folder" — the operator does not refuse today

The acceptance gate that classifies content into refusals (duplicate identities, impure managed files,
standalone non-KRM, unwatched/out-of-scope KRM, hard-Kustomize) is real and well unit-tested:

- Gate logic: [manifestanalyzer/acceptance.go](../../internal/manifestanalyzer/acceptance.go) — `Accept`
  returns `Accepted: len(issues) == 0` ([acceptance.go:160](../../internal/manifestanalyzer/acceptance.go#L160)).
- Hard-Kustomize detection: [manifestanalyzer/store.go:676](../../internal/manifestanalyzer/store.go#L676)
  `hasUnsupportedKustomizeFeature` (patches, generators, components, helmCharts, replacements,
  namePrefix/Suffix, transformers, configurations).
- Unit coverage: `internal/manifestanalyzer/acceptance_test.go` (all refusal kinds), and
  `contextual_namespace_corpus_test.go` for the unsupported-Kustomize corpus.

**But the gate is wired only into the CLI, not the controller.** The live writer builds its store with
an empty allowlist and never calls `Accept()`/`Scan()`:

- [plan_flush.go:95](../../internal/git/plan_flush.go#L95) — `BuildStoreFromFiles(..., Allowlist{})`;
  the comment at [plan_flush.go:94](../../internal/git/plan_flush.go#L94) states the acceptance gate is
  "applied upstream, not here" — but no upstream caller in the controller path applies it.
- [resync_flush.go:438](../../internal/git/resync_flush.go#L438) — `BuildPlan` directly, no acceptance.
- Only callers of `Scan()` (which runs `Accept`): [scan.go:52](../../internal/manifestanalyzer/scan.go#L52)
  and the CLI [cmd/manifest-analyzer/main.go](../../cmd/manifest-analyzer/main.go).
- There is **no GitTarget status condition or reason** for "unsupported content in path"
  ([api/v1alpha2/gittarget_types.go](../../api/v1alpha2/gittarget_types.go) has no such reason).

Net effect today: a hard-Kustomize folder is **detected but not refused** — namespace resolution
degrades and the operator keeps writing. The design intent (`docs/finished/current-manifest-support-review.md`)
is the opposite: "fail the GitTarget with a clear status and reconcile nothing until the folder is
cleaned." So the requested e2e test cannot be green until the refusal is implemented and surfaced.

***

## 3. Proposed e2e tests

Prioritized. Each test names the architecture guarantee it pins, the setup, and the key assertions.

### Test A — Mark-and-sweep reconciles a delete that happened while the operator was down  *(priority 1)*

**Pins:** [architecture.md → State Ingestion and Not Losing Deletes](../architecture.md#state-ingestion-and-not-losing-deletes)
— the "heart of the system." Complements (does not duplicate)
[restart_reconcile_e2e_test.go](../../test/e2e/restart_reconcile_e2e_test.go), which only proves the
*don't-over-delete* direction.

**Sketch** (new file `test/e2e/missed_delete_sweep_e2e_test.go`, `Serial`/`Ordered`):

1. GitProvider + GitTarget + a WatchRule (ConfigMaps, or IceCreamOrder CRD to match the restart test's
   shape).
2. Create two watched objects; wait until **both** files are committed to Git.
3. **Scale the controller Deployment to 0** (so no watch is running) and wait for the pod to be gone.
4. `kubectl delete` exactly one of the two objects while the operator is down.
5. **Scale the controller back to 1**; wait for the new pod and its post-restart reconcile to drain
   (reuse the drain-signal/branch-worker-queue waits from
   [restart_reconcile_e2e_test.go:184](../../test/e2e/restart_reconcile_e2e_test.go#L184)).
6. Assertions:
   - The deleted object's file is **removed** from Git (the sweep fired on replay).
   - The surviving object's file is **still present and unchanged** (sweep is scoped, not a wipe).
   - The removal lands in a commit authored by the **configured committer** (the actual delete was never
     witnessed — architecture says committer-authored).

**Notes / pitfalls:**
- Scale-to-0 vs. `rollout restart`: scale-to-0 guarantees a true no-watch window so the delete is
  genuinely missed. A plain rollout may keep a watch alive long enough to catch the delete as a live
  event — which would test the wrong path.
- Keep the object set tiny and `Serial` to avoid cross-test interference (consistent with the existing
  restart test).

### Test B — Comments survive a resync / controller restart  *(priority 2)*

**Pins:** §2.1 gap — comment preservation through the resync (M8) apply, not just the live (M7) path.

**Sketch** (extend [inplace_edit_e2e_test.go](../../test/e2e/inplace_edit_e2e_test.go) with a new `It`,
or a small new file):

1. Create a watched ConfigMap; wait for its committed file.
2. Seed a hand-authored comment into the committed file (reuse `seedCommentIntoRepoFile`,
   [inplace_edit_e2e_test.go:114](../../test/e2e/inplace_edit_e2e_test.go#L114)).
3. **Restart the controller** (rollout restart is fine here — we want the startup reconcile/replay).
4. Wait for the post-restart reconcile to drain.
5. Assertions:
   - The hand-authored comment is **still present** (the replay-driven reconcile did not rewrite the
     file from scratch).
   - No spurious commit was produced if the desired state is unchanged (a semantically-equal replay must
     be a no-op — reuse the "no commit since restart" check from
     [restart_reconcile_e2e_test.go:222](../../test/e2e/restart_reconcile_e2e_test.go#L222)).
6. *(Optional second phase)* While still restarted-clean, update the ConfigMap and assert the comment
   survives the in-place edit *after* a restart — proving the resync didn't leave the file in a
   non-editable shape.

### Test C — Encryption re-encrypt boundary: comments are dropped (intended)  *(priority 3)*

**Pins:** §2.1 encryption boundary — make the "comments lost on re-encrypt" behavior an explicit,
asserted contract rather than an accident.

**Sketch** (extend the encryption/SOPS e2e, e.g. alongside
[watchrule_configmap_secret](../../test/e2e/watchrule_configmap_secret_e2e_test.go)):

1. GitProvider with encryption configured; watched Secret committed as `.sops.yaml`.
2. Seed a hand-authored comment into the encrypted document (or into a sibling plaintext doc, depending
   on what the editor exposes).
3. Update the Secret to trigger a re-encrypt.
4. Assertions:
   - The Secret value change is reflected (decrypts to the new value).
   - The comment on the re-encrypted document is **gone** — and a code comment in the test names this as
     intended behavior, citing the wholesale-re-encrypt rule in
     [plan_flush.go](../../internal/git/plan_flush.go).
   - A comment on an **unrelated plaintext sibling** (if present) **survives** — re-encrypt is scoped to
     the sensitive document only.

> This test exists to *pin a boundary*, not to assert a desirable feature. If the team later decides
> comments should survive re-encrypt, this test flips from "asserts dropped" to "asserts preserved" and
> becomes the spec for that work.

### Test D — Refuse an unsupported folder  *(blocked on a product decision — see §4)*

The behavior to test does not exist yet (§2.3). The test shape depends on which direction is chosen:

- **If we implement refusal** (wire the acceptance gate into reconcile + a `Synced=False` /
  `Reason=UnsupportedContent` condition that stops writes): the e2e seeds a target path containing a
  hard-Kustomize `kustomization.yaml` (patches/generators), creates the GitTarget, and asserts the
  GitTarget goes to a refused condition with a file-naming message and that **no commit** is produced
  until the folder is cleaned. Reuse the unsupported corpus fixtures referenced by
  `contextual_namespace_corpus_test.go`.
- **If we document current behavior**: the e2e asserts the operator **leaves the unsupported files alone
  and keeps writing** its own managed files — honest, but explicitly *not* a refusal. The test's name
  and comments must make clear this documents a known limitation, linked back to §2.3.

***

## 4. Open questions (the ones worth asking)

These came out of the investigation and are genuine forks — capturing them here so they aren't lost.

1. **Unsupported-folder behavior: refuse, or keep writing?** — **DECIDED (2026-06-26): refuse.** It is
   wrong to ship the acceptance gate and not enforce it. We will implement refusal **for the cases where
   we already know the content is a problem** (the structure-only refusals: duplicate identity, impure
   managed file, standalone non-KRM / invalid YAML, mixed managed+allowlisted), wire the gate into the
   write path, surface it on GitTarget status, and stop committing until the folder is cleaned. Test D
   then proves it. The implementation design lives in
   [unsupported-folder-refusal-plan.md](unsupported-folder-refusal-plan.md).
   - ~~(b) Keep current behavior and document it.~~ Rejected.
   - ~~(c) Defer.~~ Rejected.
2. **If we implement refusal — what's the signal?** A new `Synced` reason (e.g. `UnsupportedContent`) vs.
   a dedicated condition vs. a `failingTypes`-style count + event. Needs to fit the two-axis status
   design in [status-design-git-target.md](status-design-git-target.md).
3. **If we implement refusal — is it whole-GitTarget or scoped?** Refuse the entire GitTarget subtree, or
   only the offending `(group, resource)` while other types keep syncing? The resync is already
   type-scoped (`ScopeGVR`), so per-type refusal may be the more consistent shape.
4. **Test A — scale-to-0 vs. rollout restart?** Scale-to-0 guarantees a true no-watch window (the delete
   is genuinely missed). Confirm the test harness can scale the controller Deployment and that leader
   election re-acquisition on scale-up is fast enough for the existing drain waits.
5. **Should comments survive re-encrypt (Test C)?** Today they don't, by design. Is that the long-term
   contract, or a limitation to revisit? The answer decides whether Test C asserts "dropped" or
   "preserved."
6. **Coverage vs. e2e wall-clock.** The suite is already large and the team has active e2e-speedup work
   ([e2e-speedup-plan.md](e2e-speedup-plan.md)). Tests A and B each need a controller restart/scale
   cycle (slow). Can they fold into the existing `restart-reconcile` `Serial` ordering to share one
   restart, rather than each paying its own?

***

## 5. Sequencing & scope

**Do now (small, high value, no product decision needed):**

1. Test A — missed-delete mark-and-sweep (priority 1; the biggest architectural gap).
2. Test B — comments survive restart/resync (priority 2; extends the existing in-place work).
3. Test C — encryption re-encrypt boundary (priority 3; pins a sharp edge).

**Gate on decision (§4.1):**

4. Test D — unsupported folder. If (a) is chosen, this becomes a small implementation project
   (acceptance-gate wiring + status condition) with the e2e as its acceptance proof; if (b)/(c), it is a
   documentation test or future-work item.

**Out of scope for this plan** (named so they're not silently assumed covered):

- Conflict-retry / atomic-push-under-remote-divergence as an e2e (fetch/reset/replay is unit-tested; an
  e2e would need to move the remote under the operator — high cost, low marginal confidence).
- Redis resume-cursor recovery and `410 Gone` reconnect as dedicated e2e (best-effort by design).
- `deletecollection` as a dedicated e2e (reconciled by the watch / sweep; covered transitively).

***

## 6. Relationship to existing work

- Complements [restart_reconcile_e2e_test.go](../../test/e2e/restart_reconcile_e2e_test.go): that test
  owns the *don't-over-delete* direction; Test A owns the *reconcile-a-missed-delete* direction. Together
  they fully cover the mark-and-sweep guarantee.
- Extends [inplace_edit_e2e_test.go](../../test/e2e/inplace_edit_e2e_test.go) from the live path to the
  resync/restart and encryption paths.
- Test D, if pursued via §4.1(a), is the first consumer of the acceptance gate
  ([acceptance.go](../../internal/manifestanalyzer/acceptance.go)) inside the running controller, and
  would extend the GitTarget status design in
  [status-design-git-target.md](status-design-git-target.md).
