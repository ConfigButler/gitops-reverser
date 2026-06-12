# Signing snapshot E2E failure: per-type tail replay creates event commits

> Status: investigation note, captured 2026-06-12.
> Context: GitHub Actions run
> [27377310456](https://github.com/ConfigButler/gitops-reverser/actions/runs/27377310456),
> attempt 1, commit `113b4bc0224cfc0e5f900e38f877b8676828dfe9`
> (`chore: adding logging and investigation`).
> Related investigation:
> [github-e2e-per-type-tail-failure-investigation.md](github-e2e-per-type-tail-failure-investigation.md).
> Scope: why the signing snapshot test saw per-event commits in a path that was
> expected to be snapshot-only.

## 1. Executive summary

The failed run did **not** reproduce the earlier CommitRequest `NoOpenWindow`
failure. The CommitRequest specs passed. The only visible full E2E failure was:

```text
Commit Signing
[It] should produce a snapshot commit with the custom snapshot message template
```

The signing snapshot test expected every commit under `e2e/signing-snapshot` to
use the snapshot message template:

```text
e2e-snapshot: synced N resources to signing-snapshot-dest
```

Instead, the remote history for that path also contained normal per-event
subjects:

```text
e2e-snapshot: synced 1 resources to signing-snapshot-dest
e2e-snapshot: synced 6 resources to signing-snapshot-dest
[CREATE] v1/secrets/signing-key-batch
e2e-snapshot: synced 4 resources to signing-snapshot-dest
[CREATE] v1/configmaps/batch-cm-2
```

The important conclusion:

> The snapshot path worked, but the per-type audit tail also delivered replayed
> audit events to the same GitTarget after it registered. Because the signing
> GitProvider uses `commitWindow: 0s`, those replayed events became immediate
> per-event commits.

This is a target-local freshness bug. A type-global tail can replay events that
pre-date a GitTarget's active watch relationship into that GitTarget as if they
were live events.

## 2. What the test expected

The test deliberately builds a snapshot-only situation:

1. Create `batch-cm-0`, `batch-cm-1`, and `batch-cm-2` before the relevant
   WatchRule is active.
2. Create a signing GitProvider with:
   - `commitWindow: "0s"`;
   - a custom per-event template: `[{{.Operation}}] ...`;
   - a custom snapshot template: `e2e-snapshot: synced {{.Count}} resources ...`;
   - `generateWhenMissing: true`, which creates the signing key Secret.
3. Create a GitTarget and WatchRule.
4. Recreate the GitTarget to force a fresh snapshot batch.
5. Assert that the history for `e2e/signing-snapshot` contains no subject with
   `[` because `[` identifies the per-event template.

The intended distinction is:

| Input shape | Expected path into Git | Expected subject |
|---|---|---|
| Resources that already exist when the target/rule becomes active | Snapshot/resync | `e2e-snapshot: synced N resources to signing-snapshot-dest` |
| New audit event after the target/rule is active | Live event tail | `[CREATE] ...` |

In other words, the pre-created ConfigMaps and generated signing Secret should
be represented by snapshot commits only. They should not later be replayed as
normal live events for the same GitTarget.

## 3. What actually happened

The failed history shows both paths acted on the same logical objects:

| Evidence | Meaning |
|---|---|
| `e2e-snapshot: synced 1 resources...` | A snapshot/resync commit landed. |
| `e2e-snapshot: synced 6 resources...` | Another snapshot/resync commit landed. |
| `[CREATE] v1/secrets/signing-key-batch` | The generated signing-key Secret was also committed through the live event path. |
| `e2e-snapshot: synced 4 resources...` | A later snapshot/resync commit corrected or re-folded state. |
| `[CREATE] v1/configmaps/batch-cm-2` | One of the pre-created ConfigMaps was also committed through the live event path. |

The failing assertion is therefore accurate. The path was not snapshot-only.
Per-event work really reached the remote history.

## 4. Why the event commits were made

The code path that explains the failure:

1. `DeclareForGitTarget` drives an initial backfill splice for newly claimed,
   already-synced types and starts the type audit tail:
   `internal/watch/materialization.go`.
2. `applyAuditChangesForType` fans every audit-tail batch to every current
   GitTarget that watches that type and has a registered stream:
   `internal/watch/audit_tail.go`.
3. That fan-out checks current type membership and namespace scope, but it does
   not check whether the audit event is newer than this GitTarget's own
   registration/declaration point.
4. The signing GitProvider template uses `commitWindow: "0s"`.
5. The branch worker treats every delivered live event as a per-event commit and
   finalizes immediately when `commitWindow == 0`.

So the unexpected commits were not made by the snapshot code. They were made by
the normal event path after the type-global tail replayed old-enough audit
events to a newly active target.

## 5. Expected vs actual code contract

The code appears to expect this contract:

> If a GitTarget stream is not registered yet, audit-tail events are skipped for
> that target, and the first checkpoint splice covers it.

That is visible in `applyAuditChangesForType`:

```go
if m.EventRouter.GetGitTargetEventStream(table.GitDest) == nil {
    continue // the GitTarget's stream is not registered yet; its first checkpoint splice covers it
}
```

The missing part is a target-local lower bound after the stream is registered.
Once registration exists, the code treats all tail-delivered entries as live for
that target, even when those entries belong to the gap that the first checkpoint
splice was supposed to cover.

The actual contract implemented today is closer to:

> A running type tail is global for the GVR. Any event it reads after a
> GitTarget stream becomes registered may be routed to that GitTarget if the
> target currently watches the type and namespace.

That is too broad for newly registered or recreated targets.

## 6. Why the logging run did not show the exact reason line

Commit `113b4bc` added the branch-worker reason logs, but the most useful lines
were still at `V(1)`. The E2E deployment runs with `logging.level: info`, so the
run showed the high-level symptoms (`git commit created`) but not the reasoned
line that would have said `reason=commit-window-zero`.

The follow-up change should temporarily promote these diagnostics to info:

- finalize signal enqueued/processed;
- open window created;
- open window finalized, including `reason`;
- resync request received/applied, including whether it closed a window.

This is noisy, but acceptable while this per-type-tail cutover is still under
investigation.

## 7. Fix advice

The right fix is to make audit-tail delivery target-aware, not merely
type-aware.

A practical shape:

1. When a GitTarget declares or registers a stream for a type, record a
   target-local activation watermark for `(GitTarget, GVR)`.
2. Let the initial checkpoint splice cover everything at or below that
   activation point.
3. Route audit-tail entries to that GitTarget only when the entry is newer than
   the target-local activation watermark.
4. Keep the type-global tail cursor as a read cursor, but do not treat it as
   proof that every target has had every event applied.
5. Use the same target-local applied watermark later for CommitRequest barriers.

This aligns the two recovery paths:

- snapshot/resync owns the historical catch-up when a target starts watching;
- audit-tail live delivery owns only changes that happen after that target is
  actively watching.

## 8. Regression tests to add

Add a focused unit or integration test for the newly registered target case:

1. A type already has a running tail.
2. An audit event for `configmaps/batch-cm-2` exists in the type stream.
3. A new GitTarget starts watching ConfigMaps after that event.
4. The initial splice commits the object with a snapshot message.
5. The tail must not route the older event to that GitTarget as a per-event
   write.

Also keep an E2E-level assertion like the current signing snapshot test. It is a
good user-facing detector because it catches the exact visible damage: a path
that should have snapshot-only history accumulates per-event subjects.
