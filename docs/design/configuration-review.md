# Configuration guide implementation checklist

Status: pending documentation and message-contract corrections.

Update the [configuration guide](../configuration.md) against the requirements below. The
[commit-message design](commit-message-refactor.md) owns the literal-message fix and unified
`liveTemplate` API. Keep this guide as the operator reference and link to the window and request
specifications for implementation details.

## Request messages and save outcomes

- Fix the byte truncation in
  [request finalization](../../internal/controller/commitrequest_finalize.go) and the whitespace
  fallback in the [executor](../../internal/git/commit_executor.go) before claiming accepted
  messages are preserved verbatim. Follow the message design's validation and acceptance cases.
- Describe `closeDelaySeconds` as a deadline starting at the worker's first receipt, including time
  waiting for a matching window. Repeated registration preserves that deadline. Align the
  [API description](../../api/v1alpha3/commitrequest_types.go) with the
  [worker](../../internal/git/commit_request_attach_loop.go).
- State that normal flush triggers can close an attached window early. A request claims at most
  one open window and cannot rename a finalized commit, including one waiting for push.
- Explain that applying resources and a request in one bundle gives no ordering guarantee between
  their controllers and watch streams. Recommend a non-zero commit window for custom save messages;
  the request delay does not reserve a transaction.
- Replace the claim that `kubectl wait --for=condition=Ready` waits for every settled outcome.
  Terminal failures leave `Ready=False`; automation must also stop on `Stalled=True`. Distinguish
  successful no-commit outcomes from `Pushed=True` with `status.sha`.

## Window timing

Replace the promise that every burst becomes one commit per author with the actual constraints:

- One branch worker holds one live window, bound to an author and target. Interleaved authors or
  targets split a burst.
- `spec.commit.window` is an inactivity timer. Atomic writes, buffer limits, shutdown, and request
  finalization can close a window early.
- Push cooldown is independent of window closure; `0s` does not promise an immediate remote push.

Link the complete trigger rules from the [window contract](../spec/commit-window-refactor.md).

## Template reference and examples

- Until `liveTemplate` ships, include both `eventTemplate` and `groupTemplate` in the introductory
  commit configuration example. Switch the examples together with the API change.
- Guard optional type and revision fields in every reconcile example so whole-target snapshots
  produce meaningful subjects.
- Add `Namespace` to the reconcile field reference. Match the fields in
  [runtime message types](../../internal/git/types.go).
- Document empty `Author` values alongside grouped fields, and invalid-template status alongside
  the examples. Retain the distinction between raw usernames and Git author headers.
- Keep one template field reference and add a message-precedence table beside it. Cross-link the
  save request example.

Preserve the guide's retained-entry counting semantics: coalescing happens before the writer
checks for differences in Git, so `Count` can exceed the number of changed resources.

## Guide structure and scaling

Add compact navigation for configuring automatic messages, submitting a save message, and
interpreting the result. Keep operational consequences in the guide and link longer design
arguments to their owning specifications.

Replace the claim that a cluster-wide watch's cost does not grow with the cluster. Connection
count is bounded per matched type; object count, traffic, and memory still grow.

## Acceptance

Verify every copyable template against its production renderer, including missing optional values.
Check that the request example explains both failure and no-commit outcomes. Update API comments,
generated schemas, and specifications alongside any contract correction. Run the implementation
validation gates for code or schema changes; run `task lint-docs` for documentation-only changes.
