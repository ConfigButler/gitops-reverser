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

// This spec proves the foreign-content stringency end to end
// (docs/spec/gitpath-foreign-content-stringency.md): a GitTarget path is an
// operator-exclusive subtree, so a loose non-YAML file the operator cannot manage is REFUSED
// (GitPathAccepted=False / UnsupportedContent / Stalled=True) with the offending file named,
// and the operator commits nothing. It reuses the GitPathAccepted refusal plumbing the
// unsupported-folder spec established. The escape hatch — a root .gittargetignore that lets
// the operator accept and write again once it names the passenger — is proven at the
// writer-integration level (TestWriter_IgnoredForeignFileAllowsWrite), which exercises it
// without the blocked-stream resync timing a refused GitTarget makes non-deterministic in e2e.
var _ = Describe("Manager Foreign Content Refusal", Label("manager", "foreign-content"), Ordered, func() {
	var (
		testNs       string
		repo         *RepoArtifacts
		providerName = "foreign-content-provider"
		destName     = "foreign-content-dest"
		ruleName     = "foreign-content-rule"
		gitPath      = "e2e/foreign-content"
	)

	const repoName = "e2e-foreign-content"

	BeforeAll(func() {
		By("creating the foreign-content test namespace")
		testNs = testNamespaceFor("manager-foreign-content")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		By("setting up Gitea repo and credentials")
		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("%s-%d", repoName, GinkgoRandomSeed()))

		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")

		// createGitTarget enables SOPS encryption referencing the shared sops-age-key secret,
		// so it must exist or EncryptionConfigured (and thus Ready) never goes True —
		// independent of the refusal under test.
		applySOPSAgeKeyToNamespace(testNs)

		By("creating the GitProvider")
		createGitProviderWithURLInNamespace(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")
	})

	AfterAll(func() {
		cleanupWatchRule(ruleName, testNs)
		cleanupGitTarget(destName, testNs)
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
		cleanupNamespace(testNs)
	})

	It("refuses a loose foreign file without writing", func() {
		By("seeding the Git repository with a foreign (non-YAML) file under the path")
		seedFolder := writeForeignContentFolder()
		DeferCleanup(func() { _ = os.RemoveAll(seedFolder) })
		seedRenderedFolderIntoRepo(repo, testNs, seedFolder, gitPath)

		// Capture the seed commit before the GitTarget exists, so any later operator commit is
		// detectable. The operator must add nothing on top of this.
		var seedHead string
		Eventually(func(g Gomega) {
			seedHead = remoteBranchHead(g, repo.CheckoutDir)
			g.Expect(seedHead).NotTo(BeEmpty(), "the seed commit must be on the remote")
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("creating the GitTarget and WatchRule pointed at the foreign-content path")
		createGitTarget(destName, testNs, providerName, gitPath, "main")
		err := applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", struct {
			Name            string
			Namespace       string
			DestinationName string
		}{Name: ruleName, Namespace: testNs, DestinationName: destName}, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to apply ConfigMap WatchRule")

		By("the path is refused, the GitTarget is stalled, and the offending file is named")
		waitForGitTargetGitPathRefused(destName, testNs, "UnsupportedContent")
		verifyResourceCondition("gittarget", destName, testNs,
			"GitPathAccepted", "False", "UnsupportedContent", "secrets.txt")

		By("the operator commits nothing on top of the refused path")
		Consistently(func(g Gomega) {
			g.Expect(remoteBranchHead(g, repo.CheckoutDir)).
				To(Equal(seedHead), "the operator must not commit into a refused path")
		}, 20*time.Second, 4*time.Second).Should(Succeed())

		By("foreign Git path content refused: GitTarget stalled, nothing written")
	})
})

// writeForeignContentFolder renders a temp folder holding a loose, non-YAML file
// (secrets.txt) the operator cannot manage. The folder carries no managed manifest of its
// own — the foreign file alone is enough for the operator-exclusive subtree to refuse the
// path. It also doubles as the encryption hole the design calls out: a plaintext secret
// dropped on disk the gate previously tolerated.
func writeForeignContentFolder() string {
	GinkgoHelper()

	dir, err := os.MkdirTemp("", "gitops-reverser-e2e-foreign-*")
	Expect(err).NotTo(HaveOccurred(), "failed to create foreign-content fixture directory")

	Expect(os.WriteFile(
		filepath.Join(dir, "secrets.txt"),
		[]byte("db-password=do-not-commit\n"), 0o600)).To(Succeed())
	return dir
}
