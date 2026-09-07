# Commit messages and CommitRequest: one live template

Status: proposed implementation.

Use one `GitTarget.spec.commit.message.liveTemplate` for all live-change commits. Keep
`reconcileTemplate` for snapshots and `CommitRequest.spec.message` as a literal override.
`commit.window` controls grouping independently of message formatting, including at `0s`.

Preserve the [window lifecycle](../spec/commit-window-refactor.md) and
[request matching contract](../spec/commitrequest-design.md). Attribution policy and transaction
boundaries are outside this change. See the [configuration cleanup](configuration-review.md)
for the remaining operator-guide work.

## Proposed public configuration

```yaml
spec:
  commit:
    window: "5s"
    message:
      liveTemplate: "chore: sync {{.Count}} resource{{if ne .Count 1}}s{{end}}"
      reconcileTemplate: "chore: reconcile {{.Count}} {{if .Resource}}{{.Resource}}{{else}}resources{{end}}"
```

There are three message sources, in this order:

| Input | Message |
|---|---|
| Non-empty literal override on the pending write | Exact supplied text |
| Live window, including one resource and `0s` | `liveTemplate` |
| Atomic snapshot or resync | `reconcileTemplate` |

`CommitRequest.spec.message` supplies the first row when the request attaches. Omitting it uses
the target's live template. Braces such as `{{.Author}}` in a request message remain literal text.
The message never changes the Git author or committer.

## One template context for live writes

Reuse the grouped context, rename it `LiveCommitMessageData`, and extend its resource entries.
Keep the grouped fields so existing group templates can be migrated directly.

| Field | Meaning |
|---|---|
| `Author` | Existing raw window username; empty when no actor is named |
| `GitTarget` | Target name |
| `Count` | Number of retained resource entries after window coalescing |
| `Operations` | Counts of retained `CREATE`, `UPDATE`, and `DELETE` operations |
| `Resources` | Retained entries in first-seen order |

Each resource entry exposes `Operation`, `Group`, `Version`, `Resource`, `Namespace`, `Name`, and
`APIVersion`. Keep its existing string representation for templates that print an entry directly.
Extend the existing `ResourceRef` if its call sites permit this without unrelated changes.

`Count` describes the coalesced input to the writer. It does not count incoming audit events,
changed files, or a post-apply Git diff. A retained entry may already match Git, and several
resources may share a file. Computing an exact diff summary would require additional writer
results and different replay semantics; it is outside this refactor.

Use `chore: sync N resource(s)` as the default live subject, with singular agreement and one
retained resource per body line. `sync` describes the input count without promising a measured Git
diff. Use Conventional Commits subjects in generated defaults and examples. Keep literal request
messages free-form; the submitter chooses any semantic prefix.

```yaml
spec:
  commit:
    message:
      liveTemplate: |-
        chore: sync {{.Count}} resource{{if ne .Count 1}}s{{end}}

        {{range .Resources -}}
        - [{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{if .Namespace}}{{.Namespace}}/{{end}}{{.Name}}
        {{end -}}
```

For five retained entries, this renders as:

```text
chore: sync 5 resources

- [UPDATE] apps/v1/deployments/production/api
- [UPDATE] apps/v1/deployments/production/worker
- [CREATE] v1/configmaps/production/settings
- [UPDATE] v1/services/production/api
- [DELETE] v1/configmaps/production/legacy
```

Keep the unresolved sentinel in the Git author header, as today. An empty `Author` remains valid;
the default should read sensibly without it. Preserve the reconcile context, including its optional
type, namespace, and revision fields.

## CommitRequest contract

Preserve the existing target, username, and named-actor matching checks. Admission identifies the
request submitter; watch attribution identifies the resource author. A literal message conveys
intent without granting authority over another actor's window.

Document these limits next to the example:

- The worker registers a deadline at first receipt plus `closeDelaySeconds`. Time spent waiting
  for a matching window consumes that delay. Repeated reconciliation keeps the original deadline.
- A normal flush trigger can close an attached window early. Its message travels with that window.
- A request attaches to at most one open window. It cannot rename a commit already finalized,
  including a local commit still waiting for push.
- `0s` leaves little opportunity to attach. Use a non-zero window when custom save messages matter;
  a delay on the request does not reserve a transaction or extend every normal flush timer.
- `Ready=True` includes successful no-commit outcomes. Require `Pushed=True` and `status.sha` when
  the caller needs evidence that this request produced a pushed commit. Stop waiting on `Stalled`.

Keep competing-request selection behavior for this refactor. The current worker selects the
earliest finalize deadline, despite a comment calling it the oldest request. Correct the comment;
changing fairness or equal-deadline ordering is a separate scheduling decision.

### Literal-message validation

Accept 1–1024 Unicode characters when `spec.message` is present. Allow newline; reject all other
ASCII control characters, including tab, carriage return, and DEL. Preserve every byte of accepted
text, including surrounding spaces. Omission means no override; the Go zero value represents
omission internally. An explicitly empty API field remains invalid. Reject non-empty messages
whose `strings.TrimSpace` result is empty.

Remove the controller's 1024-byte truncation. A valid message of 1024 `ü` characters is 2048 bytes.
Defensive validation must reject invalid input with a clear error; it must never alter accepted input.
Use schema/CEL validation for the public contract and
matching controller validation for stored or internal inputs. No new admission webhook is needed.

## Request example

Accept free-form messages subject to the literal validation above. Do not prepend a prefix or
replace rejected text with an automatic message. A rejected request leaves normal automatic
mirroring available. Conventional Commits enforcement and client tooling are outside this change.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: CommitRequest
metadata:
  name: correct-service-port
  namespace: production
spec:
  gitTargetRef:
    name: application
  message: |-
    fix(api): correct the service port

    Route traffic to the port exposed by the API container.
  closeDelaySeconds: 2
```

This message names the submitter's intent. Successful attachment and push still depend on the
window contract above.

## Implementation simplification

The existing flow already carries a request message from registration to the open window, then to
`PendingWrite.CommitMessage`. Retain that flow and its ownership boundaries.

| Location | Proposed change |
|---|---|
| [API message types](../../api/v1alpha3/gitprovider_types.go) | Introduce `liveTemplate`; retain legacy fields only for migration rejection |
| [Runtime types](../../internal/git/types.go) | Replace event/group configurations and contexts with the live equivalents |
| [Window context builder](../../internal/git/open_window.go) | Build the same context for every live window; include operation and API version per entry |
| [Message rendering](../../internal/git/commit.go) | One live renderer, reconcile renderers, and one shared template executor |
| [Commit executor](../../internal/git/commit_executor.go) | Resolve literal/live/reconcile once; build author and committer options once |
| [Pending writes](../../internal/git/pending_writes.go) | Remove cardinality-based message selection; retain write kinds for execution |
| [Window finalization](../../internal/git/branch_worker.go) | Remove the unused explicit-message parameter and its extra precedence layer |
| [Request controller](../../internal/controller/commitrequest_finalize.go) | Replace truncation with contract-aligned validation |

`finalizeOpenWindowWithMessage` currently has only one caller, which always passes an empty
string. Fold it into `finalizeOpenWindowWithReason` and copy the attached message directly.

`commitMetadata` already handles literal precedence before calling the atomic reconcile renderer;
remove that renderer's duplicate override argument after updating its callers. Preserve the
separate resync rendering path, which receives measured reconciliation count and scope metadata.

Remove `CommitMessageKind` if its remaining purpose is only the log field. Log the selected source
as `literal`, `live`, or `reconcile` at the selection boundary. Keep execution kinds such as atomic
and resync: they encode different write behavior, not merely different wording.

Retain resolved templates and the literal override on pending writes across push retries and
conflict replay. Do not re-read a changed target's template during replay. A no-op still creates
no commit, even with an explicit message. Existing in-memory pending state does not provide
restart durability; this proposal does not add it.

## Validation and migration

Template validation should execute the production renderer against one-resource and multi-resource
examples, mixed operations, an unnamed author, and both scoped and unscoped reconciles. The current
group validator uses only one resource, so it cannot catch an invalid field used only in a
multi-resource conditional branch. Sample execution improves coverage without claiming to prove
every possible Go template valid.

Ship the API change in an explicitly breaking minor release. Retain `eventTemplate` and
`groupTemplate` as optional schema fields for a migration release, with no new defaults. Reject
their use with instructions naming `liveTemplate`, and report legacy stored values through the
target's `Validated=False` condition. Retained fields have no runtime fallback behavior.

Inventory stored legacy values and generator output before upgrade. Test removal with both apply
and patch against real CRDs, including clients using `fieldValidation: Ignore`. Verify status
updates remain possible on legacy objects and that replacing legacy fields restores readiness.
Do not serve an alternate schema that silently prunes the replacement field.

For migration:

- Copy a group-only template to `liveTemplate`; its existing fields retain their meaning.
- Move event-only resource fields into `range .Resources` or a guarded singleton entry. Replace
  `Username` with the outer `Author`.
- Combine two custom templates with a `Count` conditional if their different wording matters.
- Leave `reconcileTemplate`, `CommitRequest.spec.message`, and window duration unchanged.

Publish the singleton default-message change and the whitespace-only rejection. Update the guide,
window and request specs, API descriptions, samples, generated CRDs, chart CRDs, README, and upgrade
guide together. Remove legacy schema fields only after the migration release and consumer review.

## Implementation and acceptance

1. **Correct the literal contract separately.** Preserve valid Unicode, reject whitespace-only
   messages, and fix request timing documentation. Test multiline literals and template-like text.
2. **Unify templates in one breaking change.** Add `liveTemplate`, extend the grouped context,
   remove event/group rendering branches and unused finalization arguments, and include migration
   guards and documentation in the same change. Avoid a temporary dual runtime selection policy.
3. **Verify behavior and release.** Extend existing suites for singleton/multiple/repeated edits,
   `0s`/non-zero windows, absent/literal messages, invalid conditional templates, author mismatch,
   early flush, no-op, delayed push, and conflict replay after a target template changes. Exercise
   schema migration with envtest and custom save messages through the existing e2e suite.

For each implementation change, follow the repository's format, generation, vet, lint, unit-test,
and sequential e2e gates. Check Docker before e2e and commit any coverage baseline increase.
