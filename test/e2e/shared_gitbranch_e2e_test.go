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

// One GitBranch per repository branch, shared across namespaces (docs/architecture.md,
// "GitBranch"). A GitBranch claims its branch once its GitProvider's credential opens a push
// session, and holds it for every GitTarget that references it: those in other namespaces are
// admitted by the GitBranch's spec.allowedGitTargets and write with its GitProvider's credential, so
// the tenant namespace holds no copy of it. A second GitBranch for the same repository branch, however it
// spells the repository, holds nothing and reports BranchConflict; one with a bad credential
// claims nothing at all, so creating it first blocks nobody.
var _ = Describe("Manager shared GitBranch", Label("manager"), Ordered, func() {
	const (
		providerName = "shared-monorepo"
		branchName   = "monorepo-main"
	)

	var (
		platformNs, tenantNs, outsiderNs string
		repo                             *RepoArtifacts
	)

	BeforeAll(func() {
		platformNs = testNamespaceFor("manager-shared-platform")
		tenantNs = testNamespaceFor("manager-shared-tenant")
		outsiderNs = testNamespaceFor("manager-shared-outsider")
		for _, ns := range []string{platformNs, tenantNs, outsiderNs} {
			_, _ = kubectlRun("create", "namespace", ns) // idempotent; ignore AlreadyExists
		}

		By("setting up the repository, with its credential in the platform namespace only")
		repo = SetupRepo(resolveE2EContext(), platformNs, fmt.Sprintf("e2e-manager-shared-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(platformNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to the platform namespace")

		By("creating the GitProvider, and its GitBranch admitting the tenant namespace")
		applySharedGitProvider(providerName, platformNs, repo.RepoURLHTTP, repo.GitSecretHTTP)
		verifyResourceStatus("gitprovider", providerName, platformNs,
			"True", "Succeeded", "Repository connectivity validated")
		applySharedGitBranch(branchName, platformNs, providerName, "main", tenantNs)
		verifyResourceStatus("gitbranch", branchName, platformNs, "True", "Succeeded", "holds")
	})

	AfterAll(func() {
		for _, ns := range []string{tenantNs, outsiderNs, platformNs} {
			cleanupNamespace(ns)
		}
	})

	SetDefaultEventuallyTimeout(60 * time.Second)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("mirrors GitTargets in two namespaces through one GitBranch, without copying the Secret", func() {
		applySharedGitTarget("platform-apps", platformNs, branchName, "", "platform/apps")
		applySharedGitTarget("apps", tenantNs, branchName, platformNs, "tenant/apps")
		verifyResourceCondition("gittarget", "apps", tenantNs, "Validated", "True", "Succeeded", "")
		verifyResourceCondition("gittarget", "apps", tenantNs, "GitBranchReady", "True", "Succeeded", "")

		out, err := kubectlRunInNamespace(tenantNs, "get", "secrets", "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(ContainSubstring(repo.GitSecretHTTP), "the tenant namespace holds no copy of the credential")

		applyIsolationWatchRule("platform-configmaps", platformNs, "platform-apps", `"configmaps"`)
		applyIsolationWatchRule("apps-configmaps", tenantNs, "apps", `"configmaps"`)
		waitForStreamsRunning("platform-apps", platformNs)
		waitForStreamsRunning("apps", tenantNs)

		By("creating a ConfigMap in each namespace")
		applyIsolationConfigMap("shared-cm", platformNs)
		applyIsolationConfigMap("shared-cm", tenantNs)

		files := []string{
			filepath.Join(repo.CheckoutDir, path.Join("platform/apps", platformNs, "configmaps", "shared-cm.yaml")),
			filepath.Join(repo.CheckoutDir, path.Join("tenant/apps", tenantNs, "configmaps", "shared-cm.yaml")),
		}
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			for _, file := range files {
				_, err := os.Stat(file)
				g.Expect(err).NotTo(HaveOccurred(), "both folders are written through the one GitBranch")
			}
		}).Should(Succeed())

		Eventually(func(g Gomega) {
			out, err := kubectlRunInNamespace(platformNs, "get", "gitbranch", branchName,
				"-o", "jsonpath={.status.gitTargetCount}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("2"))
		}).Should(Succeed())
	})

	It("refuses a namespace the GitBranch does not admit", func() {
		applySharedGitTarget("intruder", outsiderNs, branchName, platformNs, "outsider")
		verifyResourceCondition("gittarget", "intruder", outsiderNs,
			"Validated", "False", "NamespaceNotAuthorized", "spec.allowedGitTargets")
	})

	It("holds the branch against a second GitBranch, even through another GitProvider", func() {
		By("giving the tenant a GitProvider of its own, with a valid credential, for the same repository")
		_, err := kubectlRunInNamespace(tenantNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred())
		applySharedGitProvider("own-monorepo", tenantNs, repo.RepoURLHTTP, repo.GitSecretHTTP)
		applySharedGitBranch("own-main", tenantNs, "own-monorepo", "main", "")

		verifyResourceCondition("gitbranch", "own-main", tenantNs,
			"Stalled", "True", "BranchConflict", fmt.Sprintf("%s/%s", platformNs, branchName))
		verifyResourceStatus("gitbranch", branchName, platformNs, "True", "Succeeded", "holds")

		applySharedGitTarget("own", tenantNs, "own-main", "", "tenant/own")
		verifyResourceCondition("gittarget", "own", tenantNs,
			"GitBranchReady", "False", "GitBranchNotReady", "does not hold its branch")
	})

	It("does not let a GitBranch with a bad credential, created first, block a valid one", func() {
		By("creating a GitBranch for the release branch whose GitProvider's credential is wrong")
		_, err := kubectlRunInNamespace(outsiderNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred())
		applySharedGitProvider("squat", outsiderNs, repo.RepoURLHTTP, repo.GitSecretInvalid)
		applySharedGitBranch("squat-release", outsiderNs, "squat", "release", "")
		verifyResourceStatus("gitbranch", "squat-release", outsiderNs, "False", "PushSessionFailed", "")

		By("creating a valid GitBranch for the same branch afterwards")
		applySharedGitBranch("monorepo-release", platformNs, providerName, "release", "")
		verifyResourceStatus("gitbranch", "monorepo-release", platformNs, "True", "Succeeded", "holds")
	})

	It("lets a deleted GitBranch go, and leaves its GitTargets reporting it missing", func() {
		_, err := kubectlRunInNamespace(platformNs, "delete", "gitbranch", "monorepo-release", "--wait=true",
			"--timeout=60s")
		Expect(err).NotTo(HaveOccurred(), "the finalizer gives the branch up once its worker has stopped")

		By("the squatter, still unproven, does not inherit the branch")
		verifyResourceStatus("gitbranch", "squat-release", outsiderNs, "False", "PushSessionFailed", "")

		_, err = kubectlRunInNamespace(platformNs, "delete", "gitbranch", branchName, "--wait=true", "--timeout=120s")
		Expect(err).NotTo(HaveOccurred())
		verifyResourceCondition("gittarget", "platform-apps", platformNs,
			"Validated", "False", "GitBranchNotFound", "not found")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			g.Expect(remoteHeadSHA(g, repo.CheckoutDir)).NotTo(BeEmpty(), "the remote branch is never deleted")
		}).Should(Succeed())
	})
})

// applySharedGitProvider applies a GitProvider for url with secret, letting GitBranches in its own
// namespace claim main and release.
func applySharedGitProvider(name, namespace, url, secret string) {
	By(fmt.Sprintf("creating GitProvider '%s/%s'", namespace, name))
	_, err := kubectlRunWithStdin("", fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitProvider
metadata:
  name: %s
  namespace: %s
spec:
  url: %s
  allowedGitBranches:
    - from: Same
      branches: ["main", "release"]
  secretRef:
    name: %s
`, name, namespace, url, secret), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply GitProvider %s/%s", namespace, name)
}

// applySharedGitBranch applies a GitBranch writing branch through the GitProvider provider, in its
// own namespace. GitTargets in its own namespace may write through it, and in sharedWith when set.
func applySharedGitBranch(name, namespace, provider, branch, sharedWith string) {
	By(fmt.Sprintf("creating GitBranch '%s/%s' for branch '%s' through GitProvider '%s'",
		namespace, name, branch, provider))
	var shared []string
	if sharedWith != "" {
		shared = []string{sharedWith}
	}
	_, err := kubectlRunWithStdin("", fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitBranch
metadata:
  name: %s
  namespace: %s
spec:
  gitProviderRef:
    name: %s
  branch: %s
  allowedGitTargets:%s
`, name, namespace, provider, branch, gitBranchWritersYAML(shared...)), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply GitBranch %s/%s", namespace, name)
}

// applySharedGitTarget applies a GitTarget writing path through the GitBranch branchNS/branchName;
// an empty branchNS names its own namespace.
func applySharedGitTarget(name, namespace, branchName, branchNS, targetPath string) {
	By(fmt.Sprintf("creating GitTarget '%s/%s' on GitBranch '%s/%s'", namespace, name, branchNS, branchName))
	ref := "name: " + branchName
	if branchNS != "" {
		ref += "\n    namespace: " + branchNS
	}
	_, err := kubectlRunWithStdin("", fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: %s
  namespace: %s
spec:
  gitBranchRef:
    %s
%s
  path: %s
  commit:
    window:
      idleTimeout: "%s"
      maxDuration: "%s"
`, name, namespace, ref, localKubeProviderRef(), targetPath, e2eCommitWindow, e2eCommitMaxDuration),
		"apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply GitTarget %s/%s", namespace, name)
}
