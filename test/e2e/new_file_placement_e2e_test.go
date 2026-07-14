// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Validates new-file placement end-to-end: a brand-new resource with no
// existing document in Git — the "install something extra in test" launch use
// case (docs/spec/gittarget-new-file-placement-rules.md,
// docs/design/support-boundary/README.md) — lands inside the kustomize-managed overlay
// directory it belongs to, not the canonical GVR-tree path, and the overlay's
// kustomization.yaml gains the resources: entry so kustomize actually renders it.
var _ = Describe("Manager New-File Placement", Label("manager", "new-file-placement"), Ordered, func() {
	var (
		testNs       string
		repo         *RepoArtifacts
		providerName = "new-file-placement-provider"
		destName     = "new-file-placement-dest"
		ruleName     = "new-file-placement-rule"
		gitPath      = "e2e/new-file-placement"
	)

	const (
		fixtureRoot     = "test/e2e/fixtures/new-file-placement-folder"
		newConfigMap    = "debug-toolbox"
		newFileRepoPath = "debug-toolbox.yaml"
		kustRepoPath    = "kustomization.yaml"
		repoName        = "e2e-new-file-placement"
	)

	BeforeAll(func() {
		By("creating the new-file-placement test namespace")
		testNs = testNamespaceFor("manager-new-file-placement")
		_, _ = kubectlRun("create", "namespace", testNs)

		By("setting up Gitea repo and credentials")
		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("%s-%d", repoName, GinkgoRandomSeed()))

		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")

		applySOPSAgeKeyToNamespace(testNs)

		By("creating the GitProvider")
		createGitProviderWithURLInNamespace(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")
	})

	AfterAll(func() {
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", newConfigMap, "--ignore-not-found=true")
		cleanupWatchRule(ruleName, testNs)
		cleanupGitTarget(destName, testNs)
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
		cleanupNamespace(testNs)
	})

	It("places a brand-new resource inside its kustomize overlay and registers it in resources:", func() {
		renderedFixture := renderInPlaceFixtureFolder(fixtureRoot, testNs)
		DeferCleanup(func() { _ = os.RemoveAll(renderedFixture) })

		By("seeding the Git repository with the kustomize overlay fixture")
		seedRenderedFolderIntoRepo(repo, testNs, renderedFixture, gitPath)

		By("applying the rendered overlay with Kustomize")
		_, err := kubectlRunInNamespace(testNs, "apply", "-k", renderedFixture)
		Expect(err).NotTo(HaveOccurred(), "failed to apply rendered fixture kustomization")

		By("creating the GitTarget and ConfigMap WatchRule")
		createGitTarget(destName, testNs, providerName, gitPath, "main")
		err = applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", struct {
			Name            string
			Namespace       string
			DestinationName string
		}{Name: ruleName, Namespace: testNs, DestinationName: destName}, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to apply ConfigMap WatchRule")
		verifyResourceCondition("gittarget", destName, testNs, "Validated", "True", "OK", "")
		verifyResourceStatus("watchrule", ruleName, testNs, "True", "Ready", "")
		waitForStreamsRunning(destName, testNs)

		By("creating a brand-new ConfigMap with no existing document in Git")
		_, err = kubectlRunInNamespace(testNs, "create", "configmap", newConfigMap, "--from-literal=color=blue")
		Expect(err).NotTo(HaveOccurred(), "failed to create the new ConfigMap")

		By("verifying the new file landed in the overlay and the kustomization was updated")
		newFileFullPath := filepath.Join(repo.CheckoutDir, gitPath, newFileRepoPath)
		kustFullPath := filepath.Join(repo.CheckoutDir, gitPath, kustRepoPath)
		canonicalPath := filepath.Join(repo.CheckoutDir, gitPath, testNs, "configmaps", newConfigMap+".yaml")

		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			newFileBody := readRepoFile(g, newFileFullPath)
			g.Expect(newFileBody).To(ContainSubstring("name: " + newConfigMap))
			g.Expect(newFileBody).To(ContainSubstring("color: blue"))
			g.Expect(newFileBody).NotTo(ContainSubstring("namespace:"),
				"the overlay's kustomization sets namespace:, so the new file must not repeat it")

			kustBody := readRepoFile(g, kustFullPath)
			g.Expect(kustBody).To(ContainSubstring("- deployment.yaml"), "the existing entry must survive")
			g.Expect(kustBody).To(ContainSubstring("- " + newFileRepoPath))

			_, statErr := os.Stat(canonicalPath)
			g.Expect(os.IsNotExist(statErr)).
				To(BeTrue(), "must not also create a canonical-path duplicate %s", canonicalPath)
		}, 120*time.Second, 3*time.Second).Should(Succeed())

		By("✅ new resource placed inside the kustomize overlay and registered in resources:")
	})
})
