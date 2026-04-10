# Image Refresh Test Design

## What This Tests

The Make image-refresh dependency chain is load-bearing infrastructure. When it breaks,
developers waste time chasing stale pods, and CI can silently run tests against the wrong
build. This document defines the scenarios that must hold, and how to validate them.

The chain under test:

```
GO_SOURCES → controller.id → project-image.ready → image.loaded → controller.deployed
```

Each arrow is a Make dependency. When any input is newer than its stamp, the chain
invalidates forward. The end result: `kubectl rollout restart` fires, the pod picks up
the newly loaded image from containerd.

## Scenarios

### S1 — No-op: second run without changes does nothing

**Setup:** run `make prepare-e2e` once to reach a known good state.

**Action:** run `make prepare-e2e` again immediately, with no source changes.

**Assert:**
- `controller.deployed` stamp mtime is unchanged
- no `rollout restart` was issued
- the running pod is the same pod (same name, same start time)

**Why it matters:** if every run restarts the pod, tests become slow and flaky. The
stamp mechanism exists precisely to avoid this.

---

### S2 — Go source change triggers pod restart

**Setup:** cluster in known good state from S1.

**Action:** append a comment line to `cmd/main.go`, run `make prepare-e2e`.

**Assert:**
- `controller.id` stamp is newer than before
- `image.loaded` stamp content changed (new digest)
- `controller.deployed` stamp content changed (new `IMAGE@digest`)
- a new pod is running (different name or newer start time)
- the old pod is gone

**Why it matters:** this is the primary correctness invariant. A Go change must reach
the running pod.

---

### S3 — Second Go change also triggers (not one-shot)

**Setup:** cluster in state after S2 (pod already restarted once).

**Action:** append another comment to `cmd/main.go`, run `make prepare-e2e`.

**Assert:** same as S2 — new pod, new digest in stamps.

**Why it matters:** rules out accidental one-shot behavior where the first change works
because a stamp was missing, but subsequent changes are silently ignored.

---

### S4 — Non-Go file change does not trigger rebuild

**Setup:** cluster in known good state.

**Action:** append a comment to `test/e2e/helpers.go` (excluded from `GO_SOURCES`),
run `make prepare-e2e`.

**Assert:**
- `controller.id` stamp mtime is unchanged
- `image.loaded` stamp mtime is unchanged
- `controller.deployed` stamp mtime is unchanged
- pod start time is unchanged

**Why it matters:** confirms `GO_SOURCES` filtering is correct. Test-only changes must
not cause a rebuild.

---

### S5 — Dockerfile change triggers rebuild

**Setup:** cluster in known good state.

**Action:** append a comment to `Dockerfile`, run `make prepare-e2e`.

**Assert:** same as S2 — new pod, new digest.

**Why it matters:** `Dockerfile` is listed in `GO_SOURCES` (as a build input). A
changed Dockerfile must produce a fresh image.

---

### S6 — Stamp content matches what was actually deployed

**Setup:** any state after a successful `make prepare-e2e`.

**Assert:**
- `image.loaded` contains `gitops-reverser:e2e-local@sha256:<digest>`
- `controller.deployed` contains the same value
- the running pod's image matches (via `kubectl get pod -o jsonpath`)

**Why it matters:** stamps are only useful if they reflect reality. This catches
drift between stamp content and cluster state.

---

### S7 — Load-image is idempotent when digest is unchanged

**Setup:** cluster in known good state.

**Action:** touch `cmd/main.go` (update mtime without changing content) — this forces
`controller.id` to rerun `docker build`, but the resulting image should be identical
(same digest, since content is unchanged).

**Assert:**
- `image.loaded` stamp mtime may update, but stamp content is unchanged (same digest)
- no `rollout restart` was issued (since `controller.deployed` is still up-to-date
  relative to `image.loaded`)

**Why it matters:** validates that the `IMAGE@digest` stamp in `load-image.sh`
correctly short-circuits the import when digest is unchanged.

> Note: this scenario depends on Docker build cache producing a deterministic digest.
> If the build is not reproducible (e.g. timestamps embedded in the binary), this
> scenario may not hold and should be skipped or marked expected-to-fail.

---

## Decision Tree: Where to Test

```
Is this testing Make dependency semantics (stamps, mtimes, rebuild triggers)?
├── Yes → needs to run make and inspect stamp files
│         → shell script OR Go subprocess test
│
└── No, this is testing the running cluster state (pod identity, image digest)?
          → needs kubectl access and structured assertions
          → Go test (Ginkgo) is a better fit
```

For this suite, both apply. The recommendation is:

### Use Go (Ginkgo) in the existing e2e suite

**Reasons:**

- The e2e suite already owns cluster setup and teardown. These tests need the same
  cluster. Putting them in Go avoids a second setup path.

- Ginkgo `BeforeEach` / `AfterEach` can handle file modifications and `git checkout`
  cleanup reliably, even on test failure.

- Kubernetes assertions (pod name, start time, image digest) are better expressed
  with the typed client than with `kubectl ... -o jsonpath` in shell.

- Make can be invoked from Go via `exec.Command("make", ...)` and stdout/stderr can be
  captured and asserted against (e.g. check for "Restarting deployment" or absence of it).

- The existing `test/e2e/helpers.go` already has patterns for waiting on pod state.

**Structure:**

```
test/e2e/
  image_refresh_test.go   ← new file, Ginkgo suite
```

The suite should be tagged or placed in a separate `Describe` block so it can be run
independently:

```bash
go test ./test/e2e/ -v -ginkgo.v --label-filter="image-refresh"
```

or as part of the full suite without filtering.

**Each scenario maps to a Ginkgo `It` block:**

```go
Describe("image refresh dependency chain", Label("image-refresh"), func() {
    It("S1: no-op run does not restart the pod", func() { ... })
    It("S2: Go source change triggers pod restart", func() { ... })
    It("S3: second Go change also triggers restart", func() { ... })
    It("S4: test-only file change does not trigger rebuild", func() { ... })
    It("S5: Dockerfile change triggers rebuild", func() { ... })
    It("S6: stamp content matches deployed pod image", func() { ... })
})
```

**File modification helper:**

```go
func appendComment(t GinkgoTInterface, path, marker string) {
    f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
    Expect(err).NotTo(HaveOccurred())
    _, err = fmt.Fprintf(f, "\n// image-refresh-test: %s\n", marker)
    Expect(err).NotTo(HaveOccurred())
    f.Close()
    DeferCleanup(func() {
        Expect(exec.Command("git", "checkout", "--", path).Run()).To(Succeed())
    })
}
```

**Make invocation helper:**

```go
func runPrepare(ctx context.Context) string {
    cmd := exec.CommandContext(ctx, "make",
        "CTX="+kubeContext,
        "NAMESPACE="+namespace,
        "INSTALL_MODE="+installMode,
        "prepare-e2e",
    )
    out, err := cmd.CombinedOutput()
    Expect(err).NotTo(HaveOccurred(), string(out))
    return string(out)
}
```

**Pod identity helper:**

```go
func currentPodIdentity(ctx context.Context) (name, startTime string) {
    // list pods matching CONTROLLER_DEPLOY_SELECTOR
    // return name + startTime of the newest pod
}
```

### What NOT to use Go for

Stamp mtime comparison is awkward in Go (`os.Stat(...).ModTime()`). For scenarios
where the assertion is purely "stamp was not touched", capture make output and assert
absence of "Restarting deployment" — that is more reliable than mtime arithmetic.

## CI Integration

Add a Makefile target that runs only the image-refresh suite:

```make
.PHONY: test-image-refresh
test-image-refresh: ## Validate the Make image-refresh dependency chain
    export CTX=$(CTX)
    export INSTALL_MODE=$(INSTALL_MODE)
    export NAMESPACE=$(NAMESPACE)
    export E2E_AGE_KEY_FILE=$(CS)/age-key.txt
    go test ./test/e2e/ -v -ginkgo.v --label-filter="image-refresh"
```

In CI, run this after cluster setup and before the main `test-e2e` suite, so a broken
refresh chain fails fast with a clear message rather than silently passing with stale
pods.
