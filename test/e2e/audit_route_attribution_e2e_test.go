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

// This spec pins the bug the gitops-api team reported: every object mirrored through a DEDICATED
// in-cluster ClusterProvider committed as "unknown (attribution unresolved)", while the same actor's
// writes through "default" attributed correctly.
//
// The cause is structural rather than stochastic. A kube-apiserver takes one
// --audit-webhook-config-file naming one server URL, so it posts audit under exactly ONE route (here
// /audit-webhook/default). Attribution facts are partitioned by that route, so a second
// ClusterProvider on the same cluster reads a partition nothing writes unless it declares the same
// route via spec.attribution.auditRoute.
//
// Nothing else in the suite catches this. commit_author_attribution_e2e_test.go proves the OIDC
// claim chain, but only through the default provider, which is the one route the apiserver feeds.
// source_namespace_e2e_test.go creates dedicated in-cluster providers but asserts Git paths, not
// commit authors. The gap between them is exactly where the loss lived.
//
// See docs/design/attribution-fact-identity.md.
var _ = Describe("Audit route attribution", Label("manager"), Ordered, func() {
	const (
		// The apiserver's webhook-config.yaml posts to /audit-webhook/default, so this is the only
		// route that carries facts on this cluster (test/e2e/cluster/audit/webhook-config.yaml).
		fedRoute  = "default"
		basePath  = "e2e/audit-route-attribution"
		claimName = "configbutler.ai/claims/display-name"
		claimMail = "configbutler.ai/claims/email"
	)

	var (
		testNs        string
		sourceNs      string
		repo          *RepoArtifacts
		gitProvName   string
		gitTargetName string
		watchRuleName string
		clusterProv   string
	)

	BeforeAll(func() {
		if configuredAuthorModeEnabled() {
			Skip("watch-first configured-author mode has no audit facts for author attribution")
		}

		seed := GinkgoRandomSeed()
		testNs = testNamespaceFor("audit-route")
		sourceNs = testNamespaceFor("audit-route-src")
		_, _ = kubectlRun("create", "namespace", testNs)
		_, _ = kubectlRun("create", "namespace", sourceNs)

		By("setting up the Gitea repo and credentials")
		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-audit-route-%d", seed))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets")
		applySOPSAgeKeyToNamespace(testNs)

		gitProvName = fmt.Sprintf("audit-route-gitprovider-%d", seed)
		gitTargetName = fmt.Sprintf("audit-route-gittarget-%d", seed)
		watchRuleName = fmt.Sprintf("audit-route-watchrule-%d", seed)
		clusterProv = fmt.Sprintf("audit-route-cp-%d", seed)

		By("declaring a DEDICATED in-cluster ClusterProvider that declares the fed audit route")
		// kubeConfig is omitted, so this names the operator's own cluster, exactly as the reported
		// srcns-delegating provider did. Its NAME is not the route the apiserver posts to, which is
		// the whole point: without spec.attribution.auditRoute it would read a partition nothing
		// writes. It also delegates source-namespace selection, so the second case below can watch
		// a namespace other than the rule's own, reproducing the reported shape exactly.
		Expect(applyInClusterClusterProviderWithAuditRoute(
			clusterProv, testNs, sourceNs, true, fedRoute)).Error().
			NotTo(HaveOccurred(), "failed to apply the dedicated ClusterProvider")

		// A 0s commit window makes every watched event its own commit, so each assertion reads an
		// author scoped to its own file path and concurrent audit traffic cannot change it.
		createReadyGitProvider(gitProvName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)

		By("creating a GitTarget that mirrors through the dedicated provider")
		Expect(applyGitTargetWithSourceNamespaces(
			testNs, gitTargetName, gitProvName, basePath, clusterProv, testNs, sourceNs)).Error().
			NotTo(HaveOccurred(), "failed to apply the GitTarget")
		verifyResourceCondition("gittarget", gitTargetName, testNs, "Validated", "True", "Succeeded", "")
	})

	AfterAll(func() {
		deleteClusterProvider(clusterProv)
		cleanupPipeline(testNs, gitProvName, gitTargetName, watchRuleName)
		cleanupNamespace(testNs)
		cleanupNamespace(sourceNs)
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(3 * time.Second)

	// assertCommitAuthor creates a ConfigMap in ns while impersonating an identity carrying OIDC
	// claims, then asserts the commit for that object's own path is authored by it. On main before
	// this change the author is "unknown (attribution unresolved)", because the read went to the
	// provider's name rather than to the route the apiserver posts under.
	assertCommitAuthor := func(ns, cmName, asUser, displayName, email string) {
		GinkgoHelper()

		By(fmt.Sprintf("creating ConfigMap %q in %q as %q", cmName, ns, asUser))
		err := createConfigMapAsImpersonatedUser(ns, cmName, asUser,
			[]string{"system:masters"},
			map[string][]string{claimName: {displayName}, claimMail: {email}},
		)
		Expect(err).NotTo(HaveOccurred(), "failed to create the impersonated ConfigMap")

		repoPath := path.Join(basePath, fmt.Sprintf("%s/configmaps/%s.yaml", ns, cmName))
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			info, statErr := os.Stat(filepath.Join(repo.CheckoutDir, repoPath))
			g.Expect(statErr).NotTo(HaveOccurred(), "mirrored file should exist at %s", repoPath)
			g.Expect(info.Size()).To(BeNumerically(">", 0))

			out, logErr := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%an <%ae>", "--", repoPath)
			g.Expect(logErr).NotTo(HaveOccurred(), "git log author failed: %s", out)
			g.Expect(strings.TrimSpace(out)).To(Equal(fmt.Sprintf("%s <%s>", displayName, email)),
				"a commit through a dedicated ClusterProvider must carry the real actor, not the "+
					"unresolved-attribution placeholder")
		}).Should(Succeed())

		_, _ = kubectlRunInNamespace(ns, "delete", "configmap", cmName, "--ignore-not-found=true")
	}

	It("attributes a commit mirrored through a dedicated in-cluster ClusterProvider", func() {
		By("creating a WatchRule in the GitTarget's own namespace")
		data := struct{ Name, Namespace, DestinationName string }{
			Name: watchRuleName, Namespace: testNs, DestinationName: gitTargetName,
		}
		Expect(applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", data, testNs)).
			To(Succeed(), "failed to apply the WatchRule")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Succeeded", "")
		waitForStreamsRunning(gitTargetName, testNs)

		assertCommitAuthor(testNs, fmt.Sprintf("audit-route-cm-%d", GinkgoRandomSeed()),
			"oidc-route-user", "Route User", "route-user@configbutler.ai")
	})

	It("attributes a commit reached through a rules[].sourceNamespace override", func() {
		// The reported objects were mirrored through BOTH a dedicated provider and a sourceNamespace
		// override, and the report argued the override was innocent. This asserts that directly: the
		// override changes which namespace is watched and nothing about attribution.
		overrideRule := watchRuleName + "-override"
		Expect(applyWatchRuleWithSourceNamespace(overrideRule, testNs, gitTargetName, sourceNs)).Error().
			NotTo(HaveOccurred(), "failed to apply the overriding WatchRule")
		verifyResourceCondition("watchrule", overrideRule, testNs,
			"SourceNamespaceAuthorized", "True", "SourceNamespaceAllowed", "")
		waitForStreamsRunning(gitTargetName, testNs)
		DeferCleanup(func() { cleanupWatchRule(overrideRule, testNs) })

		assertCommitAuthor(sourceNs, fmt.Sprintf("audit-route-src-cm-%d", GinkgoRandomSeed()),
			"oidc-override-user", "Override User", "override-user@configbutler.ai")
	})
})

// applyInClusterClusterProviderWithAuditRoute applies a ClusterProvider that OMITS kubeConfig (so it
// names the operator's own cluster) and declares the audit route its facts arrive on. The route is
// what lets several providers on one cluster share the single stream that cluster's apiserver posts.
func applyInClusterClusterProviderWithAuditRoute(
	name, allowedNS, extraNS string, delegate bool, auditRoute string,
) (string, error) {
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: ClusterProvider
metadata:
  name: %s
spec:
  allowedNamespaces:
    names: [%s, %s]
  allowSourceNamespaceOverride: %t
  attribution:
    auditRoute: %s
`, name, allowedNS, extraNS, delegate, auditRoute)
	return kubectlRunWithStdin("", manifest, "apply", "-f", "-")
}
