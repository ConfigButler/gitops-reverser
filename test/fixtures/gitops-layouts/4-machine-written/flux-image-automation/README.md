# flux-image-automation

## What this is

Flux's image automation: `ImageRepository` scans a container registry,
`ImagePolicy` picks one tag, and `ImageUpdateAutomation` **writes that tag back
into the Git repository and pushes a commit**.

This is the one fixture in the corpus where a controller is a *writer* of the
repository rather than only a reader of it. Anything else that edits these files
shares the repo with a non-human committer that runs on an interval.

## Layout

```
16-flux-image-automation/
├── README.md
├── apps/
│   └── frontend/
│       ├── deployment.yaml       # carries a load-bearing $imagepolicy comment
│       ├── service.yaml
│       └── kustomization.yaml
├── infrastructure/
│   └── image-automation/
│       ├── imagerepository.yaml  # scans the registry
│       ├── imagepolicy.yaml      # selects one tag
│       ├── imageupdateautomation.yaml  # commits the tag back to Git
│       └── kustomization.yaml
└── clusters/
    └── production/
        ├── apps.yaml             # Flux Kustomization CR
        ├── image-automation.yaml # Flux Kustomization CR
        └── kustomization.yaml    # kustomize build file
```

## What makes it structurally distinct

- **A YAML comment is load-bearing.** In `apps/frontend/deployment.yaml` the
  image line ends with `# {"$imagepolicy": "flux-system:frontend"}`. That comment
  is a kustomize *setter*: it is the only thing that marks the field as
  automated. Strip it, reformat it away, or move it to its own line, and
  automation silently stops updating that image. A comment-destroying rewrite of
  this file is a functional regression that no schema validator would catch.
- **The marker is a cross-object reference in string form.** `flux-system:frontend`
  is `<namespace>:<name>` of the `ImagePolicy`. Nothing in the Deployment's schema
  expresses that dependency; renaming the ImagePolicy breaks it silently.
- **Git is an output, not only an input.** `ImageUpdateAutomation` clones, rewrites,
  commits as `fluxcdbot`, and pushes to `flux-image-updates`. The checked-in image
  tag is therefore both desired state *and* a value the cluster produces.
- **`update.path` scopes the writer.** Only `./apps/frontend` is rewritten. An
  identical setter comment outside that subtree is inert.
- **`spec.git.commit.messageTemplate` is a Go template inside a Kubernetes object.**
  Its `{{range .Updated.Images}}` body is data to the API server and a program to
  the controller.
- **Referenced but absent:** the `ghcr-credentials` Secret and the `flux-system`
  `GitRepository` live in the cluster, created elsewhere. Not every reference a
  folder makes is resolvable inside that folder.

## Open questions

- If another tool rewrites `deployment.yaml`, how does it guarantee the trailing
  `$imagepolicy` comment survives — byte for byte, on the same line as the image?
- Two writers now target the same file on the same branch. What happens when an
  automated tag bump and a human edit race? Who rebases, and what is the conflict
  unit — the file, the document, or the field?
- Is the image tag in Git a value a user should be allowed to edit at all, given a
  controller will overwrite it on the next interval? Should such a field be
  presented as read-only?
- `push.branch` sends commits to `flux-image-updates` rather than `main`. Does the
  "desired state" of this folder live on the branch a tool reads, or the branch
  automation writes?
- Can a scanner detect that a field is machine-owned purely from structure, or does
  recognising `$imagepolicy` require special-casing this controller's convention?
- How many other conventions encode meaning in comments (kustomize setters, `# yaml-language-server`
  schema hints, `# renovate:` directives), and does treating comments as
  presentation-only quietly break all of them?
