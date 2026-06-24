/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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

// The CommitRequest suite exercises the "save" signal: a CommitRequest
// object finalizes the open commit window for a GitTarget immediately, instead
// of waiting for the rolling silence timer. The GitProvider is configured with
// a deliberately long commitWindow so that, without the CommitRequest, the
// edit would not be committed for minutes — observing the commit promptly is
// what proves the commit-request path works.
//
// Not Serial: this spec owns a dedicated Gitea repo (its own GitProvider →
// GitTarget → namespace-scoped WatchRule), so the only writer to its main
// branch is its own GitTarget, fed exclusively by audit events from its own
// namespace. The HEAD/SHA assertions below therefore read back only this spec's
// own commit; concurrent audit traffic for other GitTargets lands in other
// repos and cannot move this repo's HEAD. See docs/design/e2e-serial-registry.md.
var _ = Describe("Commit Request", Label("commit-request", "audit-consumer"), Ordered, func() {
	var (
		testNs        string
		repo          *RepoArtifacts
		gitProvName   string
		gitTargetName string
		watchRuleName string
	)

	// commitWindow is long enough that the silence timer cannot be what
	// produces the commit within the assertion timeout below.
	const commitWindow = "300s"

	BeforeAll(func() {
		By("creating commit-request test namespace and applying git secrets")
		testNs = testNamespaceFor("commit-request")
		_, _ = kubectlRun("create", "namespace", testNs)
		repo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-commit-request-%d", GinkgoRandomSeed()),
		)
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to namespace")
		applySOPSAgeKeyToNamespace(testNs)

		seed := GinkgoRandomSeed()
		gitProvName = fmt.Sprintf("commit-request-gitprovider-%d", seed)
		gitTargetName = fmt.Sprintf("commit-request-gittarget-%d", seed)
		watchRuleName = fmt.Sprintf("commit-request-watchrule-%d", seed)

		By(fmt.Sprintf("creating GitProvider with commitWindow=%s", commitWindow))
		createGitProviderWithCommitWindow(gitProvName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP, commitWindow)
		verifyResourceStatus("gitprovider", gitProvName, testNs, "True", "Ready", "")

		createGitTarget(gitTargetName, testNs, gitProvName, "e2e/commit-request-test", "main")
		verifyResourceStatus("gittarget", gitTargetName, testNs, "True", "Ready", "")

		// Watch Deployments, not ConfigMaps: a fresh namespace contains NO Deployments, whereas every
		// namespace is pre-populated with a kube-root-ca.crt ConfigMap that a configmaps WatchRule
		// would match — its initial reconcile would establish main before the spec's own edit, hiding
		// the pure "branch not even created until something is finalized" behaviour this suite tests.
		applyDeploymentWatchRule(testNs, watchRuleName, gitTargetName)
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
	})

	AfterAll(func() {
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(gitTargetName, testNs)
		cleanupNamespacedResource(testNs, "gitprovider", gitProvName)
		cleanupNamespace(testNs)
	})

	It("finalizes the open commit window on demand and reports the resulting SHA", func() {
		basePath := "e2e/commit-request-test"
		seed := GinkgoRandomSeed()
		deployName := fmt.Sprintf("commit-request-deploy-%d", seed)
		commitRequestName := fmt.Sprintf("commit-request-save-%d", seed)
		message := fmt.Sprintf("save: commit request from e2e seed %d", seed)

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

		By("creating a CommitRequest to finalize the open window now")
		applyCommitRequest(testNs, commitRequestName, gitTargetName, message)

		By("waiting for the CommitRequest to reach the Committed phase")
		var reportedSHA string
		Eventually(func(g Gomega) {
			phase := commitRequestField(g, testNs, commitRequestName, "{.status.phase}")
			g.Expect(phase).To(Equal("Committed"),
				"CommitRequest should finalize the window and report Committed\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))

			reportedSHA = commitRequestField(g, testNs, commitRequestName, "{.status.sha}")
			g.Expect(reportedSHA).NotTo(BeEmpty(), "status.sha should be populated")

			branch := commitRequestField(g, testNs, commitRequestName, "{.status.branch}")
			g.Expect(branch).To(Equal("main"))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("verifying the commit landed in Git with the explicit message and the Deployment manifest")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			expectedFile := filepath.Join(repo.CheckoutDir, basePath,
				fmt.Sprintf("apps/v1/deployments/%s/%s.yaml", testNs, deployName))
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
				"status.sha should match the SHA of the commit on the branch\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))

			// Exactly one new commit (UC1, §8.1): this is the first commit on a fresh
			// repo, so main holds exactly one commit — the save did not also trigger a
			// stray second commit.
			g.Expect(mustCommitCount(repo.CheckoutDir)).To(Equal(1),
				"the save must produce exactly one commit on main\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up the test Deployment and CommitRequest")
		_, _ = kubectlRunInNamespace(testNs, "delete", "deployment", deployName, "--ignore-not-found=true")
		_, _ = kubectlRunInNamespace(testNs, "delete", "commitrequest", commitRequestName, "--ignore-not-found=true")
	})

	// The companion path: once a branch HAS been established (the previous spec's finalize created
	// main), a fresh edit must STILL be held in the open window — an existing branch must not
	// advance until a CommitRequest finalizes it. Together with the spec above this covers both
	// cases of "defer committing as long as possible": an absent branch and an established one.
	It("holds a new edit in the open window without advancing an already-established branch", func() {
		basePath := "e2e/commit-request-test"
		seed := GinkgoRandomSeed()
		deployName := fmt.Sprintf("commit-request-hold-%d", seed)
		commitRequestName := fmt.Sprintf("commit-request-hold-save-%d", seed)
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

		By("creating a CommitRequest and confirming the branch then advances with the held edit")
		applyCommitRequest(testNs, commitRequestName, gitTargetName, message)
		Eventually(func(g Gomega) {
			phase := commitRequestField(g, testNs, commitRequestName, "{.status.phase}")
			g.Expect(phase).To(Equal("Committed"), "CommitRequest should finalize the open window")
			g.Expect(remoteBranchHead(g, repo.CheckoutDir)).NotTo(Equal(baseSHA),
				"finalizing must advance main past the previously-established HEAD")
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("verifying the previously-held edit is now present in Git")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			expected := filepath.Join(repo.CheckoutDir, basePath,
				fmt.Sprintf("apps/v1/deployments/%s/%s.yaml", testNs, deployName))
			_, statErr := os.Stat(expected)
			g.Expect(statErr).NotTo(HaveOccurred(),
				fmt.Sprintf("the held Deployment should exist after finalize at %s", expected))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up the held Deployment and CommitRequest")
		_, _ = kubectlRunInNamespace(testNs, "delete", "deployment", deployName, "--ignore-not-found=true")
		_, _ = kubectlRunInNamespace(testNs, "delete", "commitrequest", commitRequestName, "--ignore-not-found=true")
	})

	// generateName creates surface the bug described in
	// docs/tasks/generated-name-support.md: the audit objectRef.name is empty
	// for collection POSTs, so the consumer must resolve the generated name
	// from responseObject. Without the fix the CommitRequest stays stuck in
	// WaitingForAuditEvent forever.
	It("finalizes a CommitRequest created with metadata.generateName", func() {
		basePath := "e2e/commit-request-test"
		seed := GinkgoRandomSeed()
		deployName := fmt.Sprintf("commit-request-gen-deploy-%d", seed)
		commitRequestPrefix := fmt.Sprintf("commit-request-gen-%d-", seed)
		message := fmt.Sprintf("save: generateName commit request from e2e seed %d", seed)

		By("creating a Deployment to open a commit window")
		applyScaleTestDeployment(testNs, deployName, 0)

		By("creating a CommitRequest with metadata.generateName")
		generatedName := applyCommitRequestWithGenerateName(testNs, commitRequestPrefix, gitTargetName, message)

		By("waiting for the generated-name CommitRequest to reach Committed")
		var reportedSHA string
		Eventually(func(g Gomega) {
			phase := commitRequestField(g, testNs, generatedName, "{.status.phase}")
			g.Expect(phase).To(Equal("Committed"),
				"a CommitRequest created via generateName must reach Committed\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))

			reportedSHA = commitRequestField(g, testNs, generatedName, "{.status.sha}")
			g.Expect(reportedSHA).NotTo(BeEmpty(), "status.sha should be populated")
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("verifying the commit landed in Git with the explicit message")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			expectedFile := filepath.Join(repo.CheckoutDir, basePath,
				fmt.Sprintf("apps/v1/deployments/%s/%s.yaml", testNs, deployName))
			info, statErr := os.Stat(expectedFile)
			g.Expect(statErr).NotTo(HaveOccurred(), fmt.Sprintf("Deployment file should exist at %s", expectedFile))
			g.Expect(info.Size()).To(BeNumerically(">", 0))

			subject, logErr := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%B")
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(subject)).To(Equal(message),
				"the explicit spec.message should be used verbatim\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up the generateName Deployment and CommitRequest")
		_, _ = kubectlRunInNamespace(testNs, "delete", "deployment", deployName, "--ignore-not-found=true")
		_, _ = kubectlRunInNamespace(testNs, "delete", "commitrequest", generatedName, "--ignore-not-found=true")
	})
})

// The UC2 suite exercises a `kubectl apply` bundle that includes a CommitRequest
// as its FIRST document — the deliberately-hard ordering where the save intent
// arrives before the work it is meant to save (docs/design/stream/commitrequest-design.md
// §2 UC2, §6.2, §8.2). A non-zero spec.delaySeconds is the collect-grace that
// lets the bundle's resources arrive and join the same window after the
// CommitRequest is attributed, so the whole bundle lands in ONE commit carrying
// the CommitRequest's message.
//
// Its own dedicated Gitea repo (own GitProvider → GitTarget → namespace-scoped
// Deployment WatchRule) makes the one-commit assertion unambiguous: main does not
// exist until the bundle is finalized, so the bundle commit is the only commit on
// main.
var _ = Describe("Commit Request Bundle (UC2)", Label("commit-request", "audit-consumer"), Ordered, func() {
	var (
		testNs        string
		repo          *RepoArtifacts
		gitProvName   string
		gitTargetName string
		watchRuleName string
	)

	// A long commitWindow so the silence timer can never be what produces the
	// commit — only the CommitRequest finalize may (A1).
	const commitWindow = "300s"

	BeforeAll(func() {
		By("creating commit-request-bundle test namespace and applying git secrets")
		testNs = testNamespaceFor("commit-request-bundle")
		_, _ = kubectlRun("create", "namespace", testNs)
		repo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-commit-request-bundle-%d", GinkgoRandomSeed()),
		)
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to namespace")
		applySOPSAgeKeyToNamespace(testNs)

		seed := GinkgoRandomSeed()
		gitProvName = fmt.Sprintf("commit-request-bundle-gitprovider-%d", seed)
		gitTargetName = fmt.Sprintf("commit-request-bundle-gittarget-%d", seed)
		watchRuleName = fmt.Sprintf("commit-request-bundle-watchrule-%d", seed)

		By(fmt.Sprintf("creating GitProvider with commitWindow=%s", commitWindow))
		createGitProviderWithCommitWindow(gitProvName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP, commitWindow)
		verifyResourceStatus("gitprovider", gitProvName, testNs, "True", "Ready", "")

		createGitTarget(gitTargetName, testNs, gitProvName, "e2e/commit-request-bundle", "main")
		verifyResourceStatus("gittarget", gitTargetName, testNs, "True", "Ready", "")

		// Deployments only (no ConfigMaps): a fresh namespace has no Deployments, so
		// main stays absent until the bundle is finalized — unlike a ConfigMap rule,
		// which would mirror the pre-existing kube-root-ca.crt and establish main early.
		applyDeploymentWatchRule(testNs, watchRuleName, gitTargetName)
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
	})

	AfterAll(func() {
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(gitTargetName, testNs)
		cleanupNamespacedResource(testNs, "gitprovider", gitProvName)
		cleanupNamespace(testNs)
	})

	It("lands a kubectl-apply bundle (CommitRequest first) in one commit with the request's message", func() {
		basePath := "e2e/commit-request-bundle"
		seed := GinkgoRandomSeed()
		message := fmt.Sprintf("bundle save: apply from e2e seed %d", seed)
		commitRequestName := fmt.Sprintf("commit-request-bundle-save-%d", seed)
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

		By("applying a bundle whose FIRST document is a CommitRequest, then three Deployments")
		// delaySeconds is sized to comfortably exceed the bundle's per-type ingestion
		// spread so the collect-grace (§6.2) is deterministic.
		var bundle strings.Builder
		bundle.WriteString(commitRequestManifest(testNs, commitRequestName, gitTargetName, message, 8))
		for _, name := range deployNames {
			bundle.WriteString("---\n")
			bundle.WriteString(deploymentManifest(testNs, name))
		}
		_, err := kubectlRunWithStdin(testNs, bundle.String(), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "failed to apply the CommitRequest+Deployments bundle")

		By("waiting for the CommitRequest to reach the Committed phase")
		var reportedSHA string
		Eventually(func(g Gomega) {
			phase := commitRequestField(g, testNs, commitRequestName, "{.status.phase}")
			g.Expect(phase).To(Equal("Committed"),
				"the bundle's CommitRequest should finalize the collected window and report Committed\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))

			reportedSHA = commitRequestField(g, testNs, commitRequestName, "{.status.sha}")
			g.Expect(reportedSHA).NotTo(BeEmpty(), "status.sha should be populated")

			branch := commitRequestField(g, testNs, commitRequestName, "{.status.branch}")
			g.Expect(branch).To(Equal("main"))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("verifying the whole bundle landed in exactly one commit with the request's message")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			// Exactly one commit on the fresh repo's main: the entire bundle — applied
			// across the CommitRequest's attribution and the per-type Deployment stream —
			// collapsed into a single commit (§8.2 step 4).
			g.Expect(mustCommitCount(repo.CheckoutDir)).To(Equal(1),
				"the whole bundle must land in exactly one commit\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))

			subject, logErr := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%B")
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(subject)).To(Equal(message),
				"the single commit must carry the CommitRequest's message verbatim\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))

			headSHA, shaErr := gitRun(repo.CheckoutDir, "rev-parse", "HEAD")
			g.Expect(shaErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(headSHA)).To(Equal(reportedSHA),
				"status.sha should match the single bundle commit\n%s",
				recentCommitDiagnostics(repo.CheckoutDir, basePath))

			for _, name := range deployNames {
				expected := filepath.Join(repo.CheckoutDir, basePath,
					fmt.Sprintf("apps/v1/deployments/%s/%s.yaml", testNs, name))
				_, statErr := os.Stat(expected)
				g.Expect(statErr).NotTo(HaveOccurred(),
					fmt.Sprintf("every bundled Deployment must be present in the one commit: %s", expected))
			}
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up the bundle Deployments and CommitRequest")
		for _, name := range deployNames {
			_, _ = kubectlRunInNamespace(testNs, "delete", "deployment", name, "--ignore-not-found=true")
		}
		_, _ = kubectlRunInNamespace(testNs, "delete", "commitrequest", commitRequestName, "--ignore-not-found=true")
	})
})

// commitRequestManifest renders a single CommitRequest document with an explicit
// message and delaySeconds (the collect-grace). It is used to build multi-document
// `kubectl apply` bundles where the CommitRequest is the first document.
func commitRequestManifest(namespace, name, gitTargetName, message string, delaySeconds int) string {
	return fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha2
kind: CommitRequest
metadata:
  name: %s
  namespace: %s
spec:
  targetRef:
    name: %s
  message: %q
  delaySeconds: %d
`, name, namespace, gitTargetName, message, delaySeconds)
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

// applyCommitRequestWithGenerateName creates a CommitRequest using
// metadata.generateName and returns the server-allocated name.
func applyCommitRequestWithGenerateName(namespace, prefix, gitTargetName, message string) string {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha2
kind: CommitRequest
metadata:
  generateName: %s
  namespace: %s
spec:
  targetRef:
    name: %s
  message: %q
`, prefix, namespace, gitTargetName, message)
	out, err := kubectlRunWithStdin(namespace, manifest,
		"create", "-f", "-", "-o", "jsonpath={.metadata.name}")
	Expect(err).NotTo(HaveOccurred(),
		fmt.Sprintf("failed to create CommitRequest with generateName=%s", prefix))
	name := strings.TrimSpace(out)
	Expect(name).NotTo(BeEmpty(), "kubectl create must return the server-allocated name")
	Expect(name).To(HavePrefix(prefix), "the allocated name must start with the requested prefix")
	return name
}

// applyCommitRequest creates a CommitRequest object that targets the given
// GitTarget with an optional commit message.
func applyCommitRequest(namespace, name, gitTargetName, message string) {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha2
kind: CommitRequest
metadata:
  name: %s
  namespace: %s
spec:
  targetRef:
    name: %s
  message: %q
`, name, namespace, gitTargetName, message)
	_, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to apply CommitRequest %s/%s", namespace, name))
}

// commitRequestField reads a jsonpath field off a CommitRequest object.
func commitRequestField(g Gomega, namespace, name, jsonPath string) string {
	out, err := kubectlRunInNamespace(namespace, "get", "commitrequest", name,
		"-o", "jsonpath="+jsonPath)
	g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to read %s of CommitRequest %s", jsonPath, name))
	return strings.TrimSpace(out)
}
