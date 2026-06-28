# Watch-first branch — merge readiness & finishing plan

> Status: ASSESSMENT — 2026-06-28. Scope: the `investigate` branch (watch-first ingestion big-bang)
> against `main`. This is a review-and-checklist document, not a design. The design is
> [watch-first-ingestion-architecture.md](watch-first-ingestion-architecture.md); the e2e gap analysis
> is [e2e-coverage-gaps-and-improvements-plan.md](e2e-coverage-gaps-and-improvements-plan.md). This doc
> consolidates "can we merge, and what's left."

## 1. What this branch is

A big-bang rewrite from **audit-first** to **watch-first** ingestion. `WATCH` (with `sendInitialEvents`
replay) becomes the only source of object state; the audit webhook is demoted to an **optional**
attribution lookup. **Redis/Valkey remains required** — it holds each GitTarget's watch resume cursors
(state continuity, so a restart/reconnect re-picks up where it left off), and it underpins HA and the
planned move of the branchworker queue into Redis (to keep working while Git is unreachable). Only
**author attribution** is optional. ~15k lines deleted, ~22k added across 280 files. The diff is
*deletion-forward*: whole subsystems were removed (`internal/gate`, most of `internal/queue/redis_*`,
`internal/watch/{audit_tail,splice_snapshot,materialization phase machine,target_type_watermark,type_objects_mirror}`,
`webhook/audit_joiner.go`).

The vision shift is real and, in this reviewer's read, a net improvement:

- **DeleteCollection is solved by construction.** Audit delivered one opaque `deletecollection` event;
  watch delivers N independent `DELETED` events, one per object, each handled by the normal delete path.
  No special code is needed for state correctness. (Attribution of *who* ran the collection delete is a
  separate, optional feature — see §4.)
- **Aggregated APIs, shallow bodies, CRD conversion** — the cases that hurt audit-first — are handled
  because watch carries the full persisted object.
- **The "lose a delete" failure mode is closed two ways:** Mode-A exact-RV resume (the delta carries the
  deletes) and Mode-B `sendInitialEvents` replay + mark-and-sweep (reconciles deletes missed while no
  watch ran). Neither path silently drops a delete.

## 2. Implementation status (verified against code)

| Area | Status | Evidence |
|---|---|---|
| Mode-A exact-RV resume (cursor in Redis, resume delta) | ✅ done | `internal/watch/target_watch.go` resume path + `queue/attribution_index.go` `WatchCursorStore` |
| Mode-B `sendInitialEvents` replay + mark-and-sweep | ✅ done | `target_watch.go` replay accumulation → `EnqueueResync` with `ScopeGVR`; sweep in `git/resync_flush.go` |
| Bookmark handling (`initial-events-end`, RV advance) | ✅ done | `target_watch.go` bookmark/`InitialEventsAnnotationKey` handling |
| Sanitize on the hot path | ✅ done | `sanitize.Sanitize` before diff in `target_watch.go` |
| Optional **attribution** (audit off → committer-authored) | ✅ done | `cmd/main.go` `--audit-attribution-enabled`; nil author-lookup all the way down |
| Redis **required**; resume cursors decoupled from audit | ✅ **done in this pass** — see §5 | `cmd/main.go` builds the index + wires `WatchCursorStore` unconditionally; audit gated by `--audit-attribution-enabled` |
| Conservative resolver + bounded grace window | ✅ done + tested | `internal/watch/author_resolver.go` (`DefaultAttributionGraceWindow`), `author_resolver_test.go` |
| CommitRequest: fail-closed → finalize-as-committer | ✅ done | `internal/controller/commitrequest_controller.go` |
| Refuse unsupported folder + GitTarget status | ✅ done + e2e | `manifestanalyzer/acceptance*`, `test/e2e/unsupported_folder_e2e_test.go` |
| `.gittargetignore` support | ✅ done + unit tests | `manifestanalyzer/gittargetignore.go`, `git/gittargetignore_writer_test.go` |
| DeleteCollection **attribution** expander + deletion-as-intent | ✅ done + unit + e2e | `RecordDeleteCollectionFacts` in `attribution_index.go`, `operationForLiveTargetWatchEvent` in `target_watch.go`, `exact_deletecollection_item` reason code, `test/e2e/deletecollection_intent_e2e_test.go`. See [deletecollection-attribution-expander.md](deletecollection-attribution-expander.md) |
| No-op suppression *on the watch hot path* | ⚠️ deferred to writer | writer diffs to no-op; design-acknowledged CPU cost, not a correctness gap |

**No `TODO`/`FIXME`/`panic("unimplemented")` in the core data path.** The only two TODOs are unrelated
GitProvider-readiness notes in the watchrule/clusterwatchrule controllers.

## 3. Loose ends to close before merge

> Only **remaining** work is listed here. Items finished during this review — the Redis-required /
> cursor-decoupling change, the dead-code removal, and the doc reconciliation — are recorded in §2
> (status) and §5 (work done), not repeated as open tasks.

### Blocking-ish (correctness/trust proof)

1. **Test A — missed-delete mark-and-sweep e2e (the "heart of the system").** Only the *safe half*
   (don't over-delete on restart) is proven (`restart_reconcile_e2e_test.go`). The active half — stop
   the operator, delete a watched object, restart, replay-sweep removes the orphaned file — has **no
   e2e**. The mechanism is unit-proven (`internal/git/resync_flush_test.go`
   `TestResync_DropsManagedResourceAbsentFromCluster`), so this is a proof gap, not a code gap. Sketch is
   in [e2e-coverage-gaps §Test A](e2e-coverage-gaps-and-improvements-plan.md). **Highest-value test to
   add.**
2. **DeleteCollection state-level e2e.** The headline win is currently only *captured* in the mutation
   lab corpus (`test/mutationlab/corpus/configmap/deletecollection/`), not *exercised* against the
   running operator. The gaps doc marks a dedicated e2e "out of scope / covered transitively." Given the
   user-facing claim, a thin e2e (delete a collection of 3 ConfigMaps → 3 files removed in one or more
   committer commits) is worth the cost to *prove* the promise. Pairs naturally with Test A.

### Should-fix (cleanup / honesty)

3. **Aggregated-API user guide is a content regression.** `docs/aggregated-api-guide.md` (102 lines of
   real setup instructions) was deleted and **not** migrated — `architecture.md` mentions aggregated APIs
   only once. README links were repointed (this branch) so there are no broken links, but a self-managed
   user who needs aggregated-API capture now has no setup guide. Decide: restore + rewrite for watch-first,
   or fold into `architecture.md`. The feature itself still works and is e2e-tested
   (`test/e2e/setup/manifests/sample-apiserver/`, `aggregated_apiserver_e2e_test.go`).
4. **`docs/tasks/bounded-queue.md` is obsolete.** It is about bounding the audit Redis stream
   (`--audit-redis-max-len`), a flag and a subsystem that no longer exist. Archive or delete.
5. **Attribution reason-code granularity.** Reason codes implemented today: `exact_user`,
   `exact_serviceaccount`, `weak`, `exact_deletecollection_item` (✅ landed with the expander — §4),
   `conflict`, `expired`, `absent`. The remaining finer codes (`weak-no-rv`, `conflict-multi-user`,
   `absent-no-redis`, `absent-policy-dropped`) are still not distinct. Not blocking; fast-follows that improve
   attribution fidelity and observability. (Note: the SA naming policy / `serviceaccount_collapsed` result was
   removed — a matched service account is always named by its own username.)

### Nice-to-have (test breadth)

6. Test B — comments survive a resync/restart (extends the live-path in-place edit proof).
7. Test C — encryption re-encrypt boundary (pins "comments dropped on re-encrypt" as an explicit contract).

## 4. The DeleteCollection nuance (now closed)

"Watch-first solves DeleteCollection" was always **true at the state level** (each deleted object produces its
own watch event). The **attribution expander** is now **built** ([deletecollection-attribution-expander.md](deletecollection-attribution-expander.md)):
`RecordDeleteCollectionFacts` expands one `deletecollection` response body into N per-UID facts, joined to each
removal event by UID (not RV), surfacing as `exact_deletecollection_item`. It ships alongside the
**deletion-as-intent** render rule — an object marked with a `deletionTimestamp` is removed from the intent tree
at delete-request time and attributed to the requester, even while a finalizer keeps it Terminating; later
finalizer cleanup folds to a no-op. When the API server returns a hollow body (aggregated / metadata-only), the
collection delete still commits as **committer** (the documented v1 limit), consistent with "a wrong author is
worse than no author." Unit + e2e coverage landed (`test/e2e/deletecollection_intent_e2e_test.go`).

## 5. Work completed in this pass

Applied on-branch (no commit). Validated end to end: `go build`, `go vet`, `task lint` (0 issues),
`task test` (unit, 73.8%), and `helm template` (both modes).

**Framing rule (owner decision):** Valkey/Redis is **required** — it holds each GitTarget's watch resume
state and underpins HA and the future durable branchworker queue. **Only author attribution is
optional.** Everything below states it that way; never call Redis optional.

**Code**

- **Redis required; resume cursors decoupled from attribution.** `cmd/main.go` now builds the Redis
  index and wires `watchMgr.WatchCursorStore` **unconditionally**; `validateAuditConfig` **requires** a
  non-empty `--audit-redis-addr`; a new `--audit-attribution-enabled` flag (default `true`, chart
  `attribution.enabled`) gates **only** the audit webhook ingress + resolver + CommitRequest lookup. So
  "committer-only" now means *attribution off, Redis still on*. `cmd/main_audit_server_test.go` updated
  (empty `--audit-redis-addr` is now an error; committer-only is the attribution toggle).
- **Dead-code removal.** Deleted the last materialization-phase-machine residue in `internal/watch`
  (`NudgeTypeResyncForLateEvent` → `claimedGVRForGroupResource` → `materializerInstance` + the backing
  `lateNudge*`/`materializer*` fields and `lateNudgeMinInterval` const), zero-external-caller verified;
  refreshed the stale `declaredGVRs` comment. (`dropTargetGitPathAcceptanceLocked()` was a false-positive
  in automated analysis — it is called from `target_watch.go`; kept.)

**Docs & chart**

- `README.md`: rewrote "How it works" to watch-first (Redis tracking resume state as an explicit step);
  added an **Operating modes** table framed on the *attribution* axis (both modes require Valkey/Redis);
  state-mirror/opportunistic-history honesty note; marked **only audit delivery** as attributed-mode-only
  in the quick start (Valkey is "required — both modes"); fixed two broken links to the deleted
  aggregated-API guide; bumped tested K8s 1.35 → 1.36.
- `charts/gitops-reverser/README.md`: Valkey/Redis listed as **required**; **only** audit delivery listed
  as optional; feature bullets reworded; K8s 1.35 → 1.36.
- `charts/gitops-reverser/values.yaml` (new `attribution.enabled`, reworded Redis comments) and
  `deployment.yaml` (passes `--audit-attribution-enabled`); `NOTES.txt` left at its Redis-present framing.
- `docs/configuration.md` and `docs/architecture.md` now state Redis as required with attribution as the
  only optional capability (including the startup flowchart and the incidental "no Redis" phrasings); the
  design-record `watch-first-ingestion-architecture.md` carries a correction banner.

## 6. Merge recommendation

**Mergeable in principle — the rewrite is complete, builds clean, and the core trust guarantees are
implemented.** The Redis-required / cursor-decoupling correction is done (§5). Before merge, the two
items worth not skipping are **Test A (missed-delete sweep e2e, §3.1)** and a **DeleteCollection state
e2e (§3.2)**, because they are exactly the scenarios that justify the rewrite and are currently proven
only at the unit / corpus level. The obsolete-doc cleanup and aggregated-API guide (§3.3–3.4) are
low-risk and make the deletion-forward story honest. The DeleteCollection attribution expander (§4) and
reason-code granularity (§3.5) are legitimate fast-follows, not blockers.
