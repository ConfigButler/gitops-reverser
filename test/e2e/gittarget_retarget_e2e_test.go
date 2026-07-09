// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A GitTarget's spec.path and spec.branch are mutable: changing either is a supported
// retarget. The controller tears the old materialization down — watches, event stream, and
// the durable resume cursors — and rebuilds the folder from a fresh full snapshot at the new
// destination.
//
// Three things have to hold, and only an e2e can show them together:
//
//  1. The new folder receives the resources that ALREADY existed, not just the ones that
//     change afterwards. That is what dropping the resume cursors buys; without it a
//     retargeted GitTarget resumes mid-stream into an empty folder.
//  2. The old folder is left in place. Deleting from Git is the one irreversible thing this
//     operator does, and a destination change is the moment an operator is least sure of
//     what they meant.
//  3. status.observedDestination follows the content, not the spec: it is written only once
//     a snapshot has actually landed at the new destination.
//
// Not Serial: the spec owns a dedicated Gitea repo, so nothing else writes its branch.
var _ = Describe("GitTarget retarget", Label("manager"), Ordered, func() {
	var (
		testNs    string
		repo      *RepoArtifacts
		provider  string
		target    string
		watchRule string
	)

	const (
		originalPath = "e2e/retarget/before"
		movedPath    = "e2e/retarget/after"
		cmName       = "retargeted-configmap"
	)

	BeforeAll(func() {
		By("creating the retarget namespace and Git credentials")
		testNs = testNamespaceFor("retarget")
		_, _ = kubectlRun("create", "namespace", testNs)
		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-retarget-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to namespace")
		applySOPSAgeKeyToNamespace(testNs)

		seed := GinkgoRandomSeed()
		provider = fmt.Sprintf("retarget-provider-%d", seed)
		target = fmt.Sprintf("retarget-target-%d", seed)
		watchRule = fmt.Sprintf("retarget-rule-%d", seed)

		createGitProviderWithURLInNamespace(provider, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		verifyResourceStatus("gitprovider", provider, testNs, "True", "Ready", "Repository connectivity validated")

		createGitTarget(target, testNs, provider, originalPath, "main")
		verifyResourceCondition("gittarget", target, testNs, "Validated", "True", "OK", "")

		rule := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: %s
  namespace: %s
spec:
  targetRef:
    kind: GitTarget
    name: %s
  rules:
    - resources: ["configmaps"]
`, watchRule, testNs, target)
		_, err = kubectlRunWithStdin(testNs, rule, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "failed to apply WatchRule")
		verifyResourceStatus("watchrule", watchRule, testNs, "True", "Ready", "")
		waitForStreamsRunning(target, testNs)
	})

	AfterAll(func() {
		cleanupWatchRule(watchRule, testNs)
		cleanupGitTarget(target, testNs)
		cleanupNamespace(testNs)
	})

	observedPath := func() string {
		GinkgoHelper()
		out, err := kubectlRunInNamespace(testNs, "get", "gittarget", target,
			"-o", "jsonpath={.status.observedDestination.path}")
		Expect(err).NotTo(HaveOccurred())
		return strings.TrimSpace(out)
	}

	It("rebuilds the folder at the new path, leaves the old one, and follows with observedDestination", func() {
		beforeRel := path.Join(originalPath, fmt.Sprintf("%s/configmaps/%s.yaml", testNs, cmName))
		afterRel := path.Join(movedPath, fmt.Sprintf("%s/configmaps/%s.yaml", testNs, cmName))
		beforeAbs := filepath.Join(repo.CheckoutDir, beforeRel)
		afterAbs := filepath.Join(repo.CheckoutDir, afterRel)

		By("creating a ConfigMap that lands in the original folder")
		_, err := kubectlRunInNamespace(testNs, "create", "configmap", cmName, "--from-literal=flavor=vanilla")
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			content, readErr := os.ReadFile(beforeAbs)
			g.Expect(readErr).NotTo(HaveOccurred(), "the ConfigMap must be committed at %s", beforeRel)
			g.Expect(string(content)).To(ContainSubstring("flavor: vanilla"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("status.observedDestination names the folder the content is actually in")
		Eventually(observedPath, 90*time.Second, 2*time.Second).Should(Equal(originalPath))

		By("repointing spec.path — which used to be rejected by the API server")
		_, err = kubectlRunInNamespace(testNs, "patch", "gittarget", target, "--type=merge",
			"--patch", fmt.Sprintf(`{"spec":{"path":%q}}`, movedPath))
		Expect(err).NotTo(HaveOccurred(), "spec.path must be mutable: changing it is a supported retarget")

		By("the pre-existing ConfigMap appears in the new folder from a fresh full snapshot")
		// Nothing changed the ConfigMap after the move. If the resume cursors survived the
		// retarget, the new folder would stay empty forever — which is the whole reason the
		// teardown drops them.
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			content, readErr := os.ReadFile(afterAbs)
			g.Expect(readErr).NotTo(HaveOccurred(), "the retargeted folder must be rebuilt at %s", afterRel)
			g.Expect(string(content)).To(ContainSubstring("flavor: vanilla"))
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("the abandoned folder is left in place, as unmanaged Git content")
		_, statErr := os.Stat(beforeAbs)
		Expect(statErr).NotTo(HaveOccurred(),
			"a retarget must never delete the old folder: %s should still exist", beforeRel)

		By("observedDestination follows the content, and Retargeting settles")
		Eventually(observedPath, 2*time.Minute, 2*time.Second).Should(Equal(movedPath))
		verifyResourceCondition("gittarget", target, testNs,
			"Retargeting", "False", "DestinationSettled", "was abandoned")

		By("a live edit now lands in the new folder")
		_, err = kubectlRunInNamespace(testNs, "patch", "configmap", cmName, "--type=merge",
			"--patch", `{"data":{"flavor":"chocolate"}}`)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			content, readErr := os.ReadFile(afterAbs)
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(string(content)).To(ContainSubstring("flavor: chocolate"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
	})

	// The repository is a different object, with nothing to migrate and nothing to observe.
	It("still rejects a providerRef change, and says which fields are mutable", func() {
		out, err := kubectlRunInNamespace(testNs, "patch", "gittarget", target, "--type=merge",
			"--patch", `{"spec":{"providerRef":{"name":"some-other-provider"}}}`)
		Expect(err).To(HaveOccurred(), "spec.providerRef must stay immutable")
		Expect(out).To(ContainSubstring("spec.providerRef is immutable"))
		Expect(out).To(ContainSubstring("spec.branch and spec.path are mutable"),
			"the refusal must point at the supported move rather than just refusing")
	})
})
