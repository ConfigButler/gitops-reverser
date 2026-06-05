Here is a strong prompt you can paste into your agent.

You are working in the ConfigButler codebase.

I want you to write a design document for a change around detecting duplicate Git destinations / Git upstreams.

Context:
ConfigButler has a concept called GitDestination. A GitDestination represents a Git upstream repository that ConfigButler can write to, read from, or otherwise use as a configured target. Users may configure the same repository multiple times through different URLs, for example over SSH and HTTPS:

- git@github.com:ConfigButler/gitops-reverser.git
- ssh://git@github.com/ConfigButler/gitops-reverser.git
- https://github.com/ConfigButler/gitops-reverser
- https://github.com/ConfigButler/gitops-reverser.git

These can all refer to the same real repository. I want ConfigButler to detect these cases reliably where possible.

Important insight:
Do not assume that Git itself gives us a universal repository identity. It does not. A Git remote is basically an endpoint exposing objects and refs. There is no global built-in repository UUID that works across arbitrary Git servers.

Also, do not treat the SHA of `main` or the default branch as a unique repository identifier. That only tells us something about current content. Different repositories can have the same default branch SHA, especially forks, mirrors, templates, empty repos, or recently copied repositories.

The design should treat repository identity as a confidence-based classification problem, not a perfect equality check.

Please inspect the existing codebase before writing the design. Look for:
- The existing GitDestination CRD/schema.
- Existing controllers/reconcilers touching GitDestination.
- Existing status/conditions conventions.
- Existing Git provider abstractions, Git authentication handling, URL parsing, cloning, ls-remote usage, or related packages.
- Existing documentation style/design docs, if any.

The task is to write a design document only. Do not implement the change yet.

The design document should be written in Markdown and should be suitable for inclusion in the repository, for example under `docs/design/` or a similar location if one exists.

The document should cover the following.

# 1. Problem statement

Explain the problem clearly:

Users may configure the same Git repository multiple times using different remote URLs, protocols, credentials, ports, or provider-specific aliases.

Examples:
- SSH vs HTTPS for the same GitHub repo.
- URLs with or without `.git`.
- `git@host:owner/repo.git` vs `ssh://git@host/owner/repo.git`.
- Canonical provider URLs vs vanity domains or redirects.
- Self-hosted Git servers with non-standard ports.
- Mirrors and forks that expose the same refs but are not the same repository identity.

Explain why naive approaches are insufficient:
- String equality of URLs misses common aliases.
- DNS resolution is not enough because of CNAMEs, virtual hosting, redirects, SSH config, load balancers, and provider-specific routing.
- Default branch SHA is only a content signal, not identity.
- Full refs equality is stronger than default branch SHA, but still represents “same exposed content,” not necessarily “same repository.”
- Provider APIs may expose authoritative IDs, but only for recognized providers and when credentials allow access.

# 2. Goals

The design should aim to:
- Detect high-confidence duplicate GitDestinations.
- Detect possible duplicates or mirrors without making unsafe claims.
- Support both SSH and HTTPS remotes.
- Work across common providers such as GitHub, GitLab, Gitea, and possibly Bitbucket where feasible.
- Work reasonably for arbitrary/self-hosted Git servers.
- Avoid false-positive hard failures where two distinct repositories merely share content.
- Surface duplicate information clearly in status.
- Keep the user in control for ambiguous cases.
- Make the implementation incremental.

# 3. Non-goals

Explicitly state non-goals:
- We are not trying to prove global Git repository identity for arbitrary Git remotes.
- We are not relying solely on DNS canonicalization.
- We are not rejecting GitDestinations purely because the default branch SHA matches.
- We are not requiring every provider to have a resolver on day one.
- We are not trying to understand all possible SSH client config aliases from the user’s local machine.
- We are not cloning full repositories just for identity detection unless the existing system already does so for another reason.

# 4. Identity model

Design a confidence-based identity model.

Suggested levels:

- `Authoritative`
  - Same provider-native repository/project ID.
  - Example: GitHub repository ID/node ID, GitLab project ID, Gitea repository ID.
  - This is the strongest signal.

- `Strong`
  - Same normalized canonical remote URL under provider-specific normalization rules.
  - Example: GitHub SSH and HTTPS URLs normalize to the same host/owner/repo key.
  - This is strong but less authoritative than provider ID.

- `Medium`
  - Same complete remote refs fingerprint.
  - Computed from `git ls-remote` output over advertised refs.
  - Indicates the remotes currently expose the same content/refs.
  - Could mean same repo, mirror, fork, or copied repo.

- `Weak`
  - Same default branch name and default branch SHA.
  - Useful as a clue only.
  - Should not produce a hard duplicate classification by itself.

- `None` / `Unknown`
  - Insufficient data, inaccessible remote, unsupported provider, authentication failure, etc.

The design should be careful with terms:
- “same repository” should only be used for authoritative or very high-confidence cases.
- “same content” or “possible duplicate” should be used for medium/weak signals.
- Avoid claiming certainty when the system only has a fingerprint.

# 5. Suggested status shape

Propose a status structure for GitDestination or a related status object.

Possible shape:

```yaml
status:
  identity:
    observedGeneration: 3
    provider: github
    providerRepositoryId: "123456789"
    canonicalRemoteUrl: "https://github.com/ConfigButler/gitops-reverser.git"
    normalizedRemoteKey: "github.com/configbutler/gitops-reverser"
    defaultBranch: main
    defaultBranchSha: "abc123..."
    refsFingerprint: "sha256:..."
    confidence: Authoritative
    lastResolvedAt: "2026-06-05T10:00:00Z"

  duplicateDetection:
    duplicates:
      - name: github-ssh
        namespace: configbutler-system
        reason: SameProviderRepositoryId
        confidence: Authoritative
    possibleDuplicates:
      - name: mirror-over-https
        namespace: configbutler-system
        reason: SameRefsFingerprint
        confidence: Medium

This is only a suggested shape. Please inspect existing CRD/status conventions and propose a shape that fits the codebase.

Also consider Kubernetes conditions, for example:

IdentityResolved
DuplicateDetected
PossibleDuplicateDetected
IdentityResolutionFailed

The status should be machine-readable and human-readable.

6. URL normalization

Design URL parsing and normalization.

Support at least these forms:

git@github.com:owner/repo.git
ssh://git@github.com/owner/repo.git
ssh://git@github.com:22/owner/repo.git
https://github.com/owner/repo
https://github.com/owner/repo.git

Normalization should likely include:

Lower-casing known provider hostnames.
Removing default ports where safe, for example SSH 22 and HTTPS 443.
Removing trailing .git where provider semantics allow it.
Normalizing SCP-like SSH syntax into URI-like components.
Preserving non-default ports.
Preserving path case for unknown/self-hosted providers unless a provider-specific rule says otherwise.
Avoiding unsafe assumptions for arbitrary Git servers.

Provider-specific normalization may be needed:

GitHub owner/repo matching can be normalized more aggressively.
GitLab/Gitea self-hosted instances may need more conservative rules.
Unknown Git servers should use conservative normalization.

Please discuss edge cases:

Case sensitivity.
Ports.
Nested GitLab paths/groups.
URL redirects.
Vanity domains.
SSH aliases.
Credential/userinfo in HTTPS URLs.
Different usernames in SSH URLs.
Same host/path but different auth scopes.
7. Provider-native identity resolvers

Design an interface for provider identity resolution.

Possible interface concept:

type RepositoryIdentityResolver interface {
    Supports(remote ParsedRemote) bool
    Resolve(ctx context.Context, remote ParsedRemote, auth GitAuth) (*ResolvedRepositoryIdentity, error)
}

The exact interface should fit the existing codebase.

Provider resolvers should attempt to return:

Provider name.
Provider repository/project ID.
Canonical clone URL if available.
Default branch if available.
Visibility/permissions if useful.
Provider-specific metadata needed for stable identity.

Start with the providers that are realistic for the codebase. Likely:

GitHub
GitLab
Gitea

For each provider, discuss whether the repository ID is authoritative and how it can be fetched:

Through provider API if credentials are available.
Possibly through unauthenticated API for public repositories.
Fallback to URL normalization and ls-remote when API access is unavailable.

Do not overpromise. The design should explicitly say that provider identity resolution may be unavailable due to missing credentials, unsupported provider, permissions, or network errors.

8. Git remote fingerprinting

Design a fallback fingerprint based on git ls-remote.

Possible data to collect:

Symbolic HEAD, via git ls-remote --symref <url> HEAD.
Default branch name.
Default branch SHA.
Advertised refs, via git ls-remote <url>.
Branch refs.
Tag refs.
Optionally whether peeled tags are included and how they are normalized.

Possible fingerprint:

refsFingerprint = sha256(sorted(refname + "\x00" + sha + "\n"))

The design should specify:

Which refs are included.
Whether to include tags.
How to handle peeled annotated tags like refs/tags/v1.0^{}.
How to handle hidden refs.
How to handle authorization differences where different credentials expose different refs.
How to handle empty repositories.
How to handle remote failures.
How often to refresh the fingerprint.
Whether to debounce/retry.
Whether the fingerprint should be used for status only or also for duplicate detection.

Important:
A refs fingerprint is not repository identity. It means “these remotes currently expose the same refs.” Treat this as a medium-confidence possible duplicate/mirror signal.

9. Duplicate classification algorithm

Propose an algorithm along these lines:

Parse and normalize the configured remote URL.
Build a conservative normalized remote key.
Try provider-specific identity resolution.
Run git ls-remote to determine default branch and refs fingerprint, if credentials and network allow.
Store the resolved identity data in status.
Compare this GitDestination with other GitDestinations in the same relevant scope.
Classify matches:
Same provider and same provider repo ID => duplicate, authoritative.
Same normalized remote key => duplicate/probable duplicate, strong.
Same full refs fingerprint => possible duplicate/mirror, medium.
Same default branch SHA only => possible related repository, weak; probably do not report prominently unless useful.
Set conditions/status accordingly.
Do not block reconciliation for weak/medium matches unless the user explicitly enables a stricter policy.

Please define the scope of comparison:

Same namespace?
Cluster-wide?
Same tenant/workspace if ConfigButler has such a concept?
Same GitProvider?
Same credentials?
Same GitTarget?

Pick the safest default based on the codebase.

10. User-facing behavior

Design how users should experience this.

Important:

High-confidence duplicates may be surfaced as a warning or condition.
Medium/weak matches should be advisory, not fatal.
The user should be able to intentionally configure aliases/mirrors without fighting the controller.
There may need to be an explicit override or grouping field.

Consider a field like:

spec:
  identity:
    allowDuplicateOf: some-other-destination

or:

spec:
  identity:
    aliasGroup: configbutler-main

or:

spec:
  duplicatePolicy: WarnOnly | RejectAuthoritativeDuplicates | Ignore

Do not introduce this unless it fits the project style. Discuss options and recommend one.

Possible default:

Always warn on authoritative duplicates.
Do not reject by default.
Allow future policy to reject authoritative duplicates through validation/admission or controller policy.
Never reject on default-branch SHA alone.
11. API and CRD changes

Propose concrete CRD/schema changes.

Include:

New status fields.
New conditions.
Optional spec fields, if needed.
Backward compatibility implications.
Whether conversion webhooks are needed.
Whether existing GitDestinations need migration.
Whether existing status fields can be reused.

Keep the spec minimal. Prefer status-first unless there is a strong reason for user-configurable behavior.

12. Security considerations

Discuss:

Do not leak credentials in status.
Strip username/password/token from URLs before storing canonical/normalized URLs.
Be careful with SSH usernames.
Provider APIs may reveal private repo metadata; only store safe fields.
Refs may reveal branch names; status may expose branch names to users who can read the CR.
Different credentials may see different refs, so identity can be auth-context-dependent.
Avoid making network calls from admission webhooks if that would make writes slow or fragile.
Prefer controller reconciliation for remote identity resolution.
13. Reliability and performance

Discuss:

git ls-remote is network-bound and can fail.
Provider APIs can rate-limit.
DNS/HTTP/SSH can be flaky.
Identity resolution should be retried with backoff.
Results should be cached in status.
Reconciliation should not hammer Git providers.
The controller should use timeouts.
Duplicate comparison should be efficient, possibly using indexes if controller-runtime supports it in this codebase.
Fingerprints should only be refreshed when relevant inputs change, or on a sensible interval.
14. Failure modes

Include a section with failure modes:

Remote unavailable.
Authentication failed.
Provider API unavailable.
Provider API says repo not found but git ls-remote works.
URL normalization fails.
Empty repo.
Default branch missing.
Different credentials expose different refs.
Same repo reachable through a vanity domain.
Same content in two different repos.
Mirror intentionally configured.
Fork initially identical to upstream.
Repo transferred/renamed.
GitHub/GitLab redirects.
Self-hosted provider with unusual path semantics.

For each, describe expected behavior.

15. Testing plan

Design tests.

Unit tests:

URL parser and normalizer.
SCP-style SSH parsing.
HTTPS parsing.
.git suffix behavior.
ports.
case sensitivity.
provider-specific path normalization.
refs fingerprint calculation.
duplicate classification.

Integration tests:

Fake Git server or local bare repos.
Same repo exposed via multiple URLs if feasible.
Two repos with identical refs.
Fork-like repo with same default branch SHA.
Repos that diverge after initial fingerprint.
Auth failure.
Empty repo.

Controller tests:

Status updates.
Conditions.
Duplicate detection across multiple GitDestinations.
Reconciliation retry behavior.
No hard failure for weak/medium matches by default.

E2E tests:

Use existing e2e framework if present.
Prefer local/self-contained Git server such as Gitea if the project already uses it.
Test SSH and HTTPS remotes if possible.
16. Rollout plan

Propose an incremental rollout:

Add URL parser/normalizer.
Add status fields for normalized key and basic remote info.
Add ls-remote based fingerprint.
Add duplicate detection based on normalized key and fingerprint.
Add provider resolvers for one provider first, likely GitHub or Gitea depending on existing e2e setup.
Add policy/override fields only after real behavior is understood.
17. Alternatives considered

Discuss alternatives and why they are insufficient:

Use only URL string equality.
Use only normalized URL.
Use only default branch SHA.
Use only full refs fingerprint.
Always clone the repo and inspect object database.
Rely on DNS canonicalization.
Rely only on provider APIs.
Hard reject all apparent duplicates.
18. Recommended decision

End with a concrete recommendation.

The recommendation should likely be:

Implement a status-first identity system.
Use provider-native repository IDs as authoritative where available.
Use conservative URL normalization as a strong signal.
Use git ls-remote refs fingerprint as a medium-confidence “same exposed content” signal.
Use default branch SHA only as a weak diagnostic signal.
Do not reject duplicates by default.
Surface authoritative duplicates and possible duplicates clearly in status/conditions.
Keep room for future policy enforcement.
19. Open questions

List open questions that need project-owner input, such as:

Should duplicate detection be namespace-scoped or cluster-scoped?
Should authoritative duplicates eventually be rejected?
Should mirrors be explicitly modelled?
Which providers should be supported first?
Should identity resolution be tied to GitProvider credentials?
Should users be able to set a manual identity/alias group?
How much remote metadata is acceptable to expose in status?

Output requirements:

Produce a complete Markdown design document.
Be specific and concrete.
Do not invent existing code details. If something is unknown, say what needs to be inspected or verified.
Prefer a design that can be implemented incrementally.
Include YAML examples where useful.
Include pseudocode for the classification algorithm.
Use the terminology GitDestination unless the codebase uses a different exact name.
Keep the design honest: arbitrary Git repository identity cannot be solved perfectly.

I’d keep the prompt this opinionated. Otherwise an agent will often drift toward the tempting-but-wrong “compare URLs 