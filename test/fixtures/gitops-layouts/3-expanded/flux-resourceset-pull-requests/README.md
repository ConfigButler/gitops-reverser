# flux-resourceset-pull-requests

## What this is
Ephemeral **preview environments**, one per open pull request. A
`ResourceSetInputProvider` of type `GitHubPullRequest` queries the Git host's API
for open PRs carrying the `preview` label, and a `ResourceSet` renders a
namespace + deployment for each one.

This is the same expansion mechanism as
[`flux-resourceset-inline`](../flux-resourceset-inline/), with one change that
breaks a much deeper assumption: **the inputs are not in the repository.**

## Layout
```
flux-resourceset-pull-requests/
├── README.md
└── previews/
    └── preview-envs.yaml    # ResourceSetInputProvider + ResourceSet
```

## What makes it structurally distinct
- **The desired object set is not knowable from the repository.** Every other
  fixture in this corpus can, in principle, be answered by reading files — even
  the ones we refuse. Here the answer is "however many pull requests are open
  right now", which lives in GitHub's database. No repository scan can enumerate
  it. Not because the scan is weak, but because **the answer is not here.**
- **The set changes with no commit.** Someone opens a PR: a namespace appears.
  Someone merges it: the namespace is pruned. Git does not move. There is no
  revision that corresponds to the change, so there is no revision to reconcile
  against, and nothing for a live→Git tool to write.
- **A `secretRef` to a real credential.** The provider authenticates to the Git
  host. The repository's behaviour depends on a token, not just on its contents.
- **`inputsFrom`, not `inputs`.** The inputs are a *reference* to a live object
  whose `status.exportedInputs` is populated by a controller. The input set is
  itself expansion.
- **The image tag is a commit SHA from another repository.**
  `ghcr.io/example-org/frontend:<< inputs.sha >>` — the tag is the PR's head
  commit in `example-org/frontend`, which is not this repo.

## Open questions
- A repository scan reports "what is here". For this repo, the honest report is
  "the objects this repo produces cannot be determined from this repo". Is that a
  refusal, a layout classification, or a new kind of answer entirely?
- `status.exportedInputs` is live-only and unwritable. The `ResourceSet` and the
  provider are ordinary editable KRM; their *inputs* are not. Where is the
  boundary drawn on a single document whose spec is editable and whose status is
  the only thing that matters?
- If a preview namespace is mirrored to Git, the next merge prunes the live
  objects and Git keeps a folder for a pull request that no longer exists. What
  deletes it?
- An intent cluster hydrated from this repo would need the GitHub token to
  materialise anything at all. Is a repository whose expansion requires a live
  credential hydratable, even in principle?
