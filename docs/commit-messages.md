# Commit messages

Every commit GitOps Reverser makes gets its message from one of three templates on
[`GitTarget.spec.commit.message`](configuration.md#gittargetspeccommit-how-writes-become-commits).
This page is the reference for all three. Nothing here is required: leave `message` unset and the
built-in templates apply.

`liveTemplate` formats every live window, including one retained entry and `0s` windows.
`reconcileTemplate` formats atomic snapshots and resyncs. Invalid templates report
`Validated=False` with reason `InvalidConfig`; validation exercises singleton and mixed-operation
windows, empty authors, and scoped and whole-target snapshots through the production renderer.
Sample execution cannot prove every possible conditional branch valid. That applies to
`requestTemplate`'s [message check](#framing-a-save-message) too: a template that drops the save
message only on some branch passes validation and falls back at commit time.

| Input, in precedence order | Message source |
|---|---|
| Attached [save request](configuration.md#commitrequest) **and** `requestTemplate` set | `requestTemplate`, with the request's message as `.RequestMessage` |
| Non-empty literal override, including an attached [save request](configuration.md#commitrequest) | Exact supplied text |
| Live window of any size | `liveTemplate` |
| Atomic snapshot or resync | `reconcileTemplate` |

The default live template produces a Conventional Commits subject and one retained entry per body line:

```yaml
spec:
  commit:
    message:
      liveTemplate: |-
        chore: sync {{.Count}} resource{{if ne .Count 1}}s{{end}}

        {{range .Resources -}}
        - [{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Namespace}}/{{.Name}}
        {{end -}}
      reconcileTemplate: "chore: reconcile {{.Count}} {{if .Resource}}{{.Resource}}{{else}}resources{{end}}{{if .Namespace}} in {{.Namespace}}{{end}}{{if .ResourceVersion}} (last resourceVersion: {{.ResourceVersion}}){{end}}"
```

| Template | Fields |
|---|---|
| `liveTemplate` | `Author`, `GitTarget`, `Count`, `Operations`, `Resources`, and the `LabelValues` / `LabelValue` accessors |
| `requestTemplate` | the same fields, plus `RequestMessage` |
| Each `Resources` entry | `Operation`, `Group`, `Version`, `Resource`, `Kind`, `Namespace`, `Name`, `APIVersion`, `ResourceVersion`, `Generation`, `Labels`, and the `Label` accessor |
| `reconcileTemplate` | `Count`, `GitTarget`, `Group`, `Version`, `Resource`, `APIVersion`, `Namespace`, `ResourceVersion` |

Live `Count` counts retained entries after window coalescing, before the writer compares them with
Git. Repeated edits to one resource collapse to one entry, with the last operation retained.
`Operations` counts those retained `CREATE`, `UPDATE`, and `DELETE` entries; `Resources` preserves
first-seen order. An entry already matching Git still counts, and several entries may share a file.
The count can exceed the number of changed resources. A no-op creates no commit, even with a literal
message. Printing a resource entry directly keeps its `group/version/resource[/namespace]/name` form.

`RequestMessage` belongs to `requestTemplate`. It exists on the live context too, but is always
empty there: a window carrying a save request's message renders `requestTemplate` when one is
configured and the literal message when one is not, so `liveTemplate` only ever runs for windows
that have no request message. Do not reach for `{{if .RequestMessage}}` inside `liveTemplate`;
it never fires.

`Author` is the raw window username and is empty when no actor is named. It does not use an OIDC
display name or the `attribution-unresolved` Git author sentinel. The sentinel appears only in the
Git author header when attribution ran without resolving an actor. Messages never change authorship.

## Framing a save message

By default a [save request](configuration.md#commitrequest)'s message **replaces** the template, so supplying one
costs you the resource body `liveTemplate` would have produced: the commit says why, but no longer
says what. `requestTemplate` composes the two.

```yaml
spec:
  commit:
    message:
      requestTemplate: |-
        {{.RequestMessage}}

        {{range .Resources -}}
        - [{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Namespace}}/{{.Name}}
        {{end -}}
```

It renders only for a window a save request attached to; every other window is unaffected. Omit it
and request messages are committed verbatim, exactly as before.

The request is **never** parsed as a template. Its message arrives as `.RequestMessage` and is
committed unaltered, so a save-button user supplies the content while the operator owns the
wording around it. Nothing a requester writes is ever executed, and braces inside a request message
stay literal in every case.

A `requestTemplate` that never renders `.RequestMessage` is **rejected** with `Validated=False`,
because dropping the requester's stated reason is the one thing this field must not do. Every
spelling that puts the message in the commit is accepted (`{{.RequestMessage}}`, a pipeline, or a
variable), because the check renders the template and looks for the message in the output rather
than scanning the template's text.

`requestTemplate` is validated against the same window shapes as `liveTemplate`, so a template
reading a label some resources do not carry fails at admission rather than at commit time. If one
does fail to render in production, the request's message is committed verbatim instead of the
window being lost, and the commit is counted under `message_source="commit_request_fallback"`.
Alert on that rate: the commit itself succeeds and no condition moves, so it is the only signal.

## Kind, scope, and labels

`Kind` is the commit-message spelling of the `{kind}` [placement variable](configuration.md#template-variables),
and `Namespace` now answers the scope question completely: it renders the resource's namespace, or
the literal `_cluster` when the resource is cluster-scoped, exactly as `{namespace}` does in a path.
A body line no longer has to guard it with `{{if .Namespace}}`, because the field is never blank;
the default template no longer does.

`Labels` is the resource's labels as the writer commits them, and is read with the `Label`
accessor:

```yaml
liveTemplate: |-
  chore: sync {{.Count}} resource{{if ne .Count 1}}s{{end}}{{with .LabelValue "team"}} for {{.}}{{end}}

  {{range .Resources -}}
  - [{{.Operation}}] {{.Kind}} {{.Namespace}}/{{.Name}} ({{.Label "app.kubernetes.io/instance"}})
  {{end -}}
```

Three things to know:

- **Read a label with `{{.Label "team"}}`, never `{{.Labels.team}}`.** These templates render with
  `missingkey=error`, so indexing a label a resource does not carry does not render empty: it fails
  the render, and a failed render fails the whole commit, losing the window until the next resync.
  `Label` returns the empty string instead. Validation catches the dotted form (its sample events
  include a resource with no labels), so such a template is rejected at admission rather than at
  2am, but the accessor is the spelling to write.
- **A commit spans n resources, so a label is a set here.** `LabelValues "team"` is the sorted,
  distinct list of values in this commit, skipping resources that do not set it; `LabelValue "team"`
  is the single value when the whole commit agrees on one, and empty when it does not. A subject
  line that names a team is only honest under the second. The two differ on a resource that does
  not carry the label: `LabelValues` skips it, `LabelValue` treats it as a disagreement and renders
  nothing, so a commit holding one labeled and one unlabeled resource is named after neither.
- **A `DELETE` carries no object**, because the resource is already gone from the cluster, so
  `Kind` and `Labels` are empty for one. The identity fields (`Name`, `Namespace`, `Resource`, …)
  are unaffected, and so are `ResourceVersion` and `Generation`: the watch delivers the final
  object, so the last state that existed is still knowable even though the object is not. A commit containing a
  `DELETE` therefore has no agreed `LabelValue`: what the deleted resource was labeled is not
  something the window still knows.

## Naming the state a commit wrote

Two counters describe the observed state each entry was at. Neither is committed to the file
(`resourceVersion` and `generation` are stripped from every manifest on purpose, because a counter
inside a manifest would make every observation a byte change); both travel beside the object, in
the message only. Both are off by default, and both are guarded with `{{with}}`:

```yaml
liveTemplate: |-
  chore: sync {{.Count}} resource{{if ne .Count 1}}s{{end}}

  {{range .Resources -}}
  - [{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Namespace}}/{{.Name}}{{with .ResourceVersion}}@{{.}}{{end}}{{with .Generation}} gen{{.}}{{end}}
  {{end -}}
```

| | `{{.ResourceVersion}}` | `{{.Generation}}` |
|---|---|---|
| moves when | **anything** is written, `/status` included | only the **desired state** changes |
| present on | every resource the operator observed | only kinds with a spec: **not** a ConfigMap or a Secret |
| absent value | `""` | `0` |
| safe to compare | for **equality** only | yes: per object, starts at `1`, one step per spec write |

**Turn `ResourceVersion` on to make a commit joinable**: to an audit log entry, to a
`kubectl get -o yaml` taken at the time, to another operator's logs. Equal to the object's current
version means nothing is pending. Do not subtract two of them: `resourceVersion` is opaque by API
contract and is the cluster-wide store revision in practice, so two consecutive commits of one
ConfigMap can read `@1331` then `@8402` with nothing skipped, because every other object's writes
moved the same counter.

**Turn `Generation` on to see what a commit missed.** It is the counter a gap is meaningful in: a
commit at `gen3` following one at `gen6` means three spec changes that were not committed
separately. That is the question `ResourceVersion` cannot answer.

Read them together, because each is blind where the other sees:

- **`ResourceVersion` lags, deliberately.** An update whose committed content would be identical (a
  `/status`-only write) is never routed, so the version in a commit can be older than the object's
  current one. A resource re-edited inside one window contributes the last routed version, and an
  entry that already matched Git still contributes one.
- **`Generation` misses metadata.** A label- or annotation-only edit changes what gets committed
  without moving it, so an unchanged `Generation` across two commits does not mean an unchanged
  commit. And it is `0` for every spec-less kind, which is much of what a typical target mirrors.
- **Neither counts skips.** To count what the operator deliberately did not route, read
  `watch_events_total{outcome="unchanged"}`, which is exactly that census.

A producer that observed no object renders nothing for either: reconcile, resync, and bootstrap
writes all leave both empty.

`reconcileTemplate` gets none of these. It describes a *type* being reconciled rather than a list
of resources, so there are no labels to read, and its own `{{.ResourceVersion}}` is the snapshot
`LIST`'s version: one value for the whole run, not any single object's. Live names n resources, so
it names each one's version; reconcile names a type, so it names the snapshot's. Its `Namespace`
keeps the plain meaning it always had: the namespace a namespace-scoped reconcile covered, empty for a whole-target or all-namespaces
one. That emptiness means "every namespace", not "cluster-scoped", so the `_cluster` sentinel would
be a lie there rather than a convenience.

## Two languages, one vocabulary

Placement templates and commit templates are deliberately different renderers: a path has to be
statically checkable (that is what lets the operator prove a Secret route cannot collide two
Secrets onto one file), while a commit message has to iterate over n resources, which needs
`range` and `if`. Neither language can do the other's job.

The nouns are the same in both, and only the spelling follows each host language:

| Concept | Placement | Commit message |
|---|---|---|
| namespace, or `_cluster` when cluster-scoped | `{namespace}` | `.Namespace` |
| kind | `{kind}` | `.Kind` |
| name | `{name}` | `.Name` |
| API version | `{apiVersion}` | `.APIVersion` |
| one label | `{label:team}` | `.Label "team"` |
| the observed version | (none) | `.ResourceVersion` |
| the desired-state counter | (none) | `.Generation` |

The two counters are the first nouns that legitimately exist on only one side. A path keyed on a
counter would write a new file whenever it moved, which is the opposite of what placement is for: a
path has to be stable and statically checkable. A message describes one moment, so it can name
one.

The capitals are Go's, not a style choice: `text/template` can only reach exported struct fields,
which must begin with one. The braces are lower-case because a placement template reads like the
manifest it is filing (`metadata.namespace`). A label needs a method call rather than a field on
the commit side because a Go template field name cannot contain a `:` or a `/`.

Reconcile type fields name the synced type; `Namespace` names a namespace-scoped snapshot.
Whole-target snapshots leave those fields empty. `ResourceVersion` is the snapshot's resourceVersion
and can be absent, including a pure sweep. Guard optional values as in the example.

`ResourceVersion` was called `Revision` before `v0.48.0`. A `reconcileTemplate` still naming
`{{.Revision}}` is rejected, and the `GitTarget` says so on its `Validated` condition. See
[the upgrade note](UPGRADING.md#reconciletemplates-revision-is-now-resourceversion).

A reconcile runs per *cell* (a (type, namespace) pair) rather than per target, so a
namespace-scoped run covers exactly one namespace and the default subject names it:

```text
chore: reconcile 4 configmaps in team-a (last resourceVersion: 1331)
```

Without it, a target watching one type in two namespaces writes two byte-identical subjects, which
is the same reason the type is in there. `Namespace` stays guarded by `{{if}}` rather than falling
back to a sentinel the way `Resources[i].Namespace` does, and the difference is not an oversight:
per resource, empty has exactly one meaning (the kind has no namespaces), so `_cluster` is a true
name for it. Per run, empty covers two different facts: an all-namespaces sweep of a namespaced
type, and a cluster-scoped type that has no namespaces. No single word is true of both, so the
honest rendering is to say nothing.

`eventTemplate` and `groupTemplate` are retired and rejected. Follow the
[upgrade instructions](UPGRADING.md#one-live-commit-message-template) to migrate existing templates.
