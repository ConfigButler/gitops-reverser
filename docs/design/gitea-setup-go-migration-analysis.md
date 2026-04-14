# Feasibility Analysis: Migrating Gitea E2E Setup Scripts to Go

## Purpose

Assess whether the shell-based Gitea e2e bootstrap — primarily
[`hack/e2e/gitea-run-setup.sh`](/workspaces/gitops-reverser/hack/e2e/gitea-run-setup.sh) (~500 LOC)
and [`hack/e2e/gitea-bootstrap.sh`](/workspaces/gitops-reverser/hack/e2e/gitea-bootstrap.sh) (~90 LOC)
— should be ported to Go, given that the typed Gitea API helpers added in
[follow-up-plan-3.md](./follow-up-plan-3.md) already speak the same API natively.

This document is a proposal, not a commitment. It is intended to help decide
whether to commit to a bigger port or stop at a narrower seam.

## Current Shape

Today the e2e harness is split across three layers:

| Layer | Language | Responsibility |
|---|---|---|
| `BeforeSuite` + per-file `SetupRepo(...)` | Go | orchestration, artifact consumption, assertions |
| `task e2e-gitea-run-setup` + `e2e-gitea-bootstrap-shared` | Task | glue, env var plumbing, dependency ordering |
| `hack/e2e/*.sh` | Bash + `curl` + `jq` + `kubectl` + `ssh-keygen` | actual Gitea API calls and artifact files |

`gitea-run-setup.sh` alone does all of the following:

1. Wait for Gitea API readiness.
2. Create (or reuse) a run-scoped admin access token.
3. Generate an RSA 4096 SSH keypair for clone/push auth.
4. Register that public key in Gitea (resetting existing keys first — a known parallel hazard).
5. Create the test repo in `testorg` (201 or 409).
6. Probe the Flux `Receiver` and, if present, create/update a Gitea repo webhook.
7. Generate a `known_hosts` file via a temporary `kubectl port-forward` + `ssh-keyscan`.
8. Build a namespace-free `secrets.yaml` that materializes the HTTP, SSH, and
   invalid credential Secrets — with optional `reflector.v1.k8s.emberstack.com`
   annotations for the demo flow.
9. Clone the repo into `.stamps/repos/<repo>/` and wire git user config.
10. Emit a set of stamp files (`active-repo.txt`, `checkout-path.txt`,
    `token.txt`, `repo.ready`, `checkout.ready`, `receiver-webhook-url.txt`,
    `receiver-webhook-id.txt`).

The Go side already implements, with typed helpers:

- token-authenticated Gitea HTTP client ([gitea_api_test.go](/workspaces/gitops-reverser/test/e2e/gitea_api_test.go))
- idempotent public key registration
- commit verification lookup
- user/email management

Stamp-file reading already happens in Go in
[suite_repo_test.go](/workspaces/gitops-reverser/test/e2e/suite_repo_test.go).

## What a Port Would Look Like

The cleanest shape is a single Go package that owns Gitea lifecycle for e2e:

```
test/e2e/gitea/
  client.go           // HTTP client, admin auth, retry, body-preserving errors
  tokens.go           // create_token, idempotent reuse from token.txt
  repos.go            // ensure_repo, clone, update remotes
  keys.go             // SSH keypair generation, upload, reset semantics
  users.go            // EnsureAdminUserPrimaryEmail, EnsureUserEmail
  signing.go          // signing key registration, commit verification
  webhook.go          // Flux receiver probe + webhook create/update
  knownhosts.go       // temporary port-forward + ssh-keyscan
  secrets.go          // build the namespace-free secrets.yaml manifest
  stamps.go           // read/write stamp files under .stamps/cluster/.../git-*/
  setup.go            // the SetupRepo orchestrator (what the script does today)
```

`SetupRepo(...)` in `test/e2e/suite_repo_test.go` would stop invoking
`task e2e-gitea-run-setup` and instead call into this package directly. The
`test/e2e/gitea_api_test.go` helpers would fold into it, removing the duplicate
auth and URL plumbing.

A thin `cmd/e2e-gitea-setup/main.go` wrapper can keep `task e2e-gitea-run-setup`
working for debugging and direct Task runs (`go run ./cmd/e2e-gitea-setup
--namespace=... --repo=...`).

## Pros

### 1. One HTTP client, one API contract

Today, auth, retry, error handling, and URL construction are duplicated across
shell functions, `jq` filters, and Go helpers. A single Go client collapses
that surface and makes it trivial to:

- add structured logging
- reuse connections
- add per-call timeouts and retry
- surface full response bodies in errors (already started in
  [gitea_api_test.go](/workspaces/gitops-reverser/test/e2e/gitea_api_test.go))

### 2. Typed payloads instead of `jq -r '.sha1 // ""'`

Current failure modes:

- `jq` succeeds on unexpected payloads, returning empty strings silently
- script picks wrong field when Gitea's response shape drifts across versions
- `curl -fsS` swallows bodies on non-2xx responses, losing debuggability

Typed Go structs (`giteaPublicKey`, `giteaCommitVerification`, `giteaUser`) make
these failures loud and self-documenting.

### 3. Real error propagation and test-friendly failures

The script uses `|| true` in many places to prevent pipefail from exiting, which
means **legitimate Gitea errors are silently tolerated**. For example, the SSH
key registration treats 422 as a warning, not a failure; the repo webhook probe
falls back silently; `known_hosts` generation is best-effort with no signal.

In Go this becomes explicit: every branch returns an error, callers decide
whether it is fatal.

### 4. Concurrency is reachable

The medium-term goal of `ginkgo -p` parallel execution today runs into the
shell script's shared-state hazards:

- `configure_ssh_key_in_gitea()` resets *all* of the admin user's keys
- `ensure_repo_webhook()` deletes every hook that matches the Flux URL base
- all file writes compete for the same namespace stamp path naming

A Go port makes it natural to scope these operations per-repo, use deploy keys
instead of shared user keys, and protect mutable state with a mutex when
parallel tests must share it.

### 5. IDE-visible debugging

The script is the part of the harness that's hardest to step through. A Go
port runs under Delve, shows up in the VS Code Testing pane, and can be unit
tested with an `httptest.Server` simulating Gitea.

### 6. Dependency reduction on the dev box

Removes hard dependencies on `jq`, `curl`, `ssh-keygen`, and `ssh-keyscan`
being on `$PATH`. Go already depends on `go-git` and `crypto/ssh`, which cover
the SSH keypair + known-hosts story in-process.

### 7. Reuse of production code

`internal/git/signing.go` already owns SSH keypair generation for the
controller. A Go port lets the e2e harness reuse the same entry point
(`GenerateSSHSigningKeyPair`) so the transport keypair generation path matches
what real users hit.

### 8. Natural path to a cleaner test namespace model

A Go port makes it easy to move from "single shared admin user" to
"per-file Gitea user with owned deploy keys and signing keys", unblocking the
parallel execution workstream called out as explicitly deferred in
[follow-up-plan-3.md](./follow-up-plan-3.md).

## Cons

### 1. Ownership boundary shifts

Today, anyone comfortable with bash can read `gitea-run-setup.sh` top-to-bottom
and understand the full setup. After the port, the same reader has to step
through Go code and follow method dispatch, which is a meaningfully higher bar
for ops-leaning contributors.

### 2. The script is still the canonical "what happens manually"

Several lines of the script double as executable documentation of the exact
curl calls an engineer would run while debugging a broken Gitea setup. A Go
port obscures that — unless we keep request logging loud.

### 3. Real port work is not just Gitea API

Non-trivial pieces that are easy in shell and annoying in Go:

- the `kubectl port-forward` + `ssh-keyscan` dance for `known_hosts`
- the `kubectl create secret ... --dry-run=client -o yaml` + `kubectl annotate
  --local -f -` pipe chain that produces `secrets.yaml` — Go either reimplements
  the manifest serialization (trivial with `sigs.k8s.io/yaml`) or still shells
  out to `kubectl`
- reading Flux receiver token via `kubectl -o jsonpath=.. | base64 -d`

These pieces can still shell out, but then the "all Go, no shell" narrative
becomes "mostly Go, some shell" and some benefits shrink.

### 4. Direct Task usability regresses unless we keep a CLI

`task e2e-gitea-run-setup` today is a one-liner anyone can run to rebuild repo
state outside of Go tests. If the port deletes the script, we need a
`cmd/e2e-gitea-setup` binary to preserve that ergonomic. That is easy to add
but is real work.

### 5. CI and local dev both depend on these scripts already working

The scripts are battle-tested against the specific Gitea version (1.25.4) and
the k3d layout we actually run. A port risks regressing in ways that are
subtle — timing, ordering, error-tolerance branches — unless we migrate in
small, verifiable steps rather than a big-bang rewrite.

### 6. Duplication during the transition

If we port incrementally, Go and shell will temporarily both do the same
things. That duplication itself is a source of drift and fixes landing in only
one side.

### 7. Real cost, thin benefit for some paths

Some functions (e.g. `ensure_repo_webhook`, `generate_known_hosts`) are only
needed by specific flows. Porting them just to collapse languages is pure
refactor without new capability. The smallest worthwhile scope is the core
run-scoped setup, not all of it.

## Risks to Watch

- **Parallel test isolation**: any Go port must avoid re-introducing the
  shared-state patterns (`configure_ssh_key_in_gitea()`-style resets, bulk
  webhook deletes) that the shell version has.
- **Token caching semantics**: the script's idempotent token reuse is subtle;
  blindly porting it can silently hand tests a stale token.
- **Stamp-file compatibility**: other tooling reads `.stamps/cluster/.../git-*/`
  directly; the Go port must keep writing the exact same filenames and
  formats unless we migrate readers at the same time.
- **Gitea version drift**: the script has accumulated quiet compatibility
  branches (201 vs 409 vs 422 on create, `.sha1 // ""` defaults). A typed
  client must preserve those without becoming overly strict.

## Recommended Scope If We Do This

Prefer a narrow, high-leverage seam rather than a full rewrite:

1. **Extract a reusable Go `gitea` client package** with auth, retry, and
   typed request/response models. (Moves
   [gitea_api_test.go](/workspaces/gitops-reverser/test/e2e/gitea_api_test.go)
   out of the test build and into shared test-support code.)
2. **Replace token, repo, user, key, and webhook operations** with Go calls.
   These are where typed payloads and real error propagation pay off the most.
3. **Leave `known_hosts` generation and `secrets.yaml` building as shell**
   for now, unless we also tackle the deploy-key and per-user-isolation
   workstream. Those two shell blocks have the lowest benefit-per-LOC ratio.
4. **Ship a `cmd/e2e-gitea-setup` CLI** so `task e2e-gitea-run-setup` stays
   usable for manual debugging and CI.
5. **Delete the ported shell paths only after the Go path has been green in
   CI for multiple runs.** No dual-path indefinitely.

This gets the top-three pros (one client, typed payloads, real errors) at
maybe 40% of the cost of a full port and keeps the "I can read the shell
script to see what happens" escape hatch for the parts that still shell out.

## What Would Change the Answer

- If the team **commits to `ginkgo -p` parallel execution this quarter**, the
  full port is more attractive because the shell script's shared-state hazards
  become blockers instead of annoyances.
- If Gitea gets a **dedicated SSH signing-key API** (still absent in 1.25.4),
  the Go client gains a significantly nicer signing surface that the shell
  version would have to shoehorn in.
- If **multiple repos per file** becomes a recurring need, the Go port's
  concurrency and scoping story pays off faster.

## Verdict

A **narrow port of the HTTP-surface** (token, repo, user/email, keys, webhook,
commit verification) is a good investment on its own merits: it collapses
duplicated plumbing, gives loud failures, and matches the direction the harness
is already heading. It also unblocks the parallel-execution and per-file-user
workstreams without forcing them.

A **full port** of every shell line — including `known_hosts`, `secrets.yaml`
generation, and port-forward orchestration — is defensible but has a weaker
cost/benefit ratio. Recommend deferring those until there is a second concrete
reason to touch them (e.g. parallel execution, deploy-key isolation, or a
Windows-friendly dev environment).

## Suggested Next Step

If we want to move on this, the first commit should be scoped to:

- create `test/e2e/gitea/` package
- move `gitea_api_test.go` helpers into it (making them reusable outside
  `_test.go` files)
- port `create_token`, `ensure_repo`, `configure_ssh_key_in_gitea`, and
  `ensure_repo_webhook` behind typed helpers
- leave `known_hosts`, `secrets.yaml`, and repo checkout in shell for now
- keep `task e2e-gitea-run-setup` working through a minimal Go CLI entry point

Everything else is a candidate for a later pass once that seam has proved
itself.
