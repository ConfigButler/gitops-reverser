# Proposal: clearer CRD descriptions

The branch makes the API reference more consistent, but several descriptions still ask the reader
to unpack too many rules at once. Keep the useful detail. Give each sentence one job, and use
bullets where readers need to compare values, check requirements, or follow a precedence order.

This proposal changes wording and line wrapping, and adds one lint check for line width. It proposes
no schema or controller behavior changes.

## Scope

Reviewed `chore/crd-text-sweep` at `0b8334eb`, against its merge base with `origin/main`
(`1bc97904`). The focus is newcomer-facing text in the ten `configbutler.ai/v1alpha3` CRDs and
the branch's changes to user documentation. This is an editorial review with targeted behavior
checks. A full correctness review of the controller changes is outside its scope.

The examples below are replacement descriptions, without Go comment prefixes. They use the
plain-text conventions required by [AGENTS.md](../../AGENTS.md). Markdown commentary follows the
[documentation style guide](../style-guide.md).

## What's good

- Field descriptions usually start by explaining the field's purpose.
- Defaults, namespace resolution, and immutable references are documented near the setting.
- The short descriptions already work. Keep fields such as `GitTarget.spec.path`,
  `GitProvider.spec.url`, and `GitTarget.spec.encryption` compact.
- `onRefusal` and `serializeNamespace` already give each value its own paragraph. Their structure
  works; converting those paragraphs to bullets is optional polish.
- The generated YAML excludes the long type-level design rationale I checked. For example,
  `GitTarget.spec.placement` uses the field comment. The internal discussion on
  `GitTargetPlacementSpec` stays in the source. Review the generated description before deciding
  a comment needs editorial work.

## Top risks (ranked)

1. **Some wording changes the reader's understanding of behavior.**
   `CommitRequest.spec.message` says the message is used as written, without explaining that
   `GitTarget.spec.commit.message.requestTemplate` can wrap it. `GitProvider.status.lastVerifiedAt`
   claims to show how long a connection has been broken; it records the last successful check.
   Failure may begin later, and `Ready=False` can also reflect signing or configuration failures.
   These need corrections before cosmetic edits.
2. **Most descriptions render badly in `kubectl explain`.** It keeps each source line break and
   wraps every line again at 76 characters of text, so a longer line leaves its last words on a
   line of their own. 616 of the 1,067 description lines are longer than that. See the
   [rendering measurements](../facts/crd-descriptions-in-kubectl-explain.md). Restructuring a
   description does not help until its lines fit.
3. **Grant defaults and selector pruning need clearer separation.** Grant descriptions compress
   omission, explicit lists, and combined permissions into a paragraph. Selector descriptions mix
   matching, deletion behavior, and conflicting rules. Separate these cases so readers can find
   the one that applies to them.
4. **Some references leave readers guessing which object or condition is meant.** Both watch-rule
   `objectSelector` descriptions refer to `spec.prune.mode` without naming the `GitTarget`.
   `ArgoCDClaim.status.conditions` mixes `True` and `False` with `NotConfigured`, which is a
   reason, not a condition status. It also omits `False` with reason `ArgoCDVersionNotSupported`.
5. **Long troubleshooting prose is squeezed into a table.** The new row about repeated
   `ClaimedResourceReconciled` events in the Flux guide contains the symptom, implementation
   cause, ownership explanation, and remedy. Give that case its own subsection and leave a short
   link in the table.

Evidence for the behavior corrections: [request message rendering](../../internal/git/commit_executor.go),
[provider reconciliation](../../internal/controller/gitprovider_controller.go), and
[Argo CD control conditions](../../internal/controller/argocdclaim_hold.go).

## What Flux does

Flux has a [documentation style guide](https://fluxcd.io/contributing/docs/style-guide/), maintained
in its [website source](https://github.com/fluxcd/website/blob/main/content/en/contributing/docs/style-guide.md).
It recommends active voice, present tense, direct wording, and avoiding idioms that make reading
harder for people who speak English as a second language. It puts related items that must appear
in a specific order into a list, and keeps paragraphs under six sentences. This is a website
writing guide; it does not prescribe a format for CRD comments.

The source offers useful examples too:

- In [GitRepository's API types](https://github.com/fluxcd/source-controller/blob/main/api/v1/gitrepository_types.go),
  `SecretRef` separates HTTPS and SSH requirements into sentences. `Provider` states its default
  separately. `GitRepositoryRef` describes precedence beside each choice.
- In [Kustomization's API types](https://github.com/fluxcd/kustomize-controller/blob/main/api/v1/kustomization_types.go),
  `Wait` states the action, its interaction with `HealthChecks`, and its default in separate
  sentences. Some other descriptions are still dense, so these are examples to learn from, rather
  than a uniform standard to copy.
- The [GitRepository reference](https://github.com/fluxcd/source-controller/blob/main/docs/spec/v1/gitrepositories.md)
  starts with a manifest and explains it with bullets. It gives authentication methods and branch
  selection their own sections.

Adopt those structural habits, and keep this repository's conventions for field names and literal
values. Flux's guide supports numbered lists for ordered items, such as a precedence order. Bullets
for unordered choices are this proposal's own recommendation. Sources checked on October 25, 2026;
the links track their default branches.

## Recommended changes (actionable)

### Use a small set of writing rules

- Lead with what the field controls. Follow with the default or omitted behavior.
- Use bullets for alternatives and independent requirements. Use numbered lists when order matters.
- Keep a short explanation in prose when there is no choice to compare.
- Keep existing per-value paragraphs when they already make the choices clear.
- Wrap description text at 76 characters after the Go comment prefix (risk 2). Fix that before
  optional paragraph-to-bullet changes.
- Name the subject: the `GitTarget`, the source cluster, or the referenced `GitBranch`.
- Prefer familiar verbs: `allows`, `uses`, `rejects`, and `restores`. Replace phrases such as
  `a written list`, `admits nobody`, and `hands the switch back` with concrete behavior.
- Keep deletion behavior, authorization, failure reasons, and immutability visible. Shorter text
  must preserve these details.
- Aim for two to four sentences in ordinary descriptions. Treat the existing 80-word target as
  a prompt to restructure longer text. Preserve necessary warnings.

### 1. Explain the message override accurately

Location: [CommitRequest.spec.message](../../api/v1alpha3/saverequest_types.go).

```text
Message supplies the requested commit message. Omitted, the GitTarget's
spec.commit.message.liveTemplate generates the message.

The text is never evaluated as a template. If the GitTarget configures
spec.commit.message.requestTemplate, that template wraps this text.
Otherwise, the text is used as written.

The message must contain 1–1024 characters and at least one non-whitespace
character. Newlines are allowed for a subject and body; other control
characters are rejected.
```

Short paragraphs work here: the message policy and validation are separate topics, but neither
needs an artificial list.

### 2. Describe the timestamp without claiming an outage duration

Location: [GitProvider.status.lastVerifiedAt](../../api/v1alpha3/gitprovider_types.go).

```text
LastVerifiedAt is the time of the last successful repository connectivity
check using this GitProvider's credentials, or anonymous access when
spec.secretRef is omitted. Failed checks leave this timestamp unchanged.
Check status.conditions for the reason the GitProvider is not ready.
```

The controller updates the timestamp only after a successful remote check. The timestamp cannot
identify when a later failure began.

### 3. Distinguish condition status from reason

Location: [ArgoCDClaim.status.conditions](../../api/v1alpha3/argocdclaim_types.go).

Replace the paragraph beginning `InControl is True` with:

```text
InControl reports whether GitOps Reverser controls when the Application
syncs:

- True, reason AutomatedSyncOff: GitOps Reverser controls sync timing.
- False: control is being released or revoked, or no GitTarget references
  the claim.
- False, reason NotConfigured: another field manager has enabled automated
  sync.
- False, reason ArgoCDVersionNotSupported: the Application has no
  spec.syncPolicy.automated.enabled field (Argo CD before 3.1), so GitOps
  Reverser cannot turn automated sync off.
- Unknown, reason NotConfigured: the claim is not configured to control the
  Application.

A claim with no referencing GitTarget reads no applier and reports
Ready=True, reason NotReferenced.
```

These cases come from `holdArgoCD`, `ownedSwitchCondition`, and `switchOff`. The version check in
`switchOff` also catches an Argo CD downgrade after the configuration check. Apply the same
status/reason distinction when editing the longer `Configured` paragraph and Flux conditions.

### 4. Turn namespace choices into bullets

Location: [ConsumerNamespaces.From](../../api/v1alpha3/access_grants.go).

The existing sentence puts four enum values and four explanations in one line of thought.
Each value is a separate choice:

```text
From selects which namespaces this grant allows consumers from:

- "Same": the namespace of the object that defines the grant.
- "Names": the namespaces listed in names.
- "Selector": namespaces whose labels match selector.
- "All": every namespace.
```

### 5. Make the omitted and empty grant lists easy to distinguish

Location: [GitBranch.spec.allowedGitTargets](../../api/v1alpha3/gitbranch_types.go).

The distinction is already present, but `a written list is the whole policy` makes the reader
translate it. State each case:

```text
AllowedGitTargets controls which GitTargets may write through this
GitBranch.

- Omitted: allows GitTargets in this GitBranch's namespace.
- Empty list ([]): allows no GitTargets.
- Nonempty list: allows only GitTargets matched by at least one grant.

An explicit list replaces the default. Include a grant for this GitBranch's
namespace if GitTargets there still need access.
```

Apply the same structure to `KubeProvider.spec.allowedGitTargets` and `allowedApplierClaims`,
preserving their source-namespace and applier-namespace defaults. Keep
`GitProvider.spec.allowedGitBranches` distinct: both omission and an empty list deny access.

### 6. Give each receiver type its own line

Location: [GitReceiver.spec.type](../../api/v1alpha3/gitreceiver_types.go).

```text
Type selects the push payload and authentication format:

- "github": GitHub payload with X-Hub-Signature-256.
- "gitlab": GitLab payload with X-Gitlab-Token.
- "gitea": Gitea payload with X-Gitea-Signature.
- "generic-hmac": a JSON payload with repository, branch, and after fields,
  authenticated by X-Signature: sha256=<hex HMAC of the body>.
```

This also replaces the JSON-like `{"repository", "branch", "after"}` fragment, which names fields
but is not valid JSON.

### 7. Keep template fields available, but group them

Location: [GitTarget.spec.commit.message.liveTemplate](../../api/v1alpha3/gitprovider_types.go).

The current description combines the template context, per-resource fields, empty-value handling,
and request overrides. It also omits Kind, Labels, and the Label accessor, which the
[commit message guide](../commit-messages.md) documents. Group the context and add the missing
fields:

```text
LiveTemplate is the Go template for commit messages generated from live
edits. An empty value uses the built-in template.

The template receives:

- Author: the username, which may be empty.
- GitTarget: the target name.
- Count: the edit count after coalescing and before comparison with Git.
- Operations: counts of retained entries by operation kind.
- Resources: the resources in the commit window.

Each resource provides Operation, Group, Version, Resource, Kind, Namespace,
Name, APIVersion, ResourceVersion, Generation, and Labels. Read one label
with {{.Label "key"}}. ResourceVersion identifies the state written by the
commit and may be empty. Kind and Labels are empty for a deletion.
Generation is 0 for a kind without a spec. Guard Kind, ResourceVersion, and
Generation with {{with}}.

A CommitRequest message takes precedence without changing the author. The
requestTemplate field controls whether that message is wrapped.
```

Keep examples and longer context definitions in the existing [commit message guide](../commit-messages.md).

### 8. Move the long Flux troubleshooting row into a subsection

Location: [Flux troubleshooting](../bi-directional-flux.md#troubleshooting).

Proposed subsection text:

> Repeated `ClaimedResourceReconciled` events mean something is undoing the claim's suspension.
> A rising `claimed_resource_reconciled_total` records these corrections. If the claim also reports
> `OperationUnresolved`, inspect the controller that writes the `Kustomization`.
>
> A controller that replaces the whole spec can remove `spec.suspend` by leaving it out. Flux can
> then apply before GitOps Reverser suspends the `Kustomization` again. This removal does not take
> ownership of `spec.suspend`, so `SuspendOwnedElsewhere` does not identify it.
>
> Update that controller to use one of these approaches:
>
> - Preserve the `spec.suspend` value from the object it read when replacing the spec.
> - Use server-side apply and omit `spec.suspend` from the fields it manages.

Keep the symptom in the table and link to this explanation. The adjacent `Overriding by hand`
bullet also benefits from two sentences: first the unchecked apply, then the claim's response.

### 9. Optional: use bullets for refusal choices

Location: [GitTarget.spec.onRefusal](../../api/v1alpha3/gittarget_types.go).

The existing description already separates `Ignore` and `Reapply` into paragraphs. Keep it unless
a rendered comparison shows a benefit. This alternative mainly separates the restrictions on
`Reapply` from the value's immediate effect:

```text
OnRefusal controls what happens when a live edit cannot be written to the
folder. Defaults to "Ignore". A refused edit makes no commit and remains on
the cluster until an applier overwrites it from Git.

- "Ignore": reports the refusal in conditions and events.
- "Reapply": uses spec.applierClaimRef to reapply Git after pending edits
  are published, reverting the refused edit. Requires spec.applierClaimRef.

Reapply runs only when the document already exists in the folder and the
write removes no documents. It does not run for folder-wide failures, such
as invalid YAML. A refusal that persists triggers one reapply, rather than
one per check.
```

This is a justified exception to the word-count target: the restrictions prevent a reader from
expecting a reapply to repair every refusal.

### 10. Optional: use bullets for namespace serialization

Location: [GitTarget.spec.serializeNamespace](../../api/v1alpha3/gittarget_types.go).

Omission, `true`, and `false` already have separate paragraphs. Bullets may make the alternatives
easier to scan, but this is a lower-priority presentation choice. Correct line wrapping first:

```text
SerializeNamespace controls whether written documents include
metadata.namespace. It applies to every write of a namespaced resource.
Cluster-scoped resources ignore this setting.

- Omitted: leaves out metadata.namespace only when the kustomization
  governing the document sets the same namespace.
- true: always includes metadata.namespace. Use this for a flat folder
  applied directly.
- false: always omits metadata.namespace. The namespace must come from a
  kustomization, a Flux Kustomization's targetNamespace, or an Argo CD
  Application's destination.namespace.

With false, the folder accepts only one source namespace. A WatchRule that
adds a second is rejected with GitPathAccepted=False, reason
MultipleSourceNamespaces.
```

### Remaining pass, in priority order

| Location | Proposed treatment |
| --- | --- |
| Both watch-rule `objectSelector` fields | Name `GitTarget.spec.prune.mode`; separate pruning and selector conflicts. |
| `GitTarget.spec.placement` | Separate existing-document behavior, path variables, and fallback restrictions. |
| `KubeProvider.spec.attribution.auditRoute` | Separate routing, default, namespace ownership, and conflict behavior. |
| `GitBranch.spec.parentBranch` | Separate default-parent selection from missing-parent behavior. |
| `GitProvider.spec.knownHostsRef` | Use a numbered list for the three sources in precedence order. |
| `GitTarget.spec.prune.mode` | Use one bullet for each mode, with the default first. |
| Both claim kinds' root descriptions | Lead with control of apply timing; separate the trigger cases. |
| `GitTarget.spec.suspend` | Separate stopping new commits, the final push, and resuming from cluster state. |
| Upgrade entry for removed message templates | Separate removal, client behavior, and the migration action. |

## Implementation order

1. Correct the message, timestamp, condition, template-field, and cross-object-reference wording.
2. Rewrap every description at 76 characters, in a commit of its own so the diff stays mechanical.
   In the same change, add a `task lint` check that fails on any `description` line longer than 76
   characters under `config/crd/bases`, and add the limit to the CRD house style in
   [AGENTS.md](../../AGENTS.md). Without the check, the next edit brings long lines back.
3. Apply the enum and grant-list examples.
4. Restructure the remaining long descriptions and the Flux troubleshooting case.
5. Preview `kubectl explain` output, using the envtest recipe in the
   [rendering measurements](../facts/crd-descriptions-in-kubectl-explain.md#how-to-reproduce).
   Consider the optional changes to `onRefusal` and `serializeNamespace` only if they improve
   that output.
6. Regenerate CRDs into a scratch directory and compare with the pre-edit branch snapshot after
   stripping every `description` key. Preserve scopes, defaults, validation, and list semantics.
7. Run the repository's required validation for an API-comment implementation. For this proposal
   alone, run `task lint-docs` and Vale on this file.
