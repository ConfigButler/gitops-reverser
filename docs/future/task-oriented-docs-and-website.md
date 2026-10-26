# Task-oriented documentation and a static website

Status: proposed documentation and website plan. The navigation, page splits, and website described
here are future work.

## Goal

Give GitOps Reverser a public website that helps people understand the product, install it, and
complete common tasks. Use the same documentation sources for the repository and the website.

Organize the reader-facing navigation around outcomes. A reader should be able to find the next
step without already knowing names such as `GitTarget`, attribution, or API-first publication.
Keep architecture and configuration reference available alongside the task guides.

## The four main categories

These categories form the documentation landing page and the website's documentation navigation.
They describe reader journeys; they do not require matching physical folders in the first phase.

### 1. Get started

Offer two visible setup paths. The deciding question is: does Flux or Argo CD apply the same folder
GitOps Reverser writes? If yes, follow the bi-directional path even when the immediate goal is only
to record cluster changes. An existing deployment loop needs coordination regardless of whether
anyone plans to edit Git manually.

#### Capture cluster state in Git

- **Install GitOps Reverser.** For a cluster administrator: prerequisites, credentials, permissions,
  installation, a first captured change, verification, and cleanup. Keep the default path usable
  without audit attribution. Start from the [quick start](../quickstart.md).
- **Start capturing a team's resources.** For a platform administrator or team owner: choose the
  source cluster, namespaces, resource selection, repository, branch, and folder. Cover adopting
  an existing repository through a scratch branch, supported layouts, deletion policy, and
  cross-namespace access. Build a focused guide from [configuration](../configuration.md) and
  the repository adoption guidance in the [project overview](../../README.md).
- **Record who made a change (optional).** For an administrator configuring authenticated Git
  authorship: check cluster eligibility, configure audit delivery and attribution, and verify a
  known actor's commit. Use the existing [attribution setup guide](../attribution-setup-guide.md)
  as the home for this journey. See the dedicated scope below.

This path reaches a verified commit without an applier claim or Git push webhook. Describe it as
capturing selected cluster state: batching and restart recovery mean Git history is not a complete
record of every event. Attribution is optional in both setup paths.

#### Coordinate changes from Kubernetes and Git

- **Choose bi-directional coordination.** Use [bi-directional GitOps](../bi-directional.md) as the
  overview and entry point. Explain when the shared-folder deployment loop needs coordination,
  then build on the installation and resource-selection steps above.
- **Configure the applier.** Follow the [Flux](../bi-directional-flux.md) or
  [Argo CD](../bi-directional-argocd.md) setup guide to configure the claim and connect the target.
  Explain who controls applying the folder and what happens when the operator is unavailable.
- **React promptly to Git pushes.** For an administrator enabling edits from both Git and
  Kubernetes: configure the Git host's push webhook and a `GitReceiver`, then verify delivery and
  the resulting apply. Make this an explicit step in bi-directional setup and link it from
  "Make a change through Git." See the dedicated scope below.
- **Verify both directions.** Capture a Kubernetes edit, then push a Git edit and verify the
  coordinated apply. Link to the everyday change guides and delayed-apply troubleshooting.

#### Shared setup guidance

Installation happens at the platform level; onboarding repeats for each team or destination.
Keep those outcomes separate so a team can start using an existing installation without repeating
the administrator's setup.

Link supporting setup guides where readers need them: [GitHub](../github-setup-guide.md),
[Azure DevOps](../azure-devops-getting-started.md), [RBAC](../rbac.md),
[Secret encryption](../sops-age-guide.md), and [commit signing](../commit-signing.md).
Explain when each capability is required or optional.

### 2. Make changes

- **Make a change through Kubernetes.** For an end user: create, update, or delete a selected
  resource; follow the change into Git; understand timing and refusal outcomes. Include a simple
  worked example and links to deletion policy and supported manifest layouts.
- **Group changes and explain why.** For an end user: collect related edits, attach intent through
  a `CommitRequest`, and verify the resulting commit or no-commit outcome. Explain author matching
  and window timing. Build from the [commit-window demo](../demo/commit-window.md) and
  [commit messages](../commit-messages.md).
- **Work alongside other people on the same branch.** For collaborating teams: explain how authors
  and targets share a branch, what splits a commit window, and what happens when edits overlap.
  Link to the shared conflict and publication contract in
  [API-first publication](../api-first-publication.md).
- **Make a change through Git.** For an end user and the administrator enabling this workflow:
  explain how an external push reaches the cluster through Flux or Argo CD, what happens to pending
  Kubernetes edits, and how to verify the apply. Link back to the bi-directional setup prerequisites;
  keep the common explanation in its overview and the everyday example in this task guide.

Distinguish another person's Kubernetes edit from another person's Git push. Both can affect the
same branch, but they exercise different parts of the system and need different worked examples.
Link [installing applications as Kubernetes resources](../installing-apps-as-krm.md) as an expanded
example of choosing which declarations to capture.

### 3. Operate

- **Monitor health and resource usage.** For administrators: check readiness, publication progress,
  latency, backlog, memory, watch count, and Git activity. Connect each observation to a question
  and a next action. Use [interpreting metrics](../interpreting-metrics.md) as the shared reference.
- **Investigate a missing or unexpected change.** For administrators and end users: start from
  symptoms such as no commit, an unresolved author, a refused folder, or a delayed apply. Identify
  the relevant conditions, events, logs, and metrics. Explain recovery after an outage or restart,
  including the limits of retained work and deletion recovery.
- **Upgrade an installation.** For administrators: identify migration requirements, apply the
  upgrade, and verify the installation afterward. Keep release-specific changes in the
  [upgrade guide](../UPGRADING.md); link to them from a stable upgrade procedure.

Keep routine monitoring separate from symptom-driven troubleshooting. Readers investigating an
incident should reach a diagnostic path without first learning the entire metrics vocabulary.

### 4. Understand and reference

- **Architecture.** A concise account of the system: one Kubernetes edit reaching Git, one Git
  edit reaching the cluster, component ownership, and safety and recovery boundaries. Refactor
  [architecture](../architecture.md) around that reading path.
- **Configuration.** The resource relationships, settings, defaults, and examples. Keep
  [configuration](../configuration.md) authoritative and link to it from task guides.
- **Security model.** Credentials, permissions, source identity, namespace boundaries, encryption,
  and audit ingress trust. Use [the security model](../security-model.md) and the RBAC guide.
- **Supported manifest layouts.** What the writer can edit, what it refuses, how it preserves
  existing documents, and how it places new ones. Provide a reader-facing explanation linked to
  the [support contract](../design/support-boundary/support-contract.md) and configuration reference.
- **API, Helm, and command reference.** Exact fields, chart values, flags, and defaults. Establish
  an explicit source for each surface and check it against the implementation. Start with
  configuration and the [Helm chart reference](../../charts/gitops-reverser/README.md).
- **Glossary.** Extract the reader vocabulary from [definitions](../definitions.md) into a
  published glossary. Keep maintainer naming rules in the existing document, linking to the
  extracted definitions so each term still has one authoritative definition. Update citations
  and anchors as part of that split.

Keep [the component inventory](../components.md) and deeper implementation records accessible to
contributors through the existing [maintainer index](../INDEX.md). Rewrite the existing
[user landing page](../README.md) around the four categories.

## Guide inventory and concrete backlog

This is the initial mapping, completed as part of this plan. "Exists" means a usable source exists;
it still needs publication review. "Needs split" means extract or separate reader content from a
broader document. "Missing" means a dedicated task guide is absent, even where source material
already exists. Proposed filenames are decisions for implementation, not links to absent pages.

| Guide or source | Category or destination | State and action |
| --- | --- | --- |
| `docs/README.md` | Documentation landing page | Exists: rewrite navigation |
| Root `README.md` | Homepage | Exists: reuse product explanation and demo |
| `alternatives.md` | Homepage: is this right for me? | Exists: link from adoption guidance |
| `quickstart.md` | Get started: capture cluster state | Exists: verify first commit without an applier or push webhook |
| Team onboarding | Get started: capture cluster state | Missing: extract selection, sharing, and repository adoption; check for an existing applier |
| `github-setup-guide.md` | Get started: GitHub credentials | Exists: link to push-notification setup |
| `azure-devops-getting-started.md` | Get started: Azure DevOps credentials | Exists: retain provider-specific setup |
| `attribution-setup-guide.md` | Get started: record who | Exists: retain outcome and eligibility opening |
| React promptly to Git pushes | Get started: bi-directional setup | Needs split: extract setup from `bi-directional.md`, add a GitHub walkthrough |
| `sops-age-guide.md` | Get started: encrypt sensitive resources | Exists: supporting guide |
| `commit-signing.md` | Get started: sign commits | Exists: supporting guide |
| Normal Kubernetes change | Make changes | Missing: expand the quick-start example into create, update, delete, and refusal outcomes |
| `demo/commit-window.md` | Make changes: group changes and explain why | Exists: use as the worked task guide |
| `commit-messages.md` | Understand and reference; linked from grouped changes | Exists: keep template reference separate from the task |
| Collaboration on a shared branch | Make changes | Missing: use publication semantics and multi-author examples |
| `api-first-publication.md` | Understand and reference; linked from collaboration | Exists: shared publication contract |
| `bi-directional.md` | Get started: bi-directional overview | Needs split: retain the setup decision and common behavior, extract receiver setup and everyday Git-edit example |
| `bi-directional-flux.md` | Get started: bi-directional Flux setup | Exists: link to receiver setup and verification of both directions |
| `bi-directional-argocd.md` | Get started: bi-directional Argo CD setup | Exists: link to receiver setup and verification of both directions |
| Make a change through Git | Make changes | Needs split: extract the everyday example from bi-directional guidance and link to setup prerequisites |
| `installing-apps-as-krm.md` | Make changes: capture application intent | Exists: expanded example |
| `interpreting-metrics.md` | Operate: monitor | Exists: shared metric reference and interpretation |
| Symptom-driven troubleshooting | Operate | Missing: gather diagnostic paths from configuration and metrics |
| Stable upgrade procedure | Operate | Missing: steps and verification linking to release changes |
| `UPGRADING.md` | Operate: release-specific migrations | Exists: retain release entries |
| `architecture.md` | Understand and reference | Needs split: overview, setup detail, and implementation rationale |
| `configuration.md` | Understand and reference | Needs split: extract onboarding steps, retain exact settings |
| `security-model.md` and `rbac.md` | Understand and reference; linked from setup | Exists: trust boundaries and access |
| Supported manifest layouts | Understand and reference | Needs split: reader guide from configuration and the support contract |
| `definitions.md` | Understand and reference: glossary | Needs split: publish vocabulary, retain maintainer naming rules |
| Chart `README.md` | Understand and reference: Helm | Exists: published reference sourced from the chart |
| API and command reference | Understand and reference | Needs split: consolidate existing field and flag descriptions, identify uncovered surfaces |
| `INDEX.md` and `components.md` | Maintainer entry point | Exists: retain repository links |
| `CONTRIBUTING.md`, `style-guide.md`, and e2e debugging | Maintainer entry point | Exists: retain repository links |

## Push notifications as an explicit setup activity

The outcome is "react promptly to Git pushes." Treat webhook setup as part of the normal
bi-directional installation path; capture-only installations can complete setup without it.
A Git-side commit leaves the cluster behind until the
coordinated apply runs; prompt notification shortens that interval and the opportunity for further
Kubernetes edits to overlap the incoming change.

The applier's usual reconciliation schedule is insufficient here: an applier claim holds the
applier, and GitOps Reverser decides when it applies. A rejected push or periodic refresh still
detects remote changes when webhook delivery is unavailable. Describe these as fallback paths,
with their latency, and preserve the existing conflict semantics. Do not promise that webhooks
prevent all clashes or that mirroring stops working without them.

Extract the setup from
[telling GitOps Reverser that somebody else pushed](../bi-directional.md#telling-gitops-reverser-that-somebody-else-pushed).
Keep [the GitReceiver reference](../configuration.md#gitreceiver) authoritative for fields and
delivery diagnostics. The task guide covers:

1. Enable and expose the receiver listener at an HTTPS endpoint reachable by the Git host.
2. Create the token Secret and `GitReceiver`, then read `status.webhookPath`.
3. Configure a GitHub push webhook with that URL and secret. Verify current host instructions
   when writing the walkthrough; link other supported host variants from the reference.
4. Follow the documented Flux or Argo CD rules for existing applier webhooks. Keep audit delivery
   and Git push notifications clearly named as separate webhook purposes.
5. Push a known change and check host delivery, `GitBranch.status.lastPushNoticeAt`, the observed
   remote commit, and the applier's result. A successful HTTP response alone does not prove apply.
6. Diagnose unreachable endpoints, rejected signatures, repository URL mismatches, and unmatched
   branches. Cover token rotation, which changes the webhook path, and missed-delivery recovery.

Link this guide from Git-host setup, both applier setup guides, and delayed-apply troubleshooting.

## Attribution as a first-class optional journey

The outcome is "record who made a change." Explain that the author comes from an authenticated
Kubernetes identity, which can represent a person or a service account. Explain the operator's
separate committer identity and the explicit unresolved-author outcome.

Order the guide around these questions:

1. What author information appears in Git with attribution enabled and disabled? Open with one
   sentence describing the outcome.
2. Can this cluster deliver the required API-server audit events? Check eligibility before setup.
3. How are audit policy, delivery, ingress connectivity, certificates, source routing, and
   attribution configured? Label settings by their API-server, Helm, CRD, or controller surface.
4. How does a known test change prove that delivery and matching work end to end?
5. How can an administrator diagnose missing evidence or an unresolved author?
6. How can optional OIDC claim mapping add a friendly display name and email?

Retain the attribution setup guide's existing prerequisite: "managed control planes that hide
these settings cannot use this webhook." Its opening already states the outcome and the required
control over API-server flags and files. Preserve that precise boundary and the configured-author
installation path; verify any new provider-specific claims when writing them.

Link this guide from normal changes and grouped changes. Keep "who made the change" and "why the
change was needed" as distinct tasks. Explain the separate admission-based submitter capture used
by `CommitRequest` where it affects matching, so readers do not assume audit setup controls both.

## How the guides fit together

Give each task guide a consistent structure:

1. Goal and intended reader.
2. Prerequisites, permissions, and optional capabilities.
3. Steps with a concrete example.
4. Expected result and how to verify it.
5. Relevant failure cases and next actions.
6. Links to the shared explanation and exact reference.

Create a separate document when a task has its own meaningful outcome. Keep closely related
variations together. Give each contract one authoritative home and link to it from the guides.

Shorten the architecture overview by moving field inventories, OIDC setup instructions, and
implementation history to their appropriate guides or references. Bring manifest editing and its
support boundary forward. Qualify recovery guarantees at first mention: deletion policy matters,
unpublished work is held in memory, and high availability is unfinished. Preserve the architecture's
"optional but advised" wording for Redis and explain that planned HA requires it. Distinguish
current single-replica behavior from that future requirement.

## Static website

### Homepage and entry paths

Create a product homepage with a short explanation of the problem GitOps Reverser solves, an
API-to-Git diagram, and an example change with its resulting commit. Give visitors two prominent
actions: "Install GitOps Reverser" and "Understand how it works."

Show the supported workflows and important adoption boundaries before installation. Include a
visible link to attribution eligibility. Present "Capture cluster state in Git" and "Coordinate
changes from Kubernetes and Git" as distinct setup paths under Get started, with the shared-folder
decision visible before readers choose. Keep the four top-level categories.
Use [alternatives](../alternatives.md) for the "is this right for me?" decision. Reuse existing demos
and diagrams where they explain the behavior clearly.

The documentation landing page exposes the four categories above. Add search, page-local contents,
and related next steps. Keep source code, releases, contribution guidance, and issue reporting
easy to find without placing maintainer plans in the main user navigation.

### Content and publishing requirements

- Build static pages from repository Markdown with one maintained source per guide. Keep the
  source readable on GitHub and avoid a separately maintained website copy of the documentation.
- Define a curated publication manifest. Exclude `spec/`, `design/`, `facts/`, `finished/`,
  `layout/`, `future/`, `review/`, `local-only/`, and `*.ignore.md` from publication and search by
  default. Publish extracted reader guides deliberately; link remaining maintainer references to
  their repository pages. Only explicitly listed pages enter the build.
- Require every published Markdown source to appear in [.docs-lint-scope](../../.docs-lint-scope).
  Check this inclusion automatically in CI; the prose scope may also contain unpublished contracts.
  Add pages to the gate as they are cleaned, before publishing them.
- Require Mermaid rendering and existing image support. Preserve relative `.md` links in source
  so GitHub navigation works; resolve published targets to site routes and unpublished references
  to the corresponding repository revision.
- Preserve stable page URLs and heading anchors. Record redirects when pages move or split, and
  retain valid repository links and source-code citations.
- Generate search only from the publication manifest and check the installation path on mobile
  and with keyboard navigation.
- Label which release the installation instructions and reference describe. Keep development docs
  identifiable; decide whether archived release versions are needed before adding a version selector.
- Provide a reproducible local build, a preview for review, and automated checks for generated
  links, assets, and site output before publication.

Choose the static site generator, hosting service, domain, and release publication policy during
implementation. Evaluate them against these requirements and maintenance cost. The content plan
does not depend on a particular framework or hosting provider.

## Implementation phases

### Phase 1: Apply the inventory to navigation

- Use the inventory above to rewrite `docs/README.md` under the four categories, with both setup
  paths under Get started and `bi-directional.md` as the coordination entry point.
- Retain `docs/INDEX.md` as the maintainer entry point and link it explicitly.
- Give every inventory row a destination or a named extraction task; avoid empty public pages.
- Preserve the existing maintainer lifecycle folders and source citations. Decide physical moves
  only when they improve maintenance; navigation can provide the new structure immediately.

### Phase 2: Complete the essential reading paths

- Establish capture-only installation through first verified commit as the shortest complete path.
- Fill the missing onboarding, normal-change, collaboration, troubleshooting, and stable upgrade
  guides identified above.
- Complete the bi-directional path: applier setup, push notifications with a GitHub example, and
  verification of both directions. Link everyday Git edits back to these prerequisites.
- Complete the marked splits and attribution ordering, checking examples against supported behavior.

### Phase 3: Build the website

- Select the generator and hosting, then implement the homepage and curated documentation build.
- Add navigation, search, diagram rendering, link resolution, release labeling, and previews.
- Validate the essential reading paths in a rendered preview, including mobile and keyboard use.
- Configure the chosen domain and publication workflow, then publish the reviewed site.

### Phase 4: Maintain it with the product

- Update the affected task guide and shared contract when behavior changes.
- Validate installation examples against the release they document.
- Check source and rendered links, preserve redirects, and keep the publication list explicit.
- Use installation feedback and support questions to improve the task paths.

## Completion criteria

- Every inventory row has a published destination or an intentional maintainer-only destination.
- The homepage links directly to installation and architecture; the documentation landing page
  presents all four categories with working task links and both setup paths under Get started.
- A recorded walkthrough against the documented release reaches a first commit from installation,
  without an applier claim or Git push webhook, then onboards a second target using the existing
  installation. Each guide names the expected observable result.
- Onboarding asks whether an applier deploys the same folder and routes that case to bi-directional
  setup. The Flux and Argo CD paths each verify a Kubernetes edit reaching Git and a Git edit
  reaching the cluster. The everyday Git-edit guide links back to those prerequisites.
- The attribution page states its outcome and eligibility before commands, retains the
  configured-author path, and includes a known-actor verification example.
- The GitHub receiver walkthrough verifies host delivery, branch notification, remote observation,
  and apply outcome. Both applier guides link to it and document fallback detection.
- Worked examples cover a Kubernetes edit, a Git push, and grouped changes with intent.
  Troubleshooting includes no commit, unresolved author, refused folder, and delayed apply.
- CI rejects a published Markdown source absent from `.docs-lint-scope`, an unlisted built page,
  or a broken rendered link or asset. Excluded maintainer pages do not enter the search index.
- The preview renders the existing Mermaid diagrams and preserves headings and relative links;
  changed routes have tested redirects. Installation works on mobile and with keyboard navigation.
- `task lint-docs`, Vale on touched prose, and the site build checks pass. Website configuration
  changes also pass the applicable repository validation gates.

## Change isolation

The plan was drafted on `docs-metrics-architecture-inventory`. Carry it and the documentation work
in a separate documentation PR from the metrics changes. Preserve unrelated work and avoid a
branch-wide commit or file move as part of the navigation rewrite.
