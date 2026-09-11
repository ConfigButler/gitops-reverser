# Make GitTarget red when commits cannot happen

> **design**: partly built. Index: [`../INDEX.md`](../INDEX.md)

## Purpose

This is the smaller alternative to Git write preflight. Instead of trying to predict an API write in
the admission path, make the existing `GitTarget` status answer the question operators actually have
after a change does not appear in Git:

> Why did this target not commit, who can fix it, and what should I look at first?

The watch stream remains the source of mirrored state. The writer remains the place that proves a
change can be represented in Git. The status surface should turn red after the writer refuses or
cannot run, without doing Git work in a webhook and without adding a second state-ingestion path.

"Quickly" has a bounded meaning here, not an admission-path promise. A refusal report enqueues a
`GitTarget` reconcile through a best-effort channel. If that notification is dropped, status can stay
stale until the periodic requeue. A converged target can therefore take up to the steady requeue
interval to publish the new red state. That is still the right model: Kubernetes accepted the
object, the watch stream observed it, and the writer then reported whether Git could represent it.

## Recommendation

Keep the current kstatus shape:

- `Ready=False`, `Reconciling=False`, `Stalled=True` for hard blocks that will not clear by waiting.
- `Ready=False`, `Reconciling=True`, `Stalled=False` for work that is still making progress.
- `Ready=True`, `Reconciling=False`, `Stalled=False` when the latest observed generation is healthy.

Do not add a new phase field. Do not make the webhook participate. Improve the domain conditions that
already explain the summary:

- `Validated`
- `EncryptionConfigured`
- `GitProviderReady`
- `ClusterProviderReady`
- `SourceClusterReachable`
- `StreamsRunning`
- `GitPathAccepted`
- `RenderMatchesLive`
- `LayoutResolved`

The least invasive change is to make these conditions and messages complete enough that `kubectl get
gittarget -o wide` points at the right axis, and `kubectl describe gittarget` tells the operator what
to fix.

Correctness comes before better prose. A more specific message is worse than today's message if it
can point at the first file that failed with that reason and never update when a different file fails
later. So the first implementation slice must fix the status transition semantics before enriching
`GitPathAccepted` messages.

## Current constraints

The existing surface is close, but four constraints matter for this plan:

1. `GitPathAccepted` is target-level state held in memory by the watch manager. After a manager
   restart the target reads accepted until a write or resync refuses again.
2. Acceptance-change notifications are best-effort. A full buffer drops the reconcile request and
   the published status can stay stale until the periodic requeue.
3. `reportGitPathAcceptance` compares message changes as well as accepted flag and reason, so a new
   first offender under the same reason republishes status.
4. Refusals remember their scope when the writer knows it. A successful per-scope resync clears only
   a refusal raised by the same scope; an unrelated scope cannot make the target green.

## Most likely situations

| Likelihood | Situation | Existing signal | Desired operator answer |
|---|---|---|---|
| Very common | Referenced `GitProvider` is missing, unready, or the branch is not allowed. | `Validated=False` or `GitProviderReady=False`; `Ready=False`, `Stalled=True` or `Reconciling=True` depending on the dependency. | Name the provider, namespace, branch, and whether the fix is "create/fix the GitProvider" or "allow this branch". |
| Very common | No `ClusterProvider/default`, remote cluster credential broken, or this `GitTarget` namespace is not authorized to use the provider. | `Validated=False`, `ClusterProviderReady=False`, `SourceClusterReachable` condition. | Name the referenced ClusterProvider, whether it is missing, unready, unreachable, or refuses this target namespace. |
| Very common | Encryption is enabled but the age Secret is missing, malformed, or generation is disabled. | `EncryptionConfigured=False`; `Ready=False`, `Stalled=True`. | Name the Secret and exact missing/malformed field; say whether to create it or enable generation. |
| Common | Git folder contains unsupported or unsafe content. | `GitPathAccepted=False`, reason `UnsupportedContent`; writer commits nothing. | Name the first offending path and issue kind: duplicate identity, invalid YAML, non-KRM YAML, impure multi-doc file, mixed kustomization file, foreign file, symlink, submodule, or unsupported kustomize construct. |
| Common | `.gittargetignore` hides something the operator must write. | `GitPathAccepted=False`, reason `IgnoreShadowsManagedPath`. | Name the matching pattern and managed path; say to narrow the ignore rule or move the resource. |
| Common | A `GitTarget` path covers more than one kustomize render root. | `LayoutResolved=False`, reason `Ambiguous`; then write-time `GitPathAccepted=False`, reason `AmbiguousLayout` once a write needs placement. | Name the render roots and say to point the GitTarget at one leaf/root. `LayoutResolved` explains the shape but does not write the kstatus trio. |
| Common | A namespace-free target receives objects from more than one source namespace. | `GitPathAccepted=False`, reason `MultipleSourceNamespaces`. | Name the namespaces when available; say to split targets/rules or serialize namespaces. |
| Common | A placement template writes a new file outside the render root of a kustomize folder. | `GitPathAccepted=False`, reason `UnrenderedPlacement`; `status.placement` gives the root. | Name the resolved path and render root; say the file would not be rendered by the folder. |
| Less common but important | A planned edit would write outside `spec.path`. | `GitPathAccepted=False`, reason `WriteBoundaryRefused`. | Name the path and target scope; say this target reads shared context but only writes inside its path. |
| Less common but important | A file is reached by multiple render roots, so write-through would corrupt another environment. | `GitPathAccepted=False`, reason `WriteBoundaryRefused`. | Name the shared path and say write fan-in must be 1. |
| Less common but important | Kustomize render verification refuses the planned batch. | Pure `IssueRenderDoesNotMatchLive` goes to `RenderMatchesLive=False`; `IssueRenderRefused` goes to `GitPathAccepted=False`, reason `WriteBoundaryRefused`. | Separate "live no longer matches rendered tokens" from "the proposed write would not render correctly"; name the field/token/path when known. |
| Less common | The projection cannot place a field edit, usually a list without stable item identity. | `GitPathAccepted=False`, reason `WriteBoundaryRefused`. | Say this edit has no safe source-form representation and no user action can make that specific edit reversible today. |
| Operational | The branch worker cannot be wired for this target. | `Ready=False`, reason `WorkerUnavailable`; current code folds it into the kstatus trio. | Name the provider/branch wiring failure. |
| Operational, not target-red yet | The shared branch worker queue is full, or a background resync fails after enqueue. | Logs and metrics; queue and worker state are shared across targets. | Keep this in metrics/logs until durable queue or owner identity work can tie it to a target without lying. |
| Common trap, red elsewhere | A `WatchRule` or `ClusterWatchRule` is invalid, unauthorized, or not yet streaming. | Rule-level `Ready`, `GitTargetReady`, `SourceNamespaceAuthorized`, and `StreamsRunning`; the `GitTarget` may stay green. | Tell users to inspect rules whenever the target is green but no object is committing. |
| Normal, not red | Target is suspended. | `Ready=True`, reason `Suspended`. | Say no writes are attempted by request; keep placement scanning visible. |
| Normal, not red | No WatchRule selects the target. | `StreamsRunning=True`/`NoResolvedTypes`, `Ready=True`. | Say there is nothing to mirror, not that the target is broken. |
| Progress, not red | Initial replay or watch-plan settle is still running. | `Ready=False`, `Reconciling=True`, `Stalled=False`. | Say which axis is still progressing; avoid classifying it as a fault. |

## Message contract

Every red `GitTarget` should answer five questions in bounded prose. The condition message should be
one line, preferably under 240 characters and hard-capped at 512 characters before it is written to
status. Longer detail belongs in logs.

1. **What stopped?** For example: `GitPathAccepted=False`, `Validated=False`, or
   `EncryptionConfigured=False`.
2. **Where?** Name the object reference, branch/path, file path, document index, field path, or
   source namespace when the code has it.
3. **Why was no commit made?** Use one reason string from the existing vocabulary.
4. **Who can act?** Reuse `AcceptanceIssue.Solvable` and `AcceptanceIssue.Actor`:
   `repository-author`, `platform-operator`, or no actor when this release cannot support the case.
5. **What is the next check?** Point at the specific config or file to inspect, not a generic
   troubleshooting page.

Do not put per-event counts or last-attempt timestamps in status. Those belong in metrics. Status
should move on health transitions, spec/config changes, bounded rollups, or a changed refusal
message for an already-red reason.

## Proposed implementation

### Step 1: Pin the status and reason contract

Add a table-driven unit test that constructs each existing hard-block condition and asserts the
resulting `Ready`, `Reconciling`, `Stalled`, domain condition reason, and message stem. This should
live near `internal/controller/gittarget_kstatus_test.go` and `internal/controller/gittarget_status_test.go`.

Also pin the cross-package reason agreement. The reason strings are intentionally duplicated across
controller, watch, and manifest analyzer boundaries because import cycles prevent one shared
constant package today. A test should prove that every `manifestanalyzer.GitPathRefusalReason`
outcome is accepted by the `GitTarget` stalled-reason set and is represented in the controller
vocabulary.

This first step should not change behavior. It makes the current user-facing surface visible.

### Step 2: Fix acceptance state transitions

Built: `reportGitPathAcceptance` compares `Message` as well as accepted flag and reason. That makes a
different offender under the same reason publish a fresh condition instead of leaving the first
message in place forever.

Built: the implementation tracks refusal scope and clears only when the same scope succeeds. A
target-wide refusal still needs a target-wide proof, while a scoped refusal can recover through the
matching scoped resync.

### Step 3: Improve refusal message rendering

Built: split `AcceptanceRefusedError.Error()` from `BlockMessage()` for real. Tests assert `Error()` today,
so enriching status prose through `Error()` would churn unrelated caller expectations.

Only one formatter is needed: both live and scoped-resync paths funnel through
`refused.BlockMessage()`.

The formatter should keep the existing reason mapping, but include:

- issue kind
- path and document index when present
- solvable/actor when present
- a short action hint derived from the issue kind

Keep it bounded: first issue plus "`and N more`" is enough for status. Full detail can remain in
logs. If the cap truncates the hint, preserve kind, path, and actor before prose.

### Step 4: Add missing red paths only where they are already proven

Do not broaden support. Only surface failures the current code already knows:

- live write-plan refusals
- scoped resync refusals
- render fidelity divergence
- worker wiring failure

Do not add branch-worker queue-full or background-resync failure to `GitTarget` status in this
increment. Those paths are shared across targets or metrics-only today; forcing them onto one target
would be a diagnosis lie.

### Step 5: Operator-facing documentation

Add a short user doc section, probably in `docs/configuration.md`, called "Why a GitTarget is not
committing". It should say:

```bash
kubectl get gittarget -A
kubectl describe gittarget -n <namespace> <name>
kubectl get watchrule,clusterwatchrule
```

Then map `Ready` reason and domain conditions to the likely fix. Also state the two places status can
be red:

- A red `GitTarget` means the target itself cannot validate, run, or publish.
- A green `GitTarget` with a red `WatchRule` or `ClusterWatchRule` means the target may be fine, but
  the selected source objects are not being watched or authorized.

This is where most users will start, so it should be practical rather than architectural.

### Step 6: Extend existing e2e coverage

Do not add a new expensive e2e path just to retest refusal. Extend an existing refusal spec, such as
the unsupported-folder or foreign-content e2e, to assert the new message content. The existing tests
already prove `Ready=False`, `Stalled=True`, the domain reason, and no new Git commit.

Also assert recovery there: start from a healthy target, push an unsupported Git-side change, request
a reconcile, observe `GitPathAccepted=False`, then undo the Git-side change, request another
reconcile, and observe the target and dependent rules recover.

## Non-goals

- No admission webhook preflight.
- No locks in the webhook path.
- No Git checkout, fetch, commit, or push from admission.
- No new state source beside the watch stream.
- No per-document list in `GitTarget.status`.
- No promise that an API write can be reversed before it reaches etcd.

## Compatibility

This plan is additive if it only changes condition messages, documents reason precedence, and adds
tests. A new status field would be additive at the CRD level, but it is not recommended for the first
increment because the existing condition set already has enough structure.

Reason strings should remain stable where they already exist. If a new reason is unavoidable, add it
as a domain reason and cover it in kstatus tests before using it in controller output.

Reason is also a metric label on `gitopsreverser_resource_condition`, so a new reason string is a new
time series. Prefer improving messages under an existing reason over minting another reason.

## Open questions

1. Should `AcceptanceIssue.Actor` remain status prose only, or should it become a tiny bounded status
   field later?
2. Should acceptance refusal state survive manager restart, or is "red again on the next refused
   write/resync" sufficient until the durable queue work exists?
