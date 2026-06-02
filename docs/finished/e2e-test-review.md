# E2E Test Review: Gomega/Ginkgo Implementation

Related investigation: [E2E Full-Suite Shared-State Investigation](./e2e-full-suite-shared-state-investigation.md)

## What's Good

**Correct `Eventually(func(g Gomega))` pattern throughout.** Every poll loop threads the inner `g Gomega` correctly. This avoids the classic goroutine-abort bug where an outer `Expect` inside `Eventually` kills the test process instead of retrying. That's the most important Gomega correctness rule and it's applied consistently.

**`suite_state.go` is clean.** `atomic.Bool` for the preservation flag, `sync.Mutex` for the reason slice, and `sync.RWMutex` for the done channel are all the right primitives. The separation of concerns between the state file and the suite file is good.

**`DeferCleanup`-equivalent through `skipCleanupBecauseResourcesArePreserved`.** Every cleanup helper checks preservation before deleting. This is exactly the right pattern for post-failure debugging.

**Signal handling in `watchForE2EInterrupts`.** `SIGTERM` + `SIGINT` both invoke `markE2EResourcesForPreservation` then cancel the suite context. Clean.

**`By()` saturation.** Step descriptions give Ginkgo's report real content. Failure output is much easier to triage than suites that skip `By`.

**`signing_helpers_test.go` unit-tests pure functions.** `TestRemoveGpgsigHeader_PreservesTrailingNewlineState` runs with `go test` without a cluster. That's exactly what should happen for logic that can be isolated.

**`kubectl.go` double-dash handling.** The code that inserts `-n namespace` before `--` in exec-style kubectl calls is subtle and correct.

---

## Issues and Improvements

### 1. Mutable package-level vars set in `BeforeAll` — latent data race

[e2e_test.go:47-54](../../../test/e2e/e2e_test.go#L47):

```go
var testRepoName string
var checkoutDir string
var (
    gitSecretHTTP    = "git-creds"
    gitSecretSSH     = "git-creds-ssh"
    gitSecretInvalid = "git-creds-invalid"
)
```

These are package-level vars mutated in `BeforeAll` of the `Manager` suite. If the suite is ever run with `-p` (parallel packages) or a second `Describe` reads these before the `Manager` `BeforeAll` fires, you get undefined behaviour. The `signing_e2e_test.go` suite reads env vars directly in its own `BeforeAll` to avoid this, which is the better model. Move all per-suite fixtures into a struct local to the `Describe` closure, the way `biDirectionalRun` does it correctly.

### 2. `verifyResourceStatus` has a resource-type special case baked in

[e2e_test.go:1760-1764](../../../test/e2e/e2e_test.go#L1760):

```go
if resourceType == "gittarget" && expectedReason == "Ready" {
    g.Expect([]string{"Ready", "OK"}).To(ContainElement(readyReason))
} else {
    g.Expect(readyReason).To(Equal(expectedReason))
}
```

A general status helper should not know about `gittarget`. Either accept `acceptableReasons ...string` as a variadic parameter, or — better — fix the CRD to emit a single consistent reason. The current shape means every new resource type quirk adds another branch.

### 3. Cleanup inside `It` blocks doesn't run on failure

[signing_e2e_test.go:200-205](../../../test/e2e/signing_e2e_test.go#L200):

```go
By("cleaning up")
_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, ...)
cleanupWatchRule(watchRuleName, testNs)
cleanupGitTarget(destName, testNs)
```

If the test panics or `Expect` fails before this point, those resources leak into the next test in the same namespace. The fix is `DeferCleanup` registered right after each resource is created:

```go
cmName := "signing-test-cm-per-event"
_, err = kubectlRunInNamespace(testNs, "create", "configmap", cmName, ...)
Expect(err).NotTo(HaveOccurred())
DeferCleanup(func() {
    if skipCleanupBecauseResourcesArePreserved("configmap " + cmName) {
        return
    }
    _, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
})
```

`DeferCleanup` participates in Ginkgo's lifecycle properly and respects ordering (LIFO). It also integrates with `GinkgoRecover`.

### 4. `e2eCommandContext` spawns a goroutine per kubectl call that lives until suite end

[kubectl.go:137-157](../../../test/e2e/kubectl.go#L137):

```go
commandCtx, cancel := context.WithCancel(context.Background())
go func() {
    select {
    case <-done:
        cancel()
    case <-commandCtx.Done():
    }
}()
return commandCtx
```

`cancel` is never called by the goroutine in the `<-commandCtx.Done()` branch, and it is never exposed to the caller. So for every normal kubectl invocation, a goroutine hangs until `done` closes (suite end). Over a full run that is hundreds of goroutines. The fix is either to pass the suite-level `context.Context` directly (simpler, no goroutine needed), or to add `defer cancel()` at the top of the goroutine so it always fires when the goroutine exits.

### 5. `audit_redis_e2e_test.go` uses raw `exec.Command("git")` instead of `gitRun`

[audit_redis_e2e_test.go:249-252](../../../test/e2e/audit_redis_e2e_test.go#L249):

```go
pullCmd := exec.Command("git", "pull")
pullCmd.Dir = gitCheckout
pullOut, pullErr := pullCmd.CombinedOutput()
```

`gitRun` in [helpers.go:296-301](../../../test/e2e/helpers.go#L296) wraps git in `e2eCommandContext` so Ctrl+C cancellation works. This raw call bypasses that. It also bypasses `GinkgoWriter` logging. Use `gitRun(gitCheckout, "pull")` instead.

### 6. `audit_redis_e2e_test.go` hardcodes the secret name fallback

[audit_redis_e2e_test.go:178-181](../../../test/e2e/audit_redis_e2e_test.go#L178):

```go
gitSecretName := strings.TrimSpace(os.Getenv("E2E_GIT_SECRET_HTTP"))
if gitSecretName == "" {
    gitSecretName = "git-creds"
}
```

The correct fallback is `resolveE2EHTTPSecretName(repoName)`, which appends the repo name. The plain `"git-creds"` will point at the wrong secret when `E2E_GIT_SECRET_HTTP` is unset and the repo name is not the legacy default.

### 7. `resolveE2EContext()` default says `kind-` but the cluster is `k3d-`

[e2e_suite_test.go:242](../../../test/e2e/e2e_suite_test.go#L242):

```go
return "kind-gitops-reverser-test-e2e"
```

The actual stamp path and cluster name is `k3d-gitops-reverser-test-e2e`. If `CTX` is unset and `kubectl config current-context` fails, this fallback silently points at a non-existent context.

### 8. `SetDefaultEventuallyTimeout` called in `Describe` body, not in a lifecycle hook

[e2e_test.go:123-124](../../../test/e2e/e2e_test.go#L123):

```go
// Optimize timeouts for faster test execution
SetDefaultEventuallyTimeout(30 * time.Second)
SetDefaultEventuallyPollingInterval(time.Second)
```

In Ginkgo v2, calling these in the `Describe` body (not in a lifecycle hook or `BeforeSuite`) is evaluated during the tree-building phase, which works, but the intent is unclear and the ordering relative to nested `Describe` setup is implicit. The idiomatic location is `BeforeSuite` (global defaults) or `BeforeAll` (suite-scoped defaults).

### 9. `waitForMetric` condition is opaque on failure

[helpers.go:105-123](../../../test/e2e/helpers.go#L105):

```go
func waitForMetricWithTimeout(query string, condition func(float64) bool, ...) {
    Eventually(func(g Gomega) {
        value, err := queryPrometheus(query)
        g.Expect(err).NotTo(HaveOccurred(), "Failed to query Prometheus")
        g.Expect(condition(value)).To(BeTrue(),
            fmt.Sprintf("%s (query: %s, value: %.2f)", description, query, value))
    }, ...)
}
```

The failure message reconstructs what you know but not what was expected. The `condition func(float64) bool` shape also forces callers to re-describe the condition in the string. A richer API accepts a matcher:

```go
func waitForMetric(query string, matcher types.GomegaMatcher, description string) {
    Eventually(func(g Gomega) {
        value, _ := queryPrometheus(query)
        g.Expect(value).To(matcher, description)
    }).Should(Succeed())
}

// Call site becomes self-documenting:
waitForMetric("sum(up{job='gitops-reverser'})", BeNumerically("==", 1), "controller up")
```

### 10. Redis entry failure message doesn't show what was actually received

[audit_redis_e2e_test.go:101-118](../../../test/e2e/audit_redis_e2e_test.go#L101):

```go
g.Expect(found).To(BeTrue(), "expected ConfigMap audit event to be enqueued in Redis stream")
```

On failure you know the predicate was false but not what entries existed. Prefer:

```go
g.Expect(entries).To(ContainElement(
    HaveField("Values", HaveKeyWithValue("name", testConfigMapName)),
), "expected audit event for ConfigMap %s in stream", testConfigMapName)
```

Or at minimum format the entries slice into the failure annotation.

### 11. `AfterAll` debug banner always prints to `fmt.Printf`, not `GinkgoWriter`

[e2e_test.go:108-120](../../../test/e2e/e2e_test.go#L108):

```go
fmt.Printf("═══════════════════════════════════════════════════════════\n")
fmt.Printf("📊 E2E Infrastructure kept running for debugging purposes:\n")
```

`fmt.Printf` goes straight to stdout and is not captured by Ginkgo's report, XML output, or JSON reporter. Use `fmt.Fprintf(GinkgoWriter, ...)` or `AddReportEntry("infra", ...)` so the output appears in the right place and is filterable.

---

## Features to Adopt

| Feature | Where to use |
|---|---|
| `DeferCleanup` | Replace end-of-`It` cleanup in all signing and manager tests |
| `ContainElement(HaveField(...))` | Redis stream entry assertions |
| `DescribeTable` / `Entry` | The three signing tests (per-event, batch, committer) share the same structure |
| `AddReportEntry` | Replace raw `fmt.Printf` banners in `AfterAll` |
| `MustPassRepeatedly(n)` (Gomega 1.32+) | Replace `biStableCount*` sleep patterns in the bi-directional suite |
| `Consistently` | Add stability windows after commit assertions to catch spurious re-commits |
| Custom `gomega.Matcher` (e.g. `BeReadyCondition`) | Reduce 10-line `verifyResourceStatus` call sites to one-liner matchers with rich diffs |

---

## Priority Summary

| Priority | Issue |
|---|---|
| High | `DeferCleanup` in signing `It` blocks — resources leak on failure |
| High | `audit_redis_e2e_test.go` raw `exec.Command("git")` bypasses cancellation |
| High | `audit_redis_e2e_test.go` wrong secret name fallback (`"git-creds"` instead of `resolveE2EHTTPSecretName`) |
| Medium | Mutable package-level vars in `e2e_test.go` — move into a per-suite struct like `biDirectionalRun` |
| Medium | `e2eCommandContext` goroutine-per-call leak — add `defer cancel()` or pass suite context directly |
| Medium | `resolveE2EContext()` wrong `kind-` default (should be `k3d-`) |
| Medium | `verifyResourceStatus` `gittarget` special case — accept variadic acceptable reasons instead |
| Low | `waitForMetric` opaque condition — switch to matcher API |
| Low | `SetDefaultEventuallyTimeout` in `Describe` body — move to `BeforeAll` |
| Low | Redis failure message missing actual entries |
| Low | `fmt.Printf` banners should use `GinkgoWriter` / `AddReportEntry` |
