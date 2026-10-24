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

// This spec pins the bug a downstream consumer reported: every object mirrored through a DEDICATED
// KubeProvider on the operator's own cluster committed as "unknown (attribution unresolved)", while
// the same actor's writes through the install's provider attributed correctly.
//
// The cause is structural rather than stochastic. A kube-apiserver takes one
// --audit-webhook-config-file naming one server URL, so it posts audit under exactly ONE route (here
// /audit-webhook/<operator namespace>.local). Attribution facts are partitioned by that route, so a
// second KubeProvider on the same cluster reads a partition nothing writes unless it declares the
// same route via spec.attribution.auditRoute — and only a provider in the namespace holding that
// route may: one in another namespace reports AuditRouteConflict and is not used, so a tenant cannot
// read another namespace's facts by naming its route.
//
// Nothing else in the suite catches this. commit_author_attribution_e2e_test.go proves the OIDC
// claim chain, but only through the install's provider, which is the one route the apiserver feeds.
// source_namespace_e2e_test.go creates dedicated in-cluster providers but asserts Git paths, not
// commit authors. The gap between them is exactly where the loss lived.
//
// See docs/spec/attribution.md.
var _ = Describe("Audit route attribution", Label("manager"), Ordered, func() {
	const (
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
		dedicatedProv string
		quietProv     string
		borrowingProv string
		// fedRoute is the route the apiserver's webhook-config.yaml posts to, the only route that
		// carries facts on this cluster (test/e2e/cluster/audit/webhook-config.yaml): the default
		// route of the install's KubeProvider, which the operator's namespace therefore holds.
		fedRoute = resolveE2ENamespace() + "." + e2eKubeProviderName
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
		dedicatedProv = fmt.Sprintf("audit-route-kp-%d", seed)
		quietProv = fmt.Sprintf("audit-route-quiet-%d", seed)
		borrowingProv = fmt.Sprintf("audit-route-borrow-%d", seed)

		By("declaring a DEDICATED KubeProvider on the operator's own cluster that declares the fed route")
		// It reaches this cluster through a kubeconfig, as a second connection to one API server
		// does, and lives in the operator's namespace: that namespace holds the fed route, so it may
		// share it. Its own default route (<namespace>.<name>) is not the route the apiserver posts
		// to, which is the whole point: without spec.attribution.auditRoute it would read a partition
		// nothing writes. It also delegates source-namespace selection, so the second case below can
		// watch a namespace other than the rule's own, reproducing the reported shape exactly.
		writeKubeConfigSecret(resolveE2ENamespace(), dedicatedProv, "value", sameClusterKubeConfig())
		Expect(applyKubeProviderWithAuditRoute(resolveE2ENamespace(), dedicatedProv, dedicatedProv,
			[]string{testNs, sourceNs}, true, fedRoute)).Error().
			NotTo(HaveOccurred(), "failed to apply the dedicated KubeProvider")

		By("declaring a KubeProvider on an audit route NOBODY posts under")
		// The negative half of AuditFactsReceived, and the exact misconfiguration this whole spec
		// exists for: a provider that never sets spec.attribution.auditRoute reads its default
		// partition, <namespace>.<name>, which this apiserver never writes.
		writeKubeConfigSecret(testNs, "audit-route-kubeconfig", "value", sameClusterKubeConfig())
		Expect(applyKubeProviderWithAuditRoute(testNs, quietProv, "audit-route-kubeconfig",
			nil, false, "")).Error().NotTo(HaveOccurred(), "failed to apply the quiet KubeProvider")

		// A 0s commit window makes every watched event its own commit, so each assertion reads an
		// author scoped to its own file path and concurrent audit traffic cannot change it.
		createReadyGitProvider(gitProvName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)

		By("creating a GitTarget that mirrors through the dedicated provider")
		Expect(applyGitTargetForKubeProvider(
			testNs, gitTargetName, gitProvName, basePath, resolveE2ENamespace(), dedicatedProv)).Error().
			NotTo(HaveOccurred(), "failed to apply the GitTarget")
		verifyResourceCondition("gittarget", gitTargetName, testNs, "Validated", "True", "Succeeded", "")
	})

	AfterAll(func() {
		deleteKubeProvider(resolveE2ENamespace(), dedicatedProv)
		_, _ = kubectlRunInNamespace(resolveE2ENamespace(), "delete", "secret", dedicatedProv, "--ignore-not-found")
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
				"a commit through a dedicated KubeProvider must carry the real actor, not the "+
					"unresolved-attribution placeholder")
		}).Should(Succeed())

		_, _ = kubectlRunInNamespace(ns, "delete", "configmap", cmName, "--ignore-not-found=true")
	}

	// The quiet provider is asserted BEFORE any write, because that is the state the condition has
	// to report honestly: a route with no proof yet. It is a separate object from the mirroring one
	// on purpose — this apiserver feeds the fed route continuously, so the route the
	// mirroring provider declares is already latched by the time any spec here runs, and asserting
	// "no facts yet" against it would assert a race.
	It("says so when a KubeProvider's audit route has never carried a fact", func() {
		// RouteUnused, not one of the other two silences: this cluster's apiserver IS delivering
		// audit (every other spec here depends on it) and the transport is accepting writes, so the
		// only thing left to look at is this provider's own route. That is exactly the distinction
		// the three reasons exist to make.
		verifyResourceCondition("kubeprovider", quietProv, testNs,
			"AuditFactsReceived", "Unknown", "RouteUnused", testNs+"."+quietProv)

		// The whole reason this is not folded into Ready: nothing about the provider is broken.
		// Mirroring through it would work; only the commit AUTHOR would be lost.
		verifyResourceCondition("kubeprovider", quietProv, testNs, "Ready", "True", "", "")

		// And it stays Unknown rather than decaying to False on a timer: silence is not yet a
		// verdict, and no grace window can tell a misconfigured route from a quiet cluster.
		Consistently(func(g Gomega) {
			out, err := kubectlRunInNamespace(testNs, "get", "kubeprovider", quietProv,
				"-o", `jsonpath={.status.conditions[?(@.type=="AuditFactsReceived")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("Unknown"))
		}, 20*time.Second, 5*time.Second).Should(Succeed())
	})

	// A tenant cannot read the facts of a route another namespace holds by spelling it out: the
	// provider that tries is not used, and neither is anything that mirrors through it.
	It("does not use a KubeProvider that borrows another namespace's audit route", func() {
		Expect(applyKubeProviderWithAuditRoute(testNs, borrowingProv, "audit-route-kubeconfig",
			nil, false, fedRoute)).Error().NotTo(HaveOccurred(), "failed to apply the borrowing KubeProvider")
		DeferCleanup(func() { deleteKubeProvider(testNs, borrowingProv) })
		verifyResourceCondition("kubeprovider", borrowingProv, testNs, "Validated", "True", "Succeeded", "")
		verifyResourceCondition("kubeprovider", borrowingProv, testNs,
			"Stalled", "True", "AuditRouteConflict", resolveE2ENamespace())

		target := gitTargetName + "-borrowing"
		Expect(applyGitTargetForKubeProvider(testNs, target, gitProvName, basePath+"-borrowing",
			testNs, borrowingProv)).Error().NotTo(HaveOccurred())
		DeferCleanup(func() { cleanupNamespacedResource(testNs, "gittarget", target) })
		verifyResourceCondition("gittarget", target, testNs, "Validated", "False", "AuditRouteConflict", "")
	})

	It("attributes a commit mirrored through a dedicated KubeProvider", func() {
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

		// The fact that named that author was published on the declared route, so the provider
		// reading it latches True. This is the signal an operator gets in `kubectl get
		// kubeprovider` (the FactsReceived column) instead of having to read commit authors out of Git.
		verifyResourceCondition("kubeprovider", dedicatedProv, resolveE2ENamespace(),
			"AuditFactsReceived", "True", "Received", fedRoute)
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
		// Gate on THIS rule's streams, not the target's roll-up. The roll-up answers "is every
		// stream this target has running", which a target that was already mirroring answers True
		// to before the new rule's collection has been planned at all. What this spec depends on is the
		// new collection, and the rule-scoped summary is the thing that names it.
		waitForWatchRuleStreamsRunning(overrideRule, testNs)
		DeferCleanup(func() { cleanupWatchRule(overrideRule, testNs) })

		assertCommitAuthor(sourceNs, fmt.Sprintf("audit-route-src-cm-%d", GinkgoRandomSeed()),
			"oidc-override-user", "Override User", "override-user@configbutler.ai")
	})
})

// sameClusterKubeConfig is a kubeconfig for the cluster the operator runs in, addressed the way a
// Pod reaches it: a second connection to one API server, which is what a dedicated KubeProvider for
// the operator's own cluster is when the installation does not list it for spec.inCluster.
func sameClusterKubeConfig() string {
	GinkgoHelper()
	return rawKubeConfigWithServer("https://kubernetes.default.svc")
}

// applyKubeProviderWithAuditRoute applies a KubeProvider in namespace reaching its cluster through
// the kubeconfig Secret secretName in that namespace, letting its own namespace and the namespaces
// listed mirror their own namespace (every namespace when wide), and declaring auditRoute when it is
// not empty. The route is what lets several providers on one cluster share the single stream that
// cluster's apiserver posts.
func applyKubeProviderWithAuditRoute(
	namespace, name, secretName string, admits []string, wide bool, auditRoute string,
) (string, error) {
	scope := `["{consumer.namespace}"]`
	if wide {
		scope = `["{consumer.namespace}", "*"]`
	}
	extra := fmt.Sprintf("\n  allowedGitTargets:\n    - from: Same\n      sourceNamespaces: %s", scope)
	if len(admits) > 0 {
		extra += fmt.Sprintf("\n    - from: Names\n      names: [%s]\n      sourceNamespaces: %s",
			strings.Join(admits, ", "), scope)
	}
	if auditRoute != "" {
		extra += fmt.Sprintf("\n  attribution:\n    auditRoute: %s", auditRoute)
	}
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: KubeProvider
metadata:
  name: %s
  namespace: %s
spec:
  secretRef:
    name: %s%s
`, name, namespace, secretName, extra)
	return kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
}

// applyGitTargetForKubeProvider applies a GitTarget that mirrors through the KubeProvider
// providerNS/provider. Which SOURCE namespaces may reach it is bounded by that provider's
// allowedGitTargets[].sourceNamespaces plus the source credential's own RBAC, not by any field here.
func applyGitTargetForKubeProvider(
	ns, name, gitProvider, targetPath, providerNS, provider string,
) (string, error) {
	manifest := gitBranchManifest(ns, gitProvider) + fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: %s
  namespace: %s
spec:
  gitBranchRef:
    name: %s
  kubeProviderRef:
    name: %s
    namespace: %s
  path: %s
  commit:
    window:
      idleTimeout: "0s"
`, name, ns, gitBranchName(gitProvider, "main"), provider, providerNS, targetPath)
	return kubectlRunWithStdin(ns, manifest, "apply", "-f", "-")
}

// applyWatchRuleWithSourceNamespace applies a WatchRule whose rule items watch a namespace OTHER
// than the rule's own. sourceNamespace may be an exact name or "*".
func applyWatchRuleWithSourceNamespace(name, ns, target, sourceNamespace string) (string, error) {
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: %s
  namespace: %s
spec:
  gitTargetRef:
    name: %s
  rules:
    - resources: ["configmaps"]
      sourceNamespace: %q
    - resources: ["secrets"]
      sourceNamespace: %q
`, name, ns, target, sourceNamespace, sourceNamespace)
	return kubectlRunWithStdin(ns, manifest, "apply", "-f", "-")
}
