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
  Kubernetes edits, and how to verify the apply. Keep the common explanation in
  [bi-directional GitOps](../bi-directional.md), with separate
  [Flux](../bi-directional-flux.md) and [Argo CD](../bi-directional-argocd.md) setup paths.

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

Keep [the component inventory](../components.md) and deeper implementation records accessible to
contributors through a separate maintainer entry point. The four categories remain the primary
navigation for people installing and using the product.

## Attribution as a first-class optional journey

The outcome is "record who made a change." Explain that the author comes from an authenticated
Kubernetes identity, which can represent a person or a service account. Explain the operator's
separate committer identity and the explicit unresolved-author outcome.

Order the guide around these questions:

1. Can this cluster deliver the required API-server audit events? Put the eligibility check first.
2. What author information appears in Git with attribution enabled and disabled?
3. How are audit policy, delivery, ingress connectivity, certificates, source routing, and
   attribution configured? Label settings by their API-server, Helm, CRD, or controller surface.
4. How does a known test change prove that delivery and matching work end to end?
5. How can an administrator diagnose missing evidence or an unresolved author?
6. How can optional OIDC claim mapping add a friendly display name and email?

State the managed-cluster boundary precisely: a managed cluster that does not expose the required
audit configuration cannot use this attribution path. Verify provider-specific claims when writing
the guide. Avoid a blanket claim about all managed clusters or a claim that this is their only
feature restriction. Keep the configured-author installation path visible when attribution is
unavailable.

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
unpublished work is held in memory, and high availability is unfinished. Explain Redis as optional
and distinguish current behavior from future designs.

## Static website

### Homepage and entry paths

Create a product homepage with a short explanation of the problem GitOps Reverser solves, an
API-to-Git diagram, and an example change with its resulting commit. Give visitors two prominent
actions: "Install GitOps Reverser" and "Understand how it works."

Show the supported workflows and important adoption boundaries before installation. Include a
visible link to attribution eligibility and a path for people who already have Flux or Argo CD.
Reuse existing demos and diagrams where they explain the behavior clearly.

The documentation landing page exposes the four categories above. Add search, page-local contents,
and related next steps. Keep source code, releases, contribution guidance, and issue reporting
easy to find without placing maintainer plans in the main user navigation.

### Content and publishing requirements

- Build static pages from repository Markdown with one maintained source per guide. Keep the
  source readable on GitHub and avoid a separately maintained website copy of the documentation.
- Define a curated publication manifest or navigation list. Exclude local-only notes, reviews,
  historical investigations, and future plans from public documentation search and main navigation.
  Link intentional maintainer references to their repository pages when they are not published.
- Support existing diagrams, images, code blocks, tables, and relative links. Resolve documentation
  links to website pages and implementation links to the corresponding repository revision.
- Preserve stable page URLs and heading anchors. Record redirects when pages move or split, and
  retain valid repository links and source-code citations.
- Provide readable mobile layouts, keyboard navigation, accessible contrast, diagram descriptions,
  and copyable commands. Generate the search index from the curated published content.
- Label which release the installation instructions and reference describe. Keep development docs
  identifiable; decide whether archived release versions are needed before adding a version selector.
- Provide a reproducible local build, a preview for review, and automated checks for generated
  links, assets, and site output before publication. Document deployment and rollback.

Choose the static site generator, hosting service, domain, and release publication policy during
implementation. Evaluate them against these requirements and maintenance cost. The content plan
does not depend on a particular framework or hosting provider.

## Implementation phases

### Phase 1: Map the reader journeys

- Inventory the current guides against the four categories and assign an authoritative home to
  each task and shared contract.
- Rework the documentation index around the categories using existing files first.
- Identify missing onboarding, collaboration, and troubleshooting material. Record concrete gaps
  rather than creating placeholder pages for every navigation item.
- Preserve the existing maintainer lifecycle folders and source citations. Decide physical moves
  only when they improve maintenance; navigation can provide the new structure immediately.

### Phase 2: Complete the essential reading paths

- Establish installation through first verified commit as the shortest complete path.
- Add team onboarding and the eligibility-first attribution journey.
- Finish normal edits, grouped edits with intent, collaboration, and Git-side changes.
- Refactor the architecture overview and add symptom-driven troubleshooting.
- Reuse reference material and check examples against supported behavior.

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

- A newcomer can understand the product and reach installation from the homepage.
- An administrator can install it and verify the first commit using the documented path.
- A team can onboard resources without repeating platform installation.
- Attribution eligibility is clear before optional setup begins.
- Users can find separate paths for Kubernetes changes, Git changes, and grouped changes with intent.
- Administrators can find monitoring and troubleshooting from the reported symptom or question.
- The architecture overview explains ownership, manifest support, and recovery boundaries without
  requiring a full configuration-reference read first.
- Repository and website documentation share their source; published links and diagrams work.
- Future proposals and historical records are clearly separate from supported product guidance.
- Documentation changes pass `task lint-docs` and Vale on touched prose. Website implementation
  also passes the applicable repository validation gates and the new static-site build checks.
