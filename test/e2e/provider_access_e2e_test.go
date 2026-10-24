// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"path"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Access between objects (docs/configuration.md): a platform keeps one repository credential and
// lets labelled tenant namespaces claim branches under their own prefix (GitProvider
// spec.allowedGitBranches). A tenant's GitBranch decides who writes it (spec.allowedGitTargets), so
// the tenant shares it with another team without asking the platform. Claims on main, and on
// another team's prefix, are refused with messages that name the grant.
var _ = Describe("Manager branch grants", Label("manager"), Ordered, func() {
	const (
		providerName = "shared-repo"
		tenantLabel  = "e2e.configbutler.ai/tenant"
	)

	var (
		platformNs, teamA, teamB string
		repo                     *RepoArtifacts
	)

	teamBranch := func(team string) string { return "teams/" + team + "/live" }

	BeforeAll(func() {
		platformNs = testNamespaceFor("grants-platform")
		teamA = testNamespaceFor("grants-team-a")
		teamB = testNamespaceFor("grants-team-b")
		for _, ns := range []string{platformNs, teamA, teamB} {
			_, _ = kubectlRun("create", "namespace", ns) // idempotent; ignore AlreadyExists
		}
		for _, ns := range []string{teamA, teamB} {
			_, err := kubectlRun("label", "namespace", ns, tenantLabel+"=true", "--overwrite")
			Expect(err).NotTo(HaveOccurred())
		}

		By("setting up the repository, with its credential in the platform namespace only")
		repo = SetupRepo(resolveE2EContext(), platformNs, fmt.Sprintf("e2e-branch-grants-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(platformNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to the platform namespace")

		By("creating the platform's GitProvider: main for itself, teams/{namespace}/* for tenants")
		_, err = kubectlRunWithStdin("", fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitProvider
metadata:
  name: %s
  namespace: %s
spec:
  url: %s
  secretRef:
    name: %s
  allowedGitBranches:
    - from: Same
      branches: [main]
    - from: Selector
      selector:
        matchLabels:
          %s: "true"
      branches: ["teams/{namespace}/*"]
`, providerName, platformNs, repo.RepoURLHTTP, repo.GitSecretHTTP, tenantLabel), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		verifyResourceStatus("gitprovider", providerName, platformNs,
			"True", "Succeeded", "Repository connectivity validated")
	})

	AfterAll(func() {
		for _, ns := range []string{teamA, teamB, platformNs} {
			cleanupNamespace(ns)
		}
	})

	SetDefaultEventuallyTimeout(90 * time.Second)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	// applyTenantGitBranch applies a GitBranch in namespace claiming branch through the platform's
	// GitProvider, written by its own namespace and the namespaces in sharedWith.
	applyTenantGitBranch := func(name, namespace, branch string, sharedWith ...string) {
		GinkgoHelper()
		_, err := kubectlRunWithStdin("", fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitBranch
metadata:
  name: %s
  namespace: %s
spec:
  gitProviderRef:
    name: %s
    namespace: %s
  branch: %s
  allowedGitTargets:%s
`, name, namespace, providerName, platformNs, branch, gitBranchWritersYAML(sharedWith...)), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "failed to apply GitBranch %s/%s", namespace, name)
	}

	// expectFileOnBranch waits until file is in the tip of branch on the remote.
	expectFileOnBranch := func(branch, file string) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			_, err := gitRun(repo.CheckoutDir, "fetch", "origin",
				"+refs/heads/"+branch+":refs/remotes/origin/"+branch)
			g.Expect(err).NotTo(HaveOccurred())
			_, err = gitRun(repo.CheckoutDir, "cat-file", "-e", "origin/"+branch+":"+file)
			g.Expect(err).NotTo(HaveOccurred(), "%s is on branch %s", file, branch)
		}).Should(Succeed())
	}

	It("lets a tenant claim a branch under its own prefix and write it", func() {
		applyTenantGitBranch("live", teamA, teamBranch(teamA))
		verifyResourceStatus("gitbranch", "live", teamA, "True", "Succeeded", "holds")

		applySharedGitTarget("apps", teamA, "live", "", "team-a/apps")
		verifyResourceCondition("gittarget", "apps", teamA, "Validated", "True", "Succeeded", "")
		applyIsolationWatchRule("apps-configmaps", teamA, "apps", `"configmaps"`)
		waitForStreamsRunning("apps", teamA)
		applyIsolationConfigMap("grants-cm", teamA)

		expectFileOnBranch(teamBranch(teamA), path.Join("team-a/apps", teamA, "configmaps", "grants-cm.yaml"))

		out, err := kubectlRunInNamespace(teamA, "get", "secrets", "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(ContainSubstring(repo.GitSecretHTTP), "the tenant namespace holds no copy of the credential")
	})

	It("refuses a tenant's claims on main and on another team's branch", func() {
		applyTenantGitBranch("main", teamA, "main")
		verifyResourceCondition("gitbranch", "main", teamA, "Stalled", "True", "BranchNotAllowed",
			`requested branch "main" is not allowed`)
		verifyResourceCondition("gitbranch", "main", teamA, "Ready", "False", "BranchNotAllowed",
			fmt.Sprintf(`namespace %q may claim "teams/%s/*"`, teamA, teamA))

		applyTenantGitBranch("other", teamA, teamBranch(teamB))
		verifyResourceCondition("gitbranch", "other", teamA, "Stalled", "True", "BranchNotAllowed",
			fmt.Sprintf(`requested branch %q is not allowed`, teamBranch(teamB)))
	})

	It("lets the tenant share its branch with a second team, without the platform", func() {
		applySharedGitTarget("shared", teamB, "live", teamA, "team-b/apps")
		verifyResourceCondition("gittarget", "shared", teamB, "Validated", "False", "NamespaceNotAuthorized",
			fmt.Sprintf("GitBranch %s/live admits %s in spec.allowedGitTargets", teamA, teamA))

		By("team A adds a from: Names grant for team B; the GitProvider does not change")
		applyTenantGitBranch("live", teamA, teamBranch(teamA), teamB)
		verifyResourceCondition("gittarget", "shared", teamB, "Validated", "True", "Succeeded", "")
		applyIsolationWatchRule("shared-configmaps", teamB, "shared", `"configmaps"`)
		waitForStreamsRunning("shared", teamB)
		applyIsolationConfigMap("shared-cm", teamB)

		expectFileOnBranch(teamBranch(teamA), path.Join("team-b/apps", teamB, "configmaps", "shared-cm.yaml"))
	})
})
