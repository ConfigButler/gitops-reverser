# E2E repo-per-file plan

## Goal

Make e2e Git repo bootstrapping:

- shared for the expensive cluster/install setup
- isolated for mutable Git repo state
- simpler to reason about in code
- ready for future parallel execution when the remaining shared Gitea constraints are removed

The target shape is:

- `BeforeSuite` keeps preparing shared cluster/install infrastructure
- each e2e test file owns its own repo fixtures
- each test file continues to use its own test namespace(s)
- shared artifacts stay limited to cluster/install state

This is primarily a simplification and isolation refactor. It should also move the harness toward `ginkgo -p` safety instead
of depending on package-global repo state.

## Current state

Today the package `BeforeSuite` in [test/e2e/e2e_suite_test.go](/workspaces/gitops-reverser/test/e2e/e2e_suite_test.go:59):

- runs `task prepare-e2e`
- runs `task e2e-gitea-run-setup`
- exports repo artifacts globally through `E2E_REPO_NAME`, `E2E_CHECKOUT_DIR`, `E2E_SECRETS_YAML`, and related vars

That means:

- one package run gets one shared repo
- most test files reuse that same repo and checkout
- mutable repo fixtures are effectively package-scoped, not file-scoped
- the code has hidden coupling through global env vars

This is fragile because one file can damage shared repo state that another file still expects.

## Decisions

These choices are intentional and should be reflected in the implementation:

- Keep one shared cluster/install setup in `BeforeSuite`.
- Use one repo per e2e test file.
- Keep repo stamps scoped to the test namespace or namespaces used by that file.
- Keep demo special:
  - `REPO_NAME=demo` stays fixed
  - `TESTNAMESPACE=vote` stays fixed
  - demo remains a reusable manual-inspection environment
- Support Flux receiver/webhook registration, but make it optional.
- Improve signing so both flows work:
  - operator-generated signing key
  - BYOK signing key
- Keep local verification in the tests:
  - `ssh-keygen -Y verify`
  - `git verify-commit`
- Also make Gitea show commits as properly signed, so the result is inspectable in the UI.

## Scope model

We only have one Ginkgo suite today: the package-level e2e suite.

So for this refactor, "suite-scoped repo" was too vague. The practical unit will be:

- one repo per e2e test file

That means:

- [test/e2e/e2e_test.go](/workspaces/gitops-reverser/test/e2e/e2e_test.go:1) gets its own repo
- [test/e2e/signing_e2e_test.go](/workspaces/gitops-reverser/test/e2e/signing_e2e_test.go:1) gets its own repo
- [test/e2e/quickstart_framework_e2e_test.go](/workspaces/gitops-reverser/test/e2e/quickstart_framework_e2e_test.go:1) gets its own repo
- [test/e2e/bi_directional_e2e_test.go](/workspaces/gitops-reverser/test/e2e/bi_directional_e2e_test.go:1) gets its own repo
- [test/e2e/demo_e2e_test.go](/workspaces/gitops-reverser/test/e2e/demo_e2e_test.go:1) keeps the fixed `demo` repo
- [test/e2e/audit_redis_e2e_test.go](/workspaces/gitops-reverser/test/e2e/audit_redis_e2e_test.go:1) uses one repo for the whole file

### Audit Redis file choice

`audit_redis_e2e_test.go` currently contains two top-level `Describe`s:

- producer path
- consumer path

To keep this refactor simpler and land it as one cohesive commit, we should **not** split that file yet.

Instead:

- the file gets one repo
- the producer path continues to mostly ignore repo state
- the consumer path uses the file-local repo artifacts

If we later want independent repos for those paths, we can split the file then.

## Namespace model

There are still two kinds of namespaces:

1. **Install namespace**
   - from `resolveE2ENamespace()`
   - usually `gitops-reverser`
   - owns controller install state, webhook TLS, SOPS secret state, and `prepare-e2e.ready`

2. **Test namespace**
   - from `testNamespaceFor(...)` or the demo override
   - owns file-local GitProvider, GitTarget, WatchRule, and copied repo credentials

The repo stamp path should follow the file's test namespace, not the install namespace.

So repo artifacts should live under:

- `.stamps/cluster/<ctx>/<test-namespace>/git-<repo>/`

and not under:

- `.stamps/cluster/<ctx>/<install-namespace>/git-<repo>/`

For files that use more than one test namespace, the file should choose one primary namespace for repo bootstrapping and read
artifacts from that location consistently.

For `audit_redis_e2e_test.go`, the simplest choice is:

- bootstrap the repo in `testNamespaceFor("audit-consumer")`

because that is the part of the file that actually uses the repo checkout and credentials.

## Artifact API

The current package-global env vars are the wrong abstraction for mutable repo fixtures.

Cluster-level globals can remain, for example:

- `E2E_AGE_KEY_FILE`

Repo state should move to a typed helper returned to the file:

```go
type RepoArtifacts struct {
    RepoName          string
    RepoURLHTTP       string
    RepoURLSSH        string
    CheckoutDir       string
    SecretsYAML       string
    GitSecretHTTP     string
    GitSecretSSH      string
    GitSecretInvalid  string
    ReceiverWebhookURL string
    ReceiverWebhookID  string
}

func SetupRepo(ctx, namespace, repoName string) (*RepoArtifacts, error)
```

Implementation notes:

- `ctx` should come from `resolveE2EContext()`
- the namespace must already exist before `SetupRepo` runs
- `SetupRepo` should invoke `task e2e-gitea-run-setup` for that repo
- `SetupRepo` should read the stamp files and return a struct
- tests should keep that struct in file-local variables instead of reading package-global repo env vars

Compatibility env vars are optional, but should not be the main API.

## File-to-repo mapping

| File | Test namespace model | Repo name |
|---|---|---|
| `e2e_test.go` | `testNamespaceFor("manager")` | `e2e-manager-<seed>` |
| `signing_e2e_test.go` | `testNamespaceFor("signing")` | `e2e-signing-<seed>` |
| `audit_redis_e2e_test.go` | `testNamespaceFor("audit-consumer")` as repo owner; producer namespace remains separate | `e2e-audit-redis-<seed>` |
| `quickstart_framework_e2e_test.go` | `testNamespaceFor("quickstart-framework")` | `e2e-quickstart-framework-<seed>` |
| `bi_directional_e2e_test.go` | `testNamespaceFor("bi-directional")` | `e2e-bi-directional-<seed>` |
| `demo_e2e_test.go` | fixed `vote` namespace via `TESTNAMESPACE=vote` | fixed `demo` |
| `image_refresh_test.go` | install namespace only | no repo |

The names should stay readable and close to the file purpose. That makes `.stamps` and local checkouts easier to inspect.

## Stamp contents

Repo-specific stamps remain under:

- `.stamps/cluster/<ctx>/<test-namespace>/git-<repo>/`

Expected contents:

- always:
  - `active-repo.txt`
  - `checkout-path.txt`
  - `token.txt`
  - `secrets.yaml`
  - `repo.ready`
  - `checkout.ready`
- optional:
  - `ssh/id_rsa`
  - `ssh/id_rsa.pub`
  - `ssh/known_hosts`
  - `receiver-webhook-url.txt`
  - `receiver-webhook-id.txt`

Receiver webhook files are optional because receiver setup itself is optional.

## Optional webhook receiver support

Repo setup should continue to support Gitea webhook registration to a Flux `Receiver`, but it must be optional.

Desired behavior:

- if the matching receiver resources exist, configure the repo webhook and write:
  - `receiver-webhook-url.txt`
  - `receiver-webhook-id.txt`
- if the receiver resources do not exist, continue successfully without failing repo setup

This matches the current best-effort behavior in [hack/e2e/gitea-run-setup.sh](/workspaces/gitops-reverser/hack/e2e/gitea-run-setup.sh:266).

Important clarification:

- optional receiver support is part of the repo helper contract
- provisioning new per-repo `Receiver` objects for every file is **not** required in this refactor

Demo remains special here too:

- `demo` keeps using the existing fixed receiver shape
- the fixed `demo` repo name must remain compatible with the current demo-only receiver manifests

## Signing goals

The signing file should become the strongest proof that signing works end to end.

We want all of these to be true:

- the controller can create signed commits
- local tools can verify those commits
- Gitea shows those commits as signed in the UI

### Flows to cover

1. **Operator-generated key**
   - configure `GitProvider.spec.commit.signing.generateWhenMissing=true`
   - wait for `GitProvider.status.signingPublicKey`
   - register that public key with Gitea for signature verification
   - verify the resulting commit with:
     - `ssh-keygen -Y verify`
     - `git verify-commit`
   - manually inspect in Gitea and confirm the commit is shown as signed

2. **BYOK**
   - generate an SSH signing key pair in test setup
   - write the private key into the referenced Secret
   - register the public key with Gitea for signature verification
   - verify the resulting commit with:
     - `ssh-keygen -Y verify`
     - `git verify-commit`
   - manually inspect in Gitea and confirm the commit is shown as signed

### Signing implementation direction

This likely requires better Gitea-side signing key registration than we have today.

The refactor should therefore include a small signing-specific helper, for example:

```go
func RegisterSigningPublicKey(ctx, signingPublicKey string) error
```

It is fine for this to take a bit more work than the basic repo isolation, because the goal is not only local crypto
verification but also correct Gitea UI verification.

### SSH transport note

The repo-per-file refactor should not pretend SSH transport isolation is already solved.

Today `gitea-run-setup.sh` still resets user SSH keys in Gitea, which is a shared-state hazard for parallel runs:

- [hack/e2e/gitea-run-setup.sh](/workspaces/gitops-reverser/hack/e2e/gitea-run-setup.sh:158)

Longer term, the clean model is:

- repo access via deploy keys or isolated users
- signing verification via signing-key registration

That work fits the direction of this plan, but it does not need to be fully generalized before the repo-per-file refactor
lands.

## Parallel execution

This refactor should make parallel execution more realistic, but not overclaim full safety yet.

What this refactor improves:

- distinct repo names
- distinct checkout dirs
- distinct stamp paths
- less package-global mutable state in Go code

What still blocks clean `ginkgo -p` support:

- shared Gitea user/key mutation
- any shared receiver naming assumptions
- any remaining package-global repo env coupling

So the right statement is:

- this plan reduces coupling and moves the harness toward parallel-safe execution
- it does not, by itself, guarantee that all e2e files are immediately safe under `ginkgo -p`

## Implementation plan

Keep this as one cohesive refactor commit, not a long migration series.

1. Add a file-local repo helper, likely `test/e2e/suite_repo.go`
   - define `RepoArtifacts`
   - move stamp-reading logic out of `exportGiteaArtifacts`
   - add `SetupRepo(ctx, namespace, repoName string) (*RepoArtifacts, error)`

2. Update [test/e2e/e2e_suite_test.go](/workspaces/gitops-reverser/test/e2e/e2e_suite_test.go:72)
   - keep `BeforeSuite` for `task prepare-e2e`
   - remove the package-wide `task e2e-gitea-run-setup`
   - keep cluster-level globals like `E2E_AGE_KEY_FILE`

3. Update each repo-using e2e file
   - create namespace first
   - call `SetupRepo(...)`
   - store returned artifacts in file-local vars
   - stop depending on `E2E_REPO_NAME`, `E2E_CHECKOUT_DIR`, and `E2E_SECRETS_YAML` as the primary interface

4. Keep demo special-case behavior intact
   - preserve `REPO_NAME=demo`
   - preserve `TESTNAMESPACE=vote`
   - preserve compatibility with the current demo receiver and secret reflection behavior

5. Improve signing in the same commit
   - keep local verification
   - add Gitea-visible signing verification support
   - cover both generated-key and BYOK flows

## Non-goals

This refactor is not trying to:

- redesign the full e2e suite structure
- create one Ginkgo suite per file
- fully solve every shared Gitea parallelism problem
- require Flux receiver resources for every repo

## Summary

The intended end state is simple:

- one shared cluster/install setup
- one repo per e2e test file
- demo stays special and fixed
- receiver support stays available but optional
- signing gets stronger, not weaker
- the code becomes easier to follow and less coupled to package-global repo env vars

That should give us a cleaner harness immediately and a much better base for future parallel execution.
