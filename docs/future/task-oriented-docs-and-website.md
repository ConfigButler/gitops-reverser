# Task-oriented documentation and a static website

Status: proposed documentation and website plan. The navigation, page splits, and website described
here are future work.

## Goal

Give GitOps Reverser a public website that helps people understand the product, install it, and
complete common tasks, built from the same Markdown sources as the repository.

Organize the reader-facing navigation around outcomes. A reader should be able to find the next
step without already knowing names such as `GitTarget`, attribution, or API-first publication.
Keep architecture and configuration reference available alongside the task guides.

## The four main categories

These categories form the documentation landing page and the website's documentation navigation.
They describe reader journeys; they do not require matching physical folders.

### 1. Get started

Offer two visible setup paths. The deciding question is: does Flux or Argo CD apply the same folder
GitOps Reverser writes? If yes, follow the bi-directional path even when the immediate goal is only
to record cluster changes. An existing deployment loop needs coordination regardless of whether
anyone plans to edit Git manually.

**Capture cluster state in Git.** This path reaches a verified commit without an applier claim or
a Git push webhook. Describe it as capturing selected cluster state: batching and restart recovery
mean Git history is not a complete record of every event.

- **Install GitOps Reverser.** For a cluster administrator: prerequisites, credentials, permissions,
  installation, a first captured change, verification, and cleanup. Keep the default path usable
  without audit attribution.
- **Start capturing a team's resources.** For a platform administrator or team owner: choose the
  source cluster, namespaces, resource selection, repository, branch, and folder. Cover adopting
  an existing repository through a scratch branch, supported layouts, deletion policy, and
  cross-namespace access.
- **Record who made a change (optional in both paths).** Check cluster eligibility, configure audit
  delivery and attribution, and verify a known actor's commit. See the dedicated scope below.

**Coordinate changes from Kubernetes and Git.** Build on the installation and resource-selection
steps above.

- **Choose bi-directional coordination.** Explain when the shared-folder deployment loop needs
  coordination, from the [bi-directional GitOps](../bi-directional.md) overview.
- **Configure the applier.** Follow the [Flux](../bi-directional-flux.md) or
  [Argo CD](../bi-directional-argocd.md) guide to configure the claim and connect the target.
  Explain who controls applying the folder and what happens when the operator is unavailable.
- **React promptly to Git pushes.** Configure the Git host's push webhook and a `GitReceiver`.
  See the dedicated scope below.
- **Verify both directions.** Capture a Kubernetes edit, then push a Git edit and verify the
  coordinated apply.

Installation happens at the platform level; onboarding repeats for each team or destination. Keep
those outcomes separate so a team can start using an existing installation without repeating the
administrator's setup. Link the supporting guides ([GitHub](../github-setup-guide.md),
[Azure DevOps](../azure-devops-getting-started.md), [RBAC](../rbac.md),
[Secret encryption](../sops-age-guide.md), [commit signing](../commit-signing.md)) where readers
need them, and say when each capability is required or optional.

### 2. Make changes

- **Make a change through Kubernetes.** For an end user: create, update, or delete a selected
  resource; follow the change into Git; understand timing and refusal outcomes.
- **Group changes and explain why.** For an end user: collect related edits, attach intent through
  a `SaveRequest`, and verify the resulting commit or no-commit outcome. Explain author matching
  and window timing.
- **Work alongside other people on the same branch.** For collaborating teams: explain how authors
  and targets share a branch, what splits a commit window, and what happens when edits overlap.
- **Make a change through Git.** For an end user: explain how an external push reaches the cluster
  through Flux or Argo CD, what happens to pending Kubernetes edits, and how to verify the apply.
  Link back to the bi-directional setup prerequisites.

Another person's Kubernetes edit and another person's Git push can both affect the same branch,
but they exercise different parts of the system and need different worked examples.

### 3. Operate

- **Monitor health and resource usage.** For administrators: readiness, publication progress,
  latency, backlog, memory, watch count, and Git activity, each tied to a question and a next
  action.
- **Investigate a missing or unexpected change.** For administrators and end users: start from
  symptoms such as no commit, an unresolved author, a refused folder, or a delayed apply. Explain
  recovery after an outage or restart, including the limits of retained work and deletion recovery.
- **Upgrade an installation.** A stable procedure that links to the release-specific entries.

Keep routine monitoring separate from symptom-driven troubleshooting. Readers investigating an
incident should reach a diagnostic path without first learning the entire metrics vocabulary.

### 4. Understand and reference

- **Architecture.** One Kubernetes edit reaching Git, one Git edit reaching the cluster, component
  ownership, and safety and recovery boundaries. Move field inventories, OIDC setup, and
  implementation history to their guides or references. Qualify recovery guarantees at first
  mention: deletion policy matters, unpublished work is held in memory, and high availability is
  unfinished. Keep the "optional but advised" wording for Redis and explain that planned HA
  requires it.
- **Configuration.** Resource relationships, settings, defaults, and examples. Stays authoritative;
  task guides link to it.
- **Security model.** Credentials, permissions, source identity, namespace boundaries, encryption,
  and audit ingress trust.
- **Supported manifest layouts.** What the writer can edit, what it refuses, how it preserves
  existing documents, and how it places new ones.
- **API, Helm, and command reference.** Exact fields, chart values, flags, and defaults, each with
  an explicit source that is checked against the implementation.
- **Glossary.** The reader vocabulary from [definitions](../definitions.md). Maintainer naming rules
  stay in that document and link to the glossary, so each term keeps one authoritative definition.

The existing [maintainer index](../INDEX.md) stays the contributors' entry point. The existing
[user landing page](../README.md) is rewritten around the four categories.

## Guide inventory

"Exists" means a usable source exists; it still needs publication review. "Needs split" means
separating reader content from a broader document. "Missing" means no dedicated task guide exists,
even where source material does. Proposed guides are named by task, not by filename.

| Guide or source | Destination | State and action |
| --- | --- | --- |
| `docs/README.md` | Documentation landing page | Exists: rewrite around the four categories |
| Root `README.md` | Homepage | Exists: reuse the product explanation and demo |
| `alternatives.md` | Homepage: is this right for me? | Exists: link from adoption guidance |
| `quickstart.md` | Get started: capture | Exists: verify a first commit without an applier claim or push webhook |
| Team onboarding | Get started: capture | Missing: extract selection, sharing, and repository adoption from `configuration.md` and the root README; ask whether an applier deploys the folder |
| `github-setup-guide.md` | Get started: Git host | Exists: link to push-notification setup |
| `azure-devops-getting-started.md` | Get started: Git host | Exists: state the push-notification limit described below |
| `attribution-setup-guide.md` | Get started: record who | Exists: keep its outcome and eligibility opening |
| `audit-setup/cluster/readme.md` | Get started: record who | Exists: publish as the k3s example the attribution guide links |
| `bi-directional.md` | Get started: coordination overview | Needs split: keep the setup decision and common behavior; extract receiver setup into the push-notification guide and the everyday Git edit into Make changes |
| `bi-directional-flux.md`, `bi-directional-argocd.md` | Get started: applier setup | Exists: link receiver setup and verify both directions |
| `sops-age-guide.md`, `commit-signing.md` | Get started: supporting guides | Exists |
| Normal Kubernetes change | Make changes | Missing: expand the quick-start example into create, update, delete, and refusal outcomes |
| `demo/commit-window.md` | Make changes: group and explain | Exists: the worked task guide |
| `commit-messages.md` | Reference, linked from grouped changes | Exists: template reference |
| Collaboration on a shared branch | Make changes | Missing: publication semantics and multi-author examples |
| `api-first-publication.md` | Reference, linked from collaboration | Exists: shared publication contract |
| `installing-apps-as-krm.md` | Make changes | Exists: expanded example of choosing which declarations to capture |
| `interpreting-metrics.md` | Operate: monitor | Exists: shared metric reference |
| Symptom-driven troubleshooting | Operate | Missing: gather diagnostic paths from configuration and metrics |
| Stable upgrade procedure | Operate | Missing: steps and verification linking to release entries |
| `UPGRADING.md` | Operate: release migrations | Exists: keep release entries |
| `architecture.md` | Reference | Needs split: overview, setup detail, implementation rationale |
| `configuration.md` | Reference | Needs split: extract onboarding steps, keep exact settings |
| `security-model.md`, `rbac.md` | Reference, linked from setup | Exists |
| Supported manifest layouts | Reference | Missing: reader guide sourced from the support contract and the `layout/` specs (`new-file-placement-rules.md`, `contextual-namespace.md`) |
| `definitions.md` | Reference: glossary | Needs split: publish the vocabulary, keep the naming rules |
| Chart `README.md` | Reference: Helm | Exists: published from the chart |
| API and command reference | Reference | Needs split: consolidate field and flag descriptions, find uncovered surfaces |
| `INDEX.md`, `components.md`, `CONTRIBUTING.md`, `style-guide.md` | Maintainer entry point | Exists: link to repository pages |

## Push notifications as an explicit setup activity

The outcome is "react promptly to Git pushes." Webhook setup is part of the normal bi-directional
path; capture-only installations complete setup without it. A Git-side commit leaves the cluster
behind until the coordinated apply runs. Prompt notification shortens that interval and the
opportunity for further Kubernetes edits to overlap the incoming change.

The applier's own reconciliation schedule does not help: an applier claim holds the applier, and
GitOps Reverser decides when it applies. Without a webhook, a rejected push detects the remote
change at the next publication and the periodic refresh detects it within `--git-refresh-interval`
plus the reconcile tick ([bi-directional GitOps](../bi-directional.md#telling-gitops-reverser-that-somebody-else-pushed)).
Describe these as fallback paths with that latency. Do not promise that webhooks prevent all
clashes or that mirroring stops working without them.

The task guide is extracted from that section; [the GitReceiver reference](../configuration.md#gitreceiver)
stays authoritative for fields and delivery diagnostics. It covers:

1. Enable and expose the receiver listener at an HTTPS endpoint reachable by the Git host.
2. Create the token Secret and `GitReceiver`, then read `status.webhookPath`.
3. Configure a GitHub push webhook with that URL and secret. Verify current host instructions when
   writing the walkthrough; link the GitLab, Gitea, and `generic-hmac` variants from the reference.
4. Follow the documented Flux or Argo CD rules for existing applier webhooks. Name audit delivery
   and Git push notifications as separate webhook purposes.
5. Push a known change and check host delivery, `GitBranch.status.lastPushNoticeAt`, the observed
   remote commit, and the applier's result. A successful HTTP response alone does not prove apply.
6. Diagnose unreachable endpoints, rejected signatures, repository URL mismatches, and unmatched
   branches. Cover token rotation, which moves the webhook path, and missed-delivery recovery.

**Azure DevOps has no receiver type.** `GitReceiver` verifies `github`, `gitlab`, `gitea`, and
`generic-hmac` deliveries. The Azure DevOps guide states that coordination on Azure DevOps relies
on the fallback detection above, with its latency, and that prompt notification needs a relay of
the reader's own that signs deliveries as `generic-hmac`. Do not document a relay walkthrough until
one is tested.

## Attribution as a first-class optional journey

The outcome is "record who made a change." The author comes from an authenticated Kubernetes
identity, which can represent a person or a service account. Explain the operator's separate
committer identity and the explicit unresolved-author outcome.

Order the guide around these questions:

1. What author information appears in Git with attribution enabled and disabled? Open with one
   sentence describing the outcome.
2. Can this cluster deliver the required API-server audit events? Check eligibility before setup.
3. How are audit policy, delivery, ingress connectivity, certificates, source routing, and
   attribution configured? Label settings by their API-server, Helm, CRD, or controller surface.
4. How does a known test change prove that delivery and matching work end to end?
5. How can an administrator diagnose missing evidence or an unresolved author?
6. How can optional OIDC claim mapping add a friendly display name and email?

Keep the guide's existing boundary: "managed control planes that hide these settings cannot use
this webhook." Keep the configured-author installation path visible, and verify any new
provider-specific claim when writing it.

Link this guide from normal changes and grouped changes. Keep "who made the change" and "why the
change was needed" as distinct tasks. Explain the separate admission-based submitter capture used
by `SaveRequest` where it affects matching, so readers do not assume audit setup controls both.

## Guide structure

Give each task guide the same structure: goal and intended reader; prerequisites, permissions, and
optional capabilities; steps with a concrete example; expected result and how to verify it;
relevant failure cases and next actions; links to the shared explanation and exact reference.

Create a separate document when a task has its own meaningful outcome; keep closely related
variations together. Give each contract one authoritative home and link to it from the guides.

## Static website

**Homepage.** A short explanation of the problem, an API-to-Git diagram, and an example change
with its resulting commit. Two prominent actions: "Install GitOps Reverser" and "Understand how it
works." Show the supported workflows, the shared-folder decision between the two setup paths, the
attribution eligibility link, and [alternatives](../alternatives.md) before installation. Keep
source code, releases, contribution guidance, and issue reporting easy to find.

**Requirements.**

- Build from repository Markdown with one maintained source per guide. Keep relative `.md` links
  in the source so GitHub navigation works; resolve published targets to site routes and
  unpublished references to the repository revision.
- Publish only pages listed in a curated publication manifest. `spec/`, `design/`, `facts/`,
  `finished/`, `layout/`, `future/`, `review/`, `local-only/`, and `*.ignore.md` stay unpublished;
  extracted reader guides are published deliberately. Search covers the manifest only.
- Every published Markdown source is also listed in [.docs-lint-scope](../../.docs-lint-scope), so
  Vale gates it. CI checks the inclusion. The scope may also list unpublished contracts.
- Render Mermaid diagrams and existing images.
- Preserve page URLs and heading anchors; record redirects when pages move or split.
- Label which release the installation instructions and reference describe. Decide whether archived
  release versions are needed before adding a version selector.
- Provide a reproducible local build and a preview for review.

The generator, hosting, domain, and release publication policy are chosen during implementation
against these requirements and their maintenance cost.

## Implementation phases

**Phase 1: Navigation.** Rewrite `docs/README.md` under the four categories from the inventory,
with both setup paths under Get started. Link `docs/INDEX.md` as the maintainer entry point. Keep
the lifecycle folders and source citations; navigation provides the new structure without moving
files.

**Phase 2: Reading paths.**

- Capture-only installation through a first verified commit, as the shortest complete path.
- The missing onboarding, normal-change, collaboration, troubleshooting, and upgrade guides.
- The bi-directional path: applier setup, push notifications with a GitHub example, the Azure
  DevOps limit, and verification of both directions.
- The marked splits and the attribution ordering.
- Bring each source the manifest will publish under the prose gate. Measured on 2026-10-26, these
  fail Vale: `UPGRADING.md` (135 errors), `installing-apps-as-krm.md` (15), `rbac.md` (10),
  `security-model.md` (6), `api-first-publication.md` (5), `azure-devops-getting-started.md` (3),
  and `alternatives.md` (1). The other inventoried sources pass and are already gated.

**Phase 3: Website.** Select the generator and hosting, build the homepage and the curated
documentation, validate the reading paths in a preview, then configure the domain and publish.

After launch, a behavior change updates the affected task guide and shared contract in the same
change, and support questions feed back into the task paths.

## Completion criteria

- Every inventory row has a published destination or an intentional maintainer-only destination.
- The homepage links directly to installation and architecture. The documentation landing page
  presents the four categories with working task links and both setup paths under Get started.
- A CI job runs the quick start's commands against the documented release, reaches a first commit
  without an applier claim or Git push webhook, then onboards a second target on the same
  installation.
- Onboarding asks whether an applier deploys the same folder and routes that case to bi-directional
  setup. The Flux and Argo CD paths each verify a Kubernetes edit reaching Git and a Git edit
  reaching the cluster.
- The GitHub receiver walkthrough verifies host delivery, branch notification, remote observation,
  and apply outcome. The Azure DevOps guide states its fallback latency and the relay option.
- The attribution page states its outcome and eligibility before commands, keeps the
  configured-author path, and includes a known-actor verification example.
- Worked examples cover a Kubernetes edit, a Git push, and grouped changes with intent.
  Troubleshooting covers no commit, unresolved author, refused folder, and delayed apply.
- CI rejects a published source absent from `.docs-lint-scope`, an unlisted built page, a broken
  rendered link or asset, and a changed route without a redirect.
- `task lint-docs` and the site build checks pass.
