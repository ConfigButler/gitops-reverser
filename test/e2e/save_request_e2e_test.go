// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The SaveRequest suite exercises the "save" signal: a SaveRequest
// object finalizes the open commit window for a GitTarget immediately, instead
// of waiting for the rolling silence timer. The GitProvider is configured with
// a deliberately long commitWindow so that, without the SaveRequest, the
// edit would not be committed for minutes — observing the commit promptly is
// what proves the save-request path works.
//
// Not Serial: this spec owns a dedicated Gitea repo (its own GitProvider →
// GitTarget → namespace-scoped WatchRule), so the only writer to its main
// branch is its own GitTarget, fed exclusively by audit events from its own
// namespace. The HEAD/SHA assertions below therefore read back only this spec's
// own commit; concurrent audit traffic for other GitTargets lands in other
// repos and cannot move this repo's HEAD. See docs/spec/e2e-serial-registry.md.
var _ = Describe("Save Request", Label("save-request", "audit-consumer"), Ordered, func() {
	var (
		testNs        string
		repo          *RepoArtifacts
		gitProvName   string
		gitTargetName string
		watchRuleName string
	)

	// commitWindow is long enough that the silence timer cannot be what produces the commit within
	// the assertion timeout below. It is a GitTarget field (spec.commit.window).
	const commitWindow = "300s"

	BeforeAll(func() {
		p := setupWindowedDeploymentPipeline("save-request", "e2e/save-request-test", commitWindow)
		testNs, repo, gitProvName, gitTargetName, watchRuleName = p.ns, p.repo, p.gitProvider, p.gitTarget, p.watchRule
	})

	AfterAll(func() {
		cleanupPipeline(testNs, gitProvName, gitTargetName, watchRuleName)
		cleanupNamespace(testNs)
	})

	It("finalizes the open commit window on demand and reports the resulting SHA", func() {
		basePath := "e2e/save-request-test"
		seed := GinkgoRandomSeed()
		deployName := fmt.Sprintf("save-request-deploy-%d", seed)
		saveRequestName := fmt.Sprintf("save-request-save-%d", seed)
		message := fmt.Sprintf("save: save request from e2e seed %d", seed)

		By("creating a Deployment to open a commit window")
		applyScaleTestDeployment(testNs, deployName, 0)

		By("confirming nothing is committed yet — the branch is not even created")
		// The namespace has no Deployments until now, so a brand-new GitTarget whose only edit is
		// still inside the open window has committed nothing at all: main does not exist. This is the
		// pure "defer committing as long as possible / keep the branch clean" behaviour — the initial
		// reconcile had nothing to materialise, so it created no branch.
		Consistently(func(g Gomega) {
			g.Expect(remoteBranchHead(g, repo.CheckoutDir)).To(BeEmpty(),
				"the open commit window must hold the edit; main must not exist until the window is finalized")
		}, 10*time.Second, 2*time.Second).Should(Succeed())

		By("creating a SaveRequest to finalize the open window now")
		applySaveRequest(testNs, saveRequestName, gitTargetName, message)

		By("waiting for the SaveRequest to commit and push")
		var reportedSHA string
		Eventually(func(g Gomega) {
			reportedSHA = expectSaveRequestCommitted(g, testNs, saveRequestName,
				recentCommitDiagnostics(repo.CheckoutDir, basePath))

			branch := saveRequestField(g, testNs, saveRequestName, "{.status.branch}")
			g.Expect(branch).To(Equal("main"))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("verifying the commit landed in Git with the explicit message and the Deployment manifest")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			expectedFile := filepath.Join(repo.CheckoutDir, basePath,
				fmt.Sprintf("%s/apps/deployments/%s.yaml", testNs, deployName))
			info, statErr := os.Stat(expectedFile)
			g.Expect(statErr).NotTo(HaveOccurred(), fmt.Sprintf("Deployment file should exist at %s", expectedFile))
			g.Expect(info.Size()).To(BeNumerically(">", 0))

			subject, logErr := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%B")
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(subject)).To(Equal(message),
				"the explicit spec.message should be used verbatim as the commit message\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))

			headSHA, shaErr := gitRun(repo.CheckoutDir, "rev-parse", "HEAD")
			g.Expect(shaErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(headSHA)).To(Equal(reportedSHA),
				"status.commit should match the commit on the branch\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))

			// Exactly one new commit: this is the first commit on a fresh
			// repo, so main holds exactly one commit — the save did not also trigger a
			// stray second commit.
			g.Expect(mustCommitCount(repo.CheckoutDir)).To(Equal(1),
				"the save must produce exactly one commit on main\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		// The wiring check for the metric instruments, made here because this is where the evidence
		// is: the commit above is verified in Git, so a missing series is a broken exporter rather
		// than a spec that did not run. Both queries are scoped to this suite's own GitTarget, so
		// they need no run bookkeeping to isolate from the other parallel processes.
		By("verifying this save reached the SaveRequest outcome counter")
		ensurePrometheusClient()
		waitForMetric(
			fmt.Sprintf(
				`sum(gitopsreverser_save_requests_total{outcome="committed",`+
					`gittarget_namespace=%q,gittarget_name=%q}) or vector(0)`,
				testNs, gitTargetName),
			func(v float64) bool { return v > 0 },
			"the committed save is counted under its own GitTarget",
		)

		By("verifying the window this save closed is counted and timed under its GitTarget")
		// The save attached to the Deployment's window and its own maxDuration (the 2s default)
		// closed it, so the window is counted under the request's timers, not the target's.
		waitForMetric(
			fmt.Sprintf(
				`sum(gitopsreverser_git_commit_windows_total{close_reason="max_duration",`+
					`timer_source="save_request",gittarget_namespace=%q,gittarget_name=%q}) or vector(0)`,
				testNs, gitTargetName),
			func(v float64) bool { return v > 0 },
			"the save's window is counted with the timer that closed it",
		)
		waitForMetric(
			fmt.Sprintf(
				`sum(gitopsreverser_git_commit_window_duration_seconds_count{`+
					`gittarget_namespace=%q,gittarget_name=%q}) or vector(0)`,
				testNs, gitTargetName),
			func(v float64) bool { return v > 0 },
			"the save's window has a collection time",
		)

		By("verifying the branch-target join series names this suite's GitTarget")
		waitForMetric(
			fmt.Sprintf(
				`sum(gitopsreverser_git_branch_targets{gittarget_namespace=%q,gittarget_name=%q,`+
					`gitbranch_namespace=%q,gitbranch_name=%q,source_cluster=%q}) `+
					`or vector(0)`,
				testNs, gitTargetName, testNs, gitBranchName(gitProvName, "main"),
				resolveE2ENamespace()+"/"+e2eKubeProviderName),
			func(v float64) bool { return v == 1 },
			"the join series maps this GitTarget to the GitBranch its commits are labelled by",
		)

		By("cleaning up the test Deployment and SaveRequest")
		_, _ = kubectlRunInNamespace(testNs, "delete", "deployment", deployName, "--ignore-not-found=true")
		_, _ = kubectlRunInNamespace(testNs, "delete", "saverequest", saveRequestName, "--ignore-not-found=true")
	})

	// The companion path: once a branch HAS been established (the previous spec's finalize created
	// main), a fresh edit must STILL be held in the open window — an existing branch must not
	// advance until a SaveRequest finalizes it. Together with the spec above this covers both
	// cases of "defer committing as long as possible": an absent branch and an established one.
	It("holds a new edit in the open window without advancing an already-established branch", func() {
		basePath := "e2e/save-request-test"
		seed := GinkgoRandomSeed()
		deployName := fmt.Sprintf("save-request-hold-%d", seed)
		saveRequestName := fmt.Sprintf("save-request-hold-save-%d", seed)
		message := fmt.Sprintf("save: held edit from e2e seed %d", seed)

		By("capturing the branch HEAD the previous spec established")
		var baseSHA string
		Eventually(func(g Gomega) {
			baseSHA = remoteBranchHead(g, repo.CheckoutDir)
			g.Expect(baseSHA).NotTo(BeEmpty(),
				"main must already exist from the previous spec's finalized commit")
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("creating a Deployment to open a new commit window on the existing branch")
		applyScaleTestDeployment(testNs, deployName, 0)

		By("confirming the branch HEAD does NOT advance while the window is open")
		Consistently(func(g Gomega) {
			g.Expect(remoteBranchHead(g, repo.CheckoutDir)).To(Equal(baseSHA),
				"the open commit window must hold the new edit; main must not advance until finalized")
		}, 10*time.Second, 2*time.Second).Should(Succeed())

		By("creating a SaveRequest and confirming the branch then advances with the held edit")
		applySaveRequest(testNs, saveRequestName, gitTargetName, message)
		Eventually(func(g Gomega) {
			expectSaveRequestCommitted(g, testNs, saveRequestName,
				recentCommitDiagnostics(repo.CheckoutDir, basePath))
			g.Expect(remoteBranchHead(g, repo.CheckoutDir)).NotTo(Equal(baseSHA),
				"finalizing must advance main past the previously-established HEAD")
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("verifying the previously-held edit is now present in Git")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			expected := filepath.Join(repo.CheckoutDir, basePath,
				fmt.Sprintf("%s/apps/deployments/%s.yaml", testNs, deployName))
			_, statErr := os.Stat(expected)
			g.Expect(statErr).NotTo(HaveOccurred(),
				fmt.Sprintf("the held Deployment should exist after finalize at %s", expected))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up the held Deployment and SaveRequest")
		_, _ = kubectlRunInNamespace(testNs, "delete", "deployment", deployName, "--ignore-not-found=true")
		_, _ = kubectlRunInNamespace(testNs, "delete", "saverequest", saveRequestName, "--ignore-not-found=true")
	})

	// generateName authorship is now captured at admission, not from the audit
	// response body: the API server assigns metadata.name and metadata.uid before
	// validating admission runs, so the validate-operator-types webhook records the
	// submitter keyed by uid with no response-body name recovery (the old audit
	// generateName headache is gone, see
	// docs/spec/saverequest-admission-authorship.md). This spec proves a
	// generateName SaveRequest still finalizes and becomes Ready. It is skipped in
	// configured-author mode, where the edit's window is committer-authored and the
	// named admission author would not match it end to end.
	It("finalizes a SaveRequest created with metadata.generateName", func() {
		basePath := "e2e/save-request-test"
		seed := GinkgoRandomSeed()
		deployName := fmt.Sprintf("save-request-gen-deploy-%d", seed)
		saveRequestPrefix := fmt.Sprintf("save-request-gen-%d-", seed)
		message := fmt.Sprintf("save: generateName save request from e2e seed %d", seed)

		By("creating a Deployment to open a commit window")
		applyScaleTestDeployment(testNs, deployName, 0)

		By("creating a SaveRequest with metadata.generateName")
		generatedName := applySaveRequestWithGenerateName(testNs, saveRequestPrefix, gitTargetName, message)

		By("waiting for the generated-name SaveRequest to commit")
		Eventually(func(g Gomega) {
			expectSaveRequestCommitted(g, testNs, generatedName,
				recentCommitDiagnostics(repo.CheckoutDir, basePath))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("verifying the commit landed in Git with the explicit message")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			expectedFile := filepath.Join(repo.CheckoutDir, basePath,
				fmt.Sprintf("%s/apps/deployments/%s.yaml", testNs, deployName))
			info, statErr := os.Stat(expectedFile)
			g.Expect(statErr).NotTo(HaveOccurred(), fmt.Sprintf("Deployment file should exist at %s", expectedFile))
			g.Expect(info.Size()).To(BeNumerically(">", 0))

			subject, logErr := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%B")
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(subject)).To(Equal(message),
				"the explicit spec.message should be used verbatim\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up the generateName Deployment and SaveRequest")
		_, _ = kubectlRunInNamespace(testNs, "delete", "deployment", deployName, "--ignore-not-found=true")
		_, _ = kubectlRunInNamespace(testNs, "delete", "saverequest", generatedName, "--ignore-not-found=true")
	})
})

// The UC2 suite exercises a `kubectl apply` bundle that includes a SaveRequest
// as its FIRST document — the deliberately-hard ordering where the save intent
// arrives before the work it is meant to save (docs/spec/saverequest-design.md). The request
// waits (spec.window.attachTimeout) for the first Deployment to open a window, attaches, and
// collects (spec.window.maxDuration from the attach) while the rest of the bundle arrives, so the
// whole bundle lands in ONE commit carrying the SaveRequest's message.
//
// Its own dedicated Gitea repo (own GitProvider → GitTarget → namespace-scoped
// Deployment WatchRule) makes the one-commit assertion unambiguous: main does not
// exist until the bundle is finalized, so the bundle commit is the only commit on
// main.
var _ = Describe("Save Request Bundle (UC2)", Label("save-request", "audit-consumer"), Ordered, func() {
	var (
		testNs        string
		repo          *RepoArtifacts
		gitProvName   string
		gitTargetName string
		watchRuleName string
	)

	// A long spec.commit.window so the silence timer can never be what produces the
	// commit — only the SaveRequest finalize may (A1).
	const commitWindow = "300s"

	BeforeAll(func() {
		p := setupWindowedDeploymentPipeline("save-request-bundle", "e2e/save-request-bundle", commitWindow)
		testNs, repo, gitProvName, gitTargetName, watchRuleName = p.ns, p.repo, p.gitProvider, p.gitTarget, p.watchRule
	})

	AfterAll(func() {
		cleanupPipeline(testNs, gitProvName, gitTargetName, watchRuleName)
		cleanupNamespace(testNs)
	})

	It("lands a kubectl-apply bundle (SaveRequest first) in one commit with the request's message", func() {
		basePath := "e2e/save-request-bundle"
		seed := GinkgoRandomSeed()
		message := fmt.Sprintf("bundle save: apply from e2e seed %d", seed)
		saveRequestName := fmt.Sprintf("save-request-bundle-save-%d", seed)
		deployNames := []string{
			fmt.Sprintf("bundle-deploy-a-%d", seed),
			fmt.Sprintf("bundle-deploy-b-%d", seed),
			fmt.Sprintf("bundle-deploy-c-%d", seed),
		}

		By("confirming nothing is committed yet — the branch does not exist")
		Consistently(func(g Gomega) {
			g.Expect(remoteBranchHead(g, repo.CheckoutDir)).To(BeEmpty(),
				"main must not exist before the bundle is finalized")
		}, 5*time.Second, 1*time.Second).Should(Succeed())

		By("applying a bundle whose FIRST document is a SaveRequest, then three Deployments")
		// The window is sized to comfortably exceed the bundle's per-type ingestion spread, so the
		// collection after the attach is deterministic.
		var bundle strings.Builder
		bundle.WriteString(saveRequestManifest(testNs, saveRequestName, gitTargetName, message, "8s"))
		for _, name := range deployNames {
			bundle.WriteString("---\n")
			bundle.WriteString(deploymentManifest(testNs, name))
		}
		_, err := kubectlRunWithStdin(testNs, bundle.String(), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "failed to apply the SaveRequest+Deployments bundle")

		By("waiting for the bundle's SaveRequest to commit the collected window")
		var reportedSHA string
		Eventually(func(g Gomega) {
			reportedSHA = expectSaveRequestCommitted(g, testNs, saveRequestName,
				recentCommitDiagnostics(repo.CheckoutDir, basePath))

			branch := saveRequestField(g, testNs, saveRequestName, "{.status.branch}")
			g.Expect(branch).To(Equal("main"))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("verifying the whole bundle landed in exactly one commit with the request's message")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			// Exactly one commit on the fresh repo's main: the entire bundle — applied
			// across the SaveRequest's attribution and the per-type Deployment stream —
			// collapsed into a single commit.
			g.Expect(mustCommitCount(repo.CheckoutDir)).To(Equal(1),
				"the whole bundle must land in exactly one commit\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))

			subject, logErr := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%B")
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(subject)).To(Equal(message),
				"the single commit must carry the SaveRequest's message verbatim\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))

			headSHA, shaErr := gitRun(repo.CheckoutDir, "rev-parse", "HEAD")
			g.Expect(shaErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(headSHA)).To(Equal(reportedSHA),
				"status.commit should match the single bundle commit\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))

			for _, name := range deployNames {
				expected := filepath.Join(repo.CheckoutDir, basePath,
					fmt.Sprintf("%s/apps/deployments/%s.yaml", testNs, name))
				_, statErr := os.Stat(expected)
				g.Expect(statErr).NotTo(HaveOccurred(),
					fmt.Sprintf("every bundled Deployment must be present in the one commit: %s", expected))
			}
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up the bundle Deployments and SaveRequest")
		for _, name := range deployNames {
			_, _ = kubectlRunInNamespace(testNs, "delete", "deployment", name, "--ignore-not-found=true")
		}
		_, _ = kubectlRunInNamespace(testNs, "delete", "saverequest", saveRequestName, "--ignore-not-found=true")
	})
})

// saveRequestManifest renders a single SaveRequest document with an explicit message and a
// window that waits and then collects for the given Go duration each. It is used to build
// multi-document `kubectl apply` bundles where the SaveRequest is the first document.
func saveRequestManifest(namespace, name, gitTargetName, message, window string) string {
	return fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: SaveRequest
metadata:
  name: %s
  namespace: %s
spec:
  gitTargetRef:
    name: %s
  message: %q
  window:
    attachTimeout: %q
    maxDuration: %q
`, name, namespace, gitTargetName, message, window, window)
}

// deploymentManifest renders a single zero-replica Deployment document for use in
// `kubectl apply` bundles. It mirrors applyScaleTestDeployment but returns the YAML
// instead of applying it.
func deploymentManifest(namespace, name string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 0
  selector:
    matchLabels:
      app.kubernetes.io/name: %s
  template:
    metadata:
      labels:
        app.kubernetes.io/name: %s
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.10
`, name, namespace, name, name)
}

// applySaveRequestWithGenerateName creates a SaveRequest using
// metadata.generateName and returns the server-allocated name. The spec creates its work and this
// SaveRequest back-to-back: authorship is settled synchronously at admission (no controller-side
// wait), so a zero wait would race the work's watch event and could resolve NoWindow before the
// window opens. Waiting to attach is the documented mechanism for a SaveRequest issued
// concurrently with its work (UC2).
//
// The wait is deliberately tight: it covers audit delivery (--audit-webhook-batch-max-wait=1s) and
// the join, within the 4s to 5s the configuration reference recommends for a loaded cluster, and
// stays well under --author-attribution-grace (10s here, config/deployment.yaml). A watch stream
// attributes its events in order, so a work event held behind another event that waits out the whole
// grace fails the spec with NoWindow instead of passing slowly. Do not widen it to make such a
// failure go away: that wait is the bug.
func applySaveRequestWithGenerateName(namespace, prefix, gitTargetName, message string) string {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: SaveRequest
metadata:
  generateName: %s
  namespace: %s
spec:
  gitTargetRef:
    name: %s
  message: %q
  window:
    attachTimeout: "4s"
    maxDuration: "4s"
`, prefix, namespace, gitTargetName, message)
	out, err := kubectlRunWithStdin(namespace, manifest,
		"create", "-f", "-", "-o", "jsonpath={.metadata.name}")
	Expect(err).NotTo(HaveOccurred(),
		fmt.Sprintf("failed to create SaveRequest with generateName=%s", prefix))
	name := strings.TrimSpace(out)
	Expect(name).NotTo(BeEmpty(), "kubectl create must return the server-allocated name")
	Expect(name).To(HavePrefix(prefix), "the allocated name must start with the requested prefix")
	return name
}

// applySaveRequest creates a SaveRequest object that targets the given
// GitTarget with an optional commit message.
func applySaveRequest(namespace, name, gitTargetName, message string) {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: SaveRequest
metadata:
  name: %s
  namespace: %s
spec:
  gitTargetRef:
    name: %s
  message: %q
`, name, namespace, gitTargetName, message)
	_, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to apply SaveRequest %s/%s", namespace, name))
}

// saveRequestField reads a jsonpath field off a SaveRequest object.
func saveRequestField(g Gomega, namespace, name, jsonPath string) string {
	out, err := kubectlRunInNamespace(namespace, "get", "saverequest", name,
		"-o", "jsonpath="+jsonPath)
	g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to read %s of SaveRequest %s", jsonPath, name))
	return strings.TrimSpace(out)
}

// saveRequestCondition reads the status of one condition off a SaveRequest.
func saveRequestCondition(g Gomega, namespace, name, conditionType string) string {
	return saveRequestField(g, namespace, name,
		fmt.Sprintf(`{.status.conditions[?(@.type=="%s")].status}`, conditionType))
}

// saveRequestReasonCommitted is the one Ready reason that means a commit actually happened.
// It mirrors crReasonCommitted in internal/controller/saverequest_finalize.go.
const saveRequestReasonCommitted = "Committed"

// saveRequestOutcome is the terminal shape of a SaveRequest: not just whether it finished,
// but WHICH ending it reached.
//
// Ready=True alone cannot answer that, and reading it as "it committed" is how this suite went
// blind. A benign rejection — no window collected in the grace, a foreign author's window, a
// change already present on the remote — is deliberately Ready=True with Pushed=False and an
// empty status.commit, so that kstatus reads Current rather than Failed (see rejectSaveRequest).
// A spec asserting Ready=True therefore PASSES on a request that committed nothing, then fails
// seconds later on the empty sha with "<string>: not to be empty" and no reason attached.
type saveRequestOutcome struct {
	Ready   string
	Reason  string
	Message string
	Stalled string
	Pushed  string
	Commit  string
	Branch  string
}

// String renders the outcome as the diagnostic the bare-sha assertion never produced.
func (o saveRequestOutcome) String() string {
	return fmt.Sprintf(
		"SaveRequest status: Ready=%s (reason=%q) Stalled=%s Pushed=%s commit=%q branch=%q\n  message: %s",
		o.Ready, o.Reason, o.Stalled, o.Pushed, o.Commit, o.Branch, o.Message)
}

// isTerminal reports whether the controller has stopped working on this request: Ready=True
// (committed or benignly rejected) or Stalled=True (failed). It mirrors saveRequestIsTerminal
// in the controller, and it is what makes an early give-up safe.
func (o saveRequestOutcome) isTerminal() bool {
	return o.Ready == "True" || o.Stalled == "True"
}

// readSaveRequestOutcome reads the whole terminal picture in one API call, so every field in a
// failure report is from the same observation rather than five reads a poll apart.
func readSaveRequestOutcome(g Gomega, namespace, name string) saveRequestOutcome {
	const readyPath = `{.status.conditions[?(@.type=="Ready")]`
	return saveRequestOutcome{
		Ready:   saveRequestField(g, namespace, name, readyPath+`.status}`),
		Reason:  saveRequestField(g, namespace, name, readyPath+`.reason}`),
		Message: saveRequestField(g, namespace, name, readyPath+`.message}`),
		Stalled: saveRequestCondition(g, namespace, name, "Stalled"),
		Pushed:  saveRequestCondition(g, namespace, name, "Pushed"),
		Commit:  saveRequestField(g, namespace, name, "{.status.commit}"),
		Branch:  saveRequestField(g, namespace, name, "{.status.branch}"),
	}
}

// expectSaveRequestCommitted asserts the request reached the COMMITTED ending, and returns the
// commit it reported.
//
// It gives up early on any other terminal ending rather than polling to the timeout. A terminal
// outcome is final — the controller will not revisit it — so continuing to re-read it cannot
// change the result; it only delays the report and buries the reason. Failing at the moment the
// ending is known means the message names it: "NoWindow" instead of an empty string.
func expectSaveRequestCommitted(g Gomega, namespace, name, diagnostics string) string {
	outcome := readSaveRequestOutcome(g, namespace, name)
	if outcome.isTerminal() && outcome.Reason != saveRequestReasonCommitted {
		StopTrying(fmt.Sprintf(
			"the SaveRequest reached a terminal outcome that is not a commit, so waiting longer "+
				"cannot help.\n%s\n%s", outcome, diagnostics)).Now()
	}

	g.Expect(outcome.Ready).To(Equal("True"), "SaveRequest is not Ready yet\n%s\n%s", outcome, diagnostics)
	g.Expect(outcome.Reason).To(Equal(saveRequestReasonCommitted),
		"Ready=True must mean a commit, not a benign rejection\n%s\n%s", outcome, diagnostics)
	g.Expect(outcome.Pushed).To(Equal("True"),
		"a committed SaveRequest must report Pushed=True\n%s", outcome)
	g.Expect(outcome.Commit).NotTo(BeEmpty(), "a committed SaveRequest must report status.commit\n%s", outcome)
	return outcome.Commit
}

// windowedDeploymentPipeline is the pipeline a SaveRequest suite saves through: a namespace and
// Gitea repo of its own, a GitProvider, a GitTarget with a long spec.commit.window, and a WatchRule
// on Deployments.
type windowedDeploymentPipeline struct {
	ns          string
	repo        *RepoArtifacts
	gitProvider string
	gitTarget   string
	watchRule   string
}

// setupWindowedDeploymentPipeline creates the pipeline, every name derived from name, and returns
// once its streams run: from then on a Deployment is a live, windowed event, and nothing is on main
// until a commit window is finalized.
func setupWindowedDeploymentPipeline(name, basePath, commitWindow string) windowedDeploymentPipeline {
	By("creating " + name + " test namespace and applying git secrets")
	p := windowedDeploymentPipeline{ns: testNamespaceFor(name)}
	_, _ = kubectlRun("create", "namespace", p.ns)
	p.repo = SetupRepo(resolveE2EContext(), p.ns, fmt.Sprintf("e2e-%s-%d", name, GinkgoRandomSeed()))
	_, err := kubectlRunInNamespace(p.ns, "apply", "-f", p.repo.SecretsYAML)
	Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to namespace")
	applySOPSAgeKeyToNamespace(p.ns)

	seed := GinkgoRandomSeed()
	p.gitProvider = fmt.Sprintf("%s-gitprovider-%d", name, seed)
	p.gitTarget = fmt.Sprintf("%s-gittarget-%d", name, seed)
	p.watchRule = fmt.Sprintf("%s-watchrule-%d", name, seed)

	createReadyGitProvider(p.gitProvider, p.ns, p.repo.GitSecretHTTP, p.repo.RepoURLHTTP)

	By(fmt.Sprintf("creating GitTarget with spec.commit.window=%s", commitWindow))
	createValidatedGitTargetWithCommitWindow(p.gitTarget, p.ns, p.gitProvider, basePath, commitWindow)

	// Watch Deployments: a fresh namespace contains NO Deployments, so the initial reconcile
	// commits nothing and cannot establish main before the spec's own edit, which would hide the
	// pure "branch not even created until something is finalized" behaviour these suites test.
	applyDeploymentWatchRule(p.ns, p.watchRule, p.gitTarget)
	verifyResourceStatus("watchrule", p.watchRule, p.ns, "True", "Succeeded", "")

	// Gate on StreamsRunning=True before any spec creates a Deployment: under watch-first, "Ready" only
	// means the watch set is reconciled, not that each watch has opened and finished its
	// sendInitialEvents replay. A Deployment created before the (empty) initial replay completes
	// is folded into that committer-authored reconcile and committed immediately — establishing
	// main and breaking the "branch not even created until the window is finalized" assertion.
	// StreamsRunning=True is the documented barrier that the object is a genuine live, windowed event.
	waitForStreamsRunning(p.gitTarget, p.ns)
	return p
}
