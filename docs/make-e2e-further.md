# Further e2e simplifications: moving setup out of test code

## The theme

The stamp-based approach in `make-e2e-deps.md` established one principle: if something can be verified by a file's mtime, it shouldn't run inside `go test`. The remaining opportunity is the same principle applied to the **per-suite setup** in `BeforeAll` — things that are cluster-side operations, not test logic.

This document analyses what's still happening inside the test binary on every `go test` run, what can be lifted out, and how.

---

## Current `BeforeAll` overview

```
BeforeAll
  1. preventive CRD cleanup (kubectl delete crd icecreamorders...)
  2. create namespace "sut"
  3. label namespace (pod-security.kubernetes.io/enforce=restricted)
  4. generate age key + create sops-age-key Secret        ← main target
  5. wait for cert-manager certificate secrets
  6. setup Gitea (org, repo, SSH keys, credential secrets)
  7. init Prometheus client + verify available
```

Steps 1–5 are cluster-side operations with no per-run variability. Step 6 is intrinsically per-run (unique repo name). Step 7 is a pure Go object initialisation.

---

## 1. Age key generation (primary goal)

### What the code does today

`helpers.go:setupSOPSAgeSecret` (line 284):

1. Calls `age.GenerateX25519Identity()` — generates a fresh key pair every suite run.
2. Writes the key to `/tmp/e2e-age-key.txt` in the standard `age-keygen` format.
3. Constructs a Secret YAML in memory and pipes it to `kubectl apply`.

The test code later reads the key back from `/tmp/e2e-age-key.txt` via `readSOPSAgeKeyFromFile` to decrypt encrypted payloads.

### Why a stamp makes sense

The key does **not** need to change between runs. A stable key means:
- The Secret is only re-applied when the cluster is fresh (stamp deleted with cluster).
- The YAML is a visible file on disk — auditable, not conjured from Go memory.
- `setupSOPSAgeSecret` can be deleted from Go code entirely.

### Implementation

#### Step 1 — install `age-keygen` in the devcontainer

In [.devcontainer/Dockerfile](.devcontainer/Dockerfile), add to the `ci` stage alongside the other tool installs:

```dockerfile
ENV AGE_VERSION=v1.2.1
RUN curl -fsSL "https://github.com/FiloSottile/age/releases/download/${AGE_VERSION}/age-${AGE_VERSION}-linux-amd64.tar.gz" \
    | tar -xzO age/age-keygen > /usr/local/bin/age-keygen \
    && chmod +x /usr/local/bin/age-keygen
```

`age-keygen` writes output in a format already compatible with `readSOPSAgeKeyFromFile`:
```
# created: 2025-01-01T00:00:00Z
# public key: age1...
AGE-SECRET-KEY-...
```

#### Step 2 — add the stamp target

The age key depends on the `sut` namespace existing, which is created by `controller.deployed` (kustomize applies the namespace). Place the stamp after `controller.deployed`:

```make
# Path where the test suite reads the age key from.
E2E_AGE_KEY_FILE ?= /tmp/e2e-age-key.txt

$(CS)/age-key.applied: $(CS)/controller.deployed
	mkdir -p $(CS)
	age-keygen -o $(CS)/age-key.txt 2>/dev/null
	cp $(CS)/age-key.txt $(E2E_AGE_KEY_FILE)
	AGE_SECRET=$$(grep '^AGE-SECRET-KEY-' $(CS)/age-key.txt); \
	printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: sops-age-key\n  namespace: sut\ntype: Opaque\nstringData:\n  identity.agekey: "%s"\n' "$$AGE_SECRET" \
	  > $(CS)/age-key-secret.yaml
	kubectl --context $(CTX) apply -f $(CS)/age-key-secret.yaml
	touch $@
```

Key details:
- `age-keygen -o <file>` writes the private key to the file; the corresponding public key goes to stdout. Redirect stdout to `/dev/null` so Make doesn't print it.
- The intermediate `$(CS)/age-key-secret.yaml` is a real file — inspectable, diffable, and consistent with the "materialise before apply" philosophy.
- `cp` to `$(E2E_AGE_KEY_FILE)` keeps the path the test reads from; alternatively replace the hardcoded constant with `os.Getenv("E2E_AGE_KEY_FILE")` in the test.
- The stamp is cluster-scoped so `make cleanup-cluster` automatically invalidates it.

#### Step 3 — wire it into `portforward.running`

`portforward.running` is the last infrastructure stamp before tests run. Add `age-key.applied` as a dependency there:

```make
$(CS)/portforward.running: $(CS)/controller.deployed $(CS)/gitea.installed \
                            $(CS)/prometheus.installed $(CS)/age-key.applied
```

Alternatively, make `$(CS)/e2e.passed` depend on it directly if you want the port-forward stamp to stay focused on network readiness.

#### Step 4 — simplify `BeforeAll`

Remove `setupSOPSAgeSecret(e2eAgeKeyPath)` from `BeforeAll` and delete the `setupSOPSAgeSecret` function from `helpers.go`. The `import "filippo.io/age"` can be removed from `e2e_test.go` if it is only used there (it is still used by `deriveAgeRecipient` and decryption helpers, so check first).

#### Alternative: small Go tool instead of `age-keygen`

If installing a new binary feels heavy, write a self-contained tool at
`test/e2e/tools/gen-age-key/main.go` using the already-present `filippo.io/age` dependency:

```go
// Flags: --key-file, --secret-name, --namespace, --secret-file
// Writes the age key file and the Secret YAML to the given paths.
```

Call it from the Make recipe:
```make
$(CS)/age-key.applied: $(CS)/controller.deployed
	mkdir -p $(CS)
	go run ./test/e2e/tools/gen-age-key \
	  --key-file $(CS)/age-key.txt \
	  --secret-file $(CS)/age-key-secret.yaml \
	  --namespace sut --secret-name sops-age-key
	cp $(CS)/age-key.txt $(E2E_AGE_KEY_FILE)
	kubectl --context $(CTX) apply -f $(CS)/age-key-secret.yaml
	touch $@
```

This avoids the new devcontainer dependency at the cost of a small extra Go file.

---

## 2. Namespace + pod-security label

### What the code does today

`BeforeAll` creates the `sut` namespace and labels it
`pod-security.kubernetes.io/enforce=restricted`. The namespace itself is already created by
`controller.deployed` (kustomize), so the `create ns` call just errors silently. The label however is test-specific and is not in the kustomize config.

### Proposal

Add a `$(CS)/namespace.configured` stamp that runs after `controller.deployed`:

```make
$(CS)/namespace.configured: $(CS)/controller.deployed
	kubectl --context $(CTX) label --overwrite ns sut \
	  pod-security.kubernetes.io/enforce=restricted
	touch $@
```

Make `age-key.applied` depend on it instead of `controller.deployed` directly (since the age Secret is also in `sut`):

```make
$(CS)/age-key.applied: $(CS)/namespace.configured
```

`BeforeAll` can then remove the namespace creation block and the label command, keeping only test-specific setup.

---

## 3. Certificate secret wait

### What the code does today

`waitForCertificateSecrets` polls for `admission-server-cert` and `audit-server-cert` to exist in the `sut` namespace, with a 60-second timeout. These are created by cert-manager in response to Certificate resources deployed via kustomize.

### Analysis

`controller.deployed` already calls `kubectl rollout status deploy/gitops-reverser -n sut`. The controller pod reaches Ready only once its TLS server can start — which requires the cert secrets to exist. So if `controller.deployed` succeeds, both secrets should be present.

**Caveat**: this depends on the readiness probe actually reflecting TLS readiness. If the readiness probe only checks a non-TLS endpoint, there is a race. Verify by checking the readiness probe definition in `config/manager/manager.yaml`. If the probe is on the HTTPS webhook port or any endpoint that needs a cert, the wait is already implicit.

**If confirmed implicit**: remove `waitForCertificateSecrets` from `BeforeAll` entirely — it is redundant.

**If a race is possible**: add a `$(CS)/certs.issued` stamp:

```make
$(CS)/certs.issued: $(CS)/controller.deployed
	kubectl --context $(CTX) wait secret admission-server-cert \
	  -n sut --for=jsonpath='{.metadata.name}'=admission-server-cert --timeout=60s
	kubectl --context $(CTX) wait secret audit-server-cert \
	  -n sut --for=jsonpath='{.metadata.name}'=audit-server-cert --timeout=60s
	touch $@
```

And remove `waitForCertificateSecrets` from `BeforeAll`.

---

## 4. Preventive CRD cleanup

### What the code does today

`BeforeAll` runs:
```go
kubectl delete crd icecreamorders.shop.example.com --ignore-not-found=true
```

This is defensive cleanup of a CRD that can be left behind by a test that installs it mid-suite. It exists because tests that deploy the icecreamorders CRD don't always clean up on failure.

### Proposal

Two options:

**Option A** — move into `AfterAll` as explicit test teardown. Better for test hygiene: the test that creates the resource owns the cleanup.

**Option B** — add to `controller.deployed` recipe, which already runs kustomize apply. A quick `--ignore-not-found` delete before the apply ensures a clean CRD slate. This is already idempotent.

Either way it leaves `BeforeAll`.

---

## 5. What cannot be moved (must stay in `BeforeAll`)

| Step | Why it stays |
|---|---|
| Gitea repo setup (`setup-gitea.sh`) | Time-based unique repo name; genuinely per-run. Can't be stamped. |
| Prometheus client init (`setupPrometheusClient`) | Pure Go object setup; no cluster side-effects. |
| `verifyPrometheusAvailable` | Uses the Go prometheus client; tiny and idiomatic in test setup. |

---

## 6. SSH key generation in `setup-gitea.sh`

`setup-gitea.sh` generates an RSA 4096-bit SSH key pair at `/tmp/e2e-ssh-key` on every invocation (line 142). Since the Gitea repo is recreated each run (new org token, new repo), the SSH key must be re-registered anyway. **This is correctly per-run** and does not need to move.

---

## Resulting `BeforeAll` after all changes

```go
BeforeAll(func() {
    // cert secrets: covered by controller.deployed rollout wait
    // age key: covered by $(CS)/age-key.applied stamp
    // namespace + label: covered by $(CS)/namespace.configured stamp

    By("setting up Gitea test environment with unique repository")
    companyStart := time.Date(2025, 5, 12, 0, 0, 0, 0, time.UTC)
    minutesSinceStart := int(time.Since(companyStart).Minutes())
    testRepoName = fmt.Sprintf("e2e-test-%d", minutesSinceStart)
    checkoutDir = fmt.Sprintf("/tmp/gitops-reverser/%s", testRepoName)
    cmd := exec.Command("bash", "test/e2e/scripts/setup-gitea.sh", testRepoName, checkoutDir)
    _, err := utils.Run(cmd)
    Expect(err).NotTo(HaveOccurred())

    By("setting up Prometheus client for metrics testing")
    setupPrometheusClient()
    verifyPrometheusAvailable()
})
```

`BeforeAll` goes from 7 operations (some with retry loops) to 2.

---

## Summary of proposed new stamps

| Stamp | Depends on | Replaces |
|---|---|---|
| `$(CS)/namespace.configured` | `controller.deployed` | namespace create + pod-security label in `BeforeAll` |
| `$(CS)/age-key.applied` | `namespace.configured` | `setupSOPSAgeSecret` in `BeforeAll` |
| `$(CS)/certs.issued` *(optional)* | `controller.deployed` | `waitForCertificateSecrets` in `BeforeAll` |

Updated dependency chain for `portforward.running`:

```
portforward.running
  ├─ controller.deployed
  │    └─ namespace.configured
  │         └─ age-key.applied
  │              └─ $(CS)/age-key-secret.yaml  ← materialised file
  ├─ gitea.installed
  └─ prometheus.installed
```

Or, if you prefer to keep `portforward.running` focused on network readiness and not pull in age key setup, make `e2e.passed` depend on `age-key.applied` directly.
