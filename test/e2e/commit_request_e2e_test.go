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
				"CommitRequest should finalize the window and report Committed")

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
				"the explicit spec.message should be used verbatim as the commit message")

			headSHA, shaErr := gitRun(repo.CheckoutDir, "rev-parse", "HEAD")
			g.Expect(shaErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(headSHA)).To(Equal(reportedSHA),
				"status.sha should match the SHA of the commit on the branch")
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
				"a CommitRequest created via generateName must reach Committed")

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
				"the explicit spec.message should be used verbatim")
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up the generateName Deployment and CommitRequest")
		_, _ = kubectlRunInNamespace(testNs, "delete", "deployment", deployName, "--ignore-not-found=true")
		_, _ = kubectlRunInNamespace(testNs, "delete", "commitrequest", generatedName, "--ignore-not-found=true")
	})
})

// applyCommitRequestWithGenerateName creates a CommitRequest using
// metadata.generateName and returns the server-allocated name.
func applyCommitRequestWithGenerateName(namespace, prefix, gitTargetName, message string) string {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha1
kind: CommitRequest
metadata:
  generateName: %s
  namespace: %s
spec:
  gitTargetRef:
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
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha1
kind: CommitRequest
metadata:
  name: %s
  namespace: %s
spec:
  gitTargetRef:
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
