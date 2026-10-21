// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// One GitProvider per repository branch, shared across namespaces (docs/architecture.md,
// "GitProvider"). A GitProvider admits other namespaces through spec.accessFrom, and their
// GitTargets name it with spec.gitProviderRef.namespace: they write with its credential, so the
// tenant namespace holds no copy of it. A namespace it does not admit is refused, and so is a
// GitTarget that writes the same repository branch through a GitProvider of its own: one branch
// worker writes the branch, with one credential.
var _ = Describe("Manager shared GitProvider", Label("manager"), Ordered, func() {
	const providerName = "shared-monorepo"

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

		By("creating the GitProvider, admitting the tenant namespace")
		_, err = kubectlRunWithStdin("", fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitProvider
metadata:
  name: %s
  namespace: %s
spec:
  url: %s
  allowedBranches: ["main"]
  secretRef:
    name: %s
  accessFrom:
    names: [%s]
`, providerName, platformNs, repo.RepoURLHTTP, repo.GitSecretHTTP, tenantNs), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "failed to apply the shared GitProvider")
		verifyResourceStatus("gitprovider", providerName, platformNs,
			"True", "Succeeded", "Repository connectivity validated")
	})

	AfterAll(func() {
		for _, ns := range []string{tenantNs, outsiderNs, platformNs} {
			cleanupNamespace(ns)
		}
	})

	SetDefaultEventuallyTimeout(60 * time.Second)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("mirrors a GitTarget in another namespace through the shared GitProvider", func() {
		applySharedGitTarget("apps", tenantNs, providerName, platformNs, "tenant/apps")
		verifyResourceCondition("gittarget", "apps", tenantNs, "Validated", "True", "Succeeded", "")

		applyIsolationWatchRule("apps-configmaps", tenantNs, "apps", `"configmaps"`)
		waitForStreamsRunning("apps", tenantNs)

		By("creating a ConfigMap in the tenant namespace")
		applyIsolationConfigMap("shared-cm", tenantNs)

		file := filepath.Join(repo.CheckoutDir, path.Join("tenant/apps", tenantNs, "configmaps", "shared-cm.yaml"))
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			_, err := os.Stat(file)
			g.Expect(err).NotTo(HaveOccurred(), "the ConfigMap is written with the shared GitProvider's credential")
		}).Should(Succeed())
	})

	It("refuses a namespace the GitProvider does not admit", func() {
		applySharedGitTarget("intruder", outsiderNs, providerName, platformNs, "outsider")
		verifyResourceCondition("gittarget", "intruder", outsiderNs,
			"Validated", "False", "NamespaceNotAuthorized", "spec.accessFrom")
	})

	It("refuses a second GitProvider on the same repository branch", func() {
		By("giving the tenant a GitProvider of its own for the same repository")
		// No credential: which GitProvider owns the branch is decided from the URL alone.
		_, err := kubectlRunWithStdin("", fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitProvider
metadata:
  name: own-monorepo
  namespace: %s
spec:
  url: %s
  allowedBranches: ["main"]
`, tenantNs, repo.RepoURLHTTP), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "failed to apply the tenant's own GitProvider")

		applySharedGitTarget("own", tenantNs, "own-monorepo", "", "tenant/own")
		verifyResourceCondition("gittarget", "own", tenantNs,
			"Validated", "False", "TargetConflict", fmt.Sprintf("GitProvider '%s/%s'", platformNs, providerName))
	})
})

// applySharedGitTarget applies a GitTarget writing path on main through the GitProvider
// providerNS/providerName; an empty providerNS names its own namespace.
func applySharedGitTarget(name, namespace, providerName, providerNS, targetPath string) {
	By(fmt.Sprintf("creating GitTarget '%s/%s' through GitProvider '%s/%s'", namespace, name, providerNS, providerName))
	ref := "name: " + providerName
	if providerNS != "" {
		ref += "\n    namespace: " + providerNS
	}
	_, err := kubectlRunWithStdin("", fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: %s
  namespace: %s
spec:
  gitProviderRef:
    %s
  branch: main
  path: %s
  commit:
    window:
      idleTimeout: "%s"
      maxDuration: "%s"
`, name, namespace, ref, targetPath, e2eCommitWindow, e2eCommitMaxDuration), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply GitTarget %s/%s", namespace, name)
}
