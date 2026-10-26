# CRD descriptions in `kubectl explain`: keep each line to 76 characters

> **reference**: durable background. Index: [`../INDEX.md`](../INDEX.md)
>
> Measured on 2026-10-25 with `kubectl` v1.37.1 against an envtest API server (the binaries in
> `bin/k8s/current`), using this repository's CRDs at commit `0b8334eb`. The behavior facts at the
> end were read from the controller source at that commit.

## Why this matters

A CRD field's doc comment is the text an operator reads in `kubectl explain`. How it renders there
depends on where the comment's lines break as well as on what it says. Most of our descriptions
currently render with stray words on their own lines, because their source lines are too long for
the terminal output. Restructuring a description, for example into bullets, does not help until the
line width is right.

## The facts

1. **controller-gen keeps the comment's line breaks.** Each line of the Go doc comment becomes a
   line of the generated `description`, written as a `|-` block scalar in `config/crd/bases`. A
   blank comment line becomes a blank line in the description.

2. **`kubectl explain` keeps those line breaks, then wraps each line again at 80 columns.** The
   output indents the description by 4 spaces, so a line keeps 76 characters of text. A line of 76
   characters prints whole. A line of 77 or more has its last words pushed onto a line of their
   own, and the next source line starts on a new line again. Today's
   `GitTarget.spec.serializeNamespace` prints like this:

   ```text
   SerializeNamespace decides whether a namespaced resource's document carries
   metadata.namespace. It applies to every write, not just the first, and is
   ignored for
   cluster-scoped resources.
   ```

3. **Lists render as lists.** A line starting with a `-` bullet stays at the start of its own line, so
   bullets work in `kubectl explain`. A bullet's continuation lines are subject to the same
   76-character limit as any other line.

4. **Most of the current text is too wide.** At `0b8334eb`, 616 of the 1,067 description lines
   across the ten `configbutler.ai` CRDs are longer than 76 characters. The longest is 138.

## What follows for writing descriptions

- Wrap the text of every API doc comment at 76 characters, measured after the `//` prefix and its space. A
  shorter line costs nothing in the source and prints whole in the terminal.
- Use a blank comment line to start a paragraph, and `-` bullets for a list of values or cases.
- Check the width mechanically rather than by eye: any line longer than 76 characters in a
  `description` under `config/crd/bases` will print badly.
- The house style for the words themselves is in [AGENTS.md](../../AGENTS.md), under API Changes,
  and [style-guide.md](../style-guide.md) covers prose in markdown.

## How to reproduce

`kubectl explain` needs a server that publishes the CRD's OpenAPI schema; a running cluster is not
required. Start an envtest API server with the CRDs loaded, write an admin kubeconfig, and run
`kubectl explain`. Retry for a few seconds, because the schema is published shortly after the CRD
is created and an early call fails with `couldn't find resource`:

```go
env := &envtest.Environment{CRDDirectoryPaths: []string{crdDir}, ErrorIfCRDPathMissing: true}
cfg, _ := env.Start()
defer env.Stop()
user, _ := env.AddUser(envtest.User{Name: "admin", Groups: []string{"system:masters"}}, cfg)
kubeconfig, _ := user.KubeConfig()
// write kubeconfig to a file, then retry until it succeeds:
//   kubectl --kubeconfig <file> explain gittarget.spec.serializeNamespace
```

Run it with `KUBEBUILDER_ASSETS=bin/k8s/current`. To test a draft, edit the `description` in a copy
of the generated CRD and point `crdDir` at the copy.

## Behavior a description must match

These came out of the same review. Each one is behavior that a shorter or rearranged description
can easily misstate.

- **A SaveRequest's message can be wrapped.** The message is never evaluated as a template.
  When the GitTarget sets `spec.commit.message.requestTemplate`, that template frames the message
  as `RequestMessage`; without one, the message is committed as written. See
  [`commit_executor.go`](../../internal/git/commit_executor.go).
- **`GitProvider.status.lastVerifiedAt` records the last success only.** It is set after a
  successful connectivity check and never cleared. Every earlier failure in the same reconcile
  also leaves it unchanged: a signing-key problem or a missing or malformed Secret stops the
  reconcile before the connectivity check. So it bounds when the provider last worked, not when a
  failure began. See [`gitprovider_controller.go`](../../internal/controller/gitprovider_controller.go).
- **`ArgoCDClaim` `InControl` uses all three statuses.** See
  [`argocdclaim_hold.go`](../../internal/controller/argocdclaim_hold.go):

  | Status | Reason | When |
  | --- | --- | --- |
  | `True` | `AutomatedSyncOff` | Automated sync is off and GitOps Reverser starts every sync, including when another field manager owns the switch and keeps it off. |
  | `False` | `NotReferenced` | No GitTarget references the claim. |
  | `False` | `Releasing`, `Revoking` | The claim is handing the Application back. |
  | `False` | `ArgoCDVersionNotSupported` | The Application has no `spec.syncPolicy.automated.enabled` (Argo CD before 3.1). |
  | `False` | `NotConfigured` | Another field manager owns the switch and has turned automated sync on. |
  | `Unknown` | `NotConfigured` | `Configured` is not `True` for any other reason. |

  On a failed read of the Application, the previous `InControl` value is kept.
- **Grant lists: omitted and empty mean different things, except on GitProvider.** On
  `GitBranch.spec.allowedGitTargets` and both `KubeProvider` grant lists, omitting the list
  defaults it to one grant for the object's own namespace, while `[]` allows nobody. On
  `GitProvider.spec.allowedGitBranches` there is no default: omitted and `[]` both deny every
  GitBranch, including those in the provider's own namespace.

## Writing-style references worth borrowing

- **Flux's [documentation style guide](https://fluxcd.io/contributing/docs/style-guide/)**
  (source in [fluxcd/website](https://github.com/fluxcd/website/blob/main/content/en/contributing/docs/style-guide.md)).
  It asks for active voice, present tense, and direct wording, and it avoids idioms that are hard
  for readers whose first language is not English. It also puts related items that must appear in
  a specific order into a list, and keeps paragraphs under six sentences. It is written for
  Flux's website, and it fits this repository's docs and CRD descriptions as well.
- **Flux's API types, as worked examples of CRD descriptions.** In
  [GitRepository](https://github.com/fluxcd/source-controller/blob/main/api/v1/gitrepository_types.go),
  `SecretRef` gives HTTPS and SSH requirements separate sentences, and `Provider` states its
  default on its own. In
  [Kustomization](https://github.com/fluxcd/kustomize-controller/blob/main/api/v1/kustomization_types.go),
  `Wait` gives the action, its interaction with `HealthChecks`, and its default one sentence each.
  Not every Flux description is this clean, so treat these as examples rather than a standard.
