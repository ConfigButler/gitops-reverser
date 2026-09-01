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

// This spec covers WatchRule.spec.rules[].sourceNamespace end to end (documented in
// docs/configuration.md and docs/security-model.md).
//
// The load-bearing assertion is the GIT PATH one. The whole design rests on a claim that is
// invisible from the API types: sourceNamespace changes which namespace is WATCHED and nothing
// about where objects are WRITTEN, because Git placement is rebuilt from each mirrored object's own
// metadata.namespace rather than from any config-plane namespace. If that ever regresses, a rule in
// tenant-* would start filing another namespace's objects under its own folder — silently, and in a
// tenant's repository. Nothing else in the suite would catch it.
//
// The refusal spec is its safety twin: an unauthorized override must publish a terminal condition
// AND write nothing at all. The wildcard spec is the third, and it is the one whose MEANING
// changed: "*" is now one cluster-wide watch bounded by the source credential's RBAC, so it reaches
// a namespace no rule item and no policy ever named. That widening is the thing to keep asserted,
// because nothing in the API shape shows it.
var _ = Describe("WatchRule source namespace", Label("manager"), Ordered, func() {
	const (
		providerName = "gitprovider-srcns"
		// Two GitTargets: one whose provider delegates the override, one whose provider does not.
		delegatingCP    = "srcns-delegating"
		nonDelegatingCP = "srcns-non-delegating"
		grantedTarget   = "srcns-granted"
		refusedTarget   = "srcns-refused"
		grantedRule     = "srcns-granted-rule"
		refusedRule     = "srcns-refused-rule"
		wildcardRule    = "srcns-wildcard-rule"
		grantedPath     = "e2e/srcns-granted"
		refusedPath     = "e2e/srcns-refused"
	)

	var (
		// configNS holds the WatchRules and GitTargets; sourceNS is the namespace they WATCH. The
		// two differ on purpose — that separation is the entire feature. wildcardNS is named by NO
		// rule item, so only a wildcard can reach it. outsideNS is named by nothing either, and
		// exists to prove that "*" really is cluster-wide rather than quietly enumerating.
		configNS   string
		sourceNS   string
		wildcardNS string
		outsideNS  string
		srcnsRepo  *RepoArtifacts
	)

	BeforeAll(func() {
		By("creating separate config-plane, source, and two unnamed namespaces")
		configNS = testNamespaceFor("srcns-config")
		sourceNS = testNamespaceFor("srcns-source")
		wildcardNS = testNamespaceFor("srcns-wildcard")
		outsideNS = testNamespaceFor("srcns-outside")
		_, _ = kubectlRun("create", "namespace", configNS)
		_, _ = kubectlRun("create", "namespace", sourceNS)
		_, _ = kubectlRun("create", "namespace", wildcardNS)
		_, _ = kubectlRun("create", "namespace", outsideNS)

		By("setting up Gitea repo and credentials")
		srcnsRepo = SetupRepo(
			resolveE2EContext(),
			configNS,
			fmt.Sprintf("e2e-srcns-%d", GinkgoRandomSeed()),
		)
		_, err := kubectlRunInNamespace(configNS, "apply", "-f", srcnsRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets")
		applySOPSAgeKeyToNamespace(configNS)

		createGitProviderWithURLInNamespace(
			providerName, configNS, srcnsRepo.GitSecretHTTP, srcnsRepo.RepoURLHTTP)
		verifyResourceStatus("gitprovider", providerName, configNS,
			"True", "Succeeded", "Repository connectivity validated")

		By("declaring two in-cluster ClusterProviders that differ ONLY in the delegation flag")
		// Both omit kubeConfig, so both name the operator's own cluster: this is deliberately the
		// SHARPER of the two cases the design describes, where an authorized override bypasses live
		// namespace RBAC. Dedicated providers keep the shared "default" one untouched.
		Expect(applyInClusterClusterProvider(delegatingCP, configNS, true)).Error().
			NotTo(HaveOccurred(), "failed to apply delegating ClusterProvider")
		Expect(applyInClusterClusterProvider(nonDelegatingCP, configNS, false)).Error().
			NotTo(HaveOccurred(), "failed to apply non-delegating ClusterProvider")

		By("creating one GitTarget behind the delegating provider and one behind the refusing one")
		// The targets are identical. The only thing that differs is which ClusterProvider they
		// mirror through, which is now the whole of the source-namespace policy: there is no
		// per-target allow-list any more.
		Expect(applyGitTargetForClusterProvider(
			configNS, grantedTarget, providerName, grantedPath, delegatingCP)).Error().
			NotTo(HaveOccurred(), "failed to apply granted GitTarget")
		Expect(applyGitTargetForClusterProvider(
			configNS, refusedTarget, providerName, refusedPath, nonDelegatingCP)).Error().
			NotTo(HaveOccurred(), "failed to apply refused GitTarget")

		verifyResourceCondition("gittarget", grantedTarget, configNS, "Validated", "True", "Succeeded", "")
		verifyResourceCondition("gittarget", refusedTarget, configNS, "Validated", "True", "Succeeded", "")
	})

	AfterAll(func() {
		deleteClusterProvider(delegatingCP)
		deleteClusterProvider(nonDelegatingCP)
		cleanupNamespace(configNS)
		cleanupNamespace(sourceNS)
		cleanupNamespace(wildcardNS)
		cleanupNamespace(outsideNS)
	})

	SetDefaultEventuallyTimeout(60 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("mirrors an authorized source namespace under THAT namespace's Git folder", func() {
		By("creating a WatchRule whose rule item watches the source namespace")
		Expect(applyWatchRuleWithSourceNamespace(
			grantedRule, configNS, grantedTarget, sourceNS)).Error().
			NotTo(HaveOccurred(), "failed to apply granted WatchRule")

		By("asserting the override is authorized")
		verifyResourceCondition("watchrule", grantedRule, configNS,
			"SourceNamespaceAuthorized", "True", "SourceNamespaceAllowed", "")
		verifyResourceStatus("watchrule", grantedRule, configNS, "True", "Succeeded", "")
		waitForStreamsRunning(grantedTarget, configNS)

		By("creating a ConfigMap in the SOURCE namespace")
		const cmName = "srcns-mirrored"
		_, err := kubectlRunInNamespace(sourceNS, "create", "configmap", cmName,
			"--from-literal=hello=world")
		Expect(err).NotTo(HaveOccurred(), "failed to create ConfigMap in the source namespace")

		By("asserting it lands under the SOURCE namespace's folder, not the config namespace's")
		// This is the appendix's claim, asserted: the write path never substitutes the WatchRule's
		// control-plane namespace for the mirrored object's own.
		wantPath := path.Join(grantedPath, fmt.Sprintf("%s/configmaps/%s.yaml", sourceNS, cmName))
		mustNotExist := path.Join(grantedPath, fmt.Sprintf("%s/configmaps/%s.yaml", configNS, cmName))

		Eventually(func(g Gomega) {
			pullLatestRepoState(g, srcnsRepo.CheckoutDir)

			g.Expect(filepath.Join(srcnsRepo.CheckoutDir, wantPath)).To(BeAnExistingFile(),
				"a rule in %q watching %q must write under %q — Git placement follows the MIRRORED "+
					"OBJECT's namespace, never the WatchRule's. Recent commits:\n%s",
				configNS, sourceNS, sourceNS, recentCommitDiagnostics(srcnsRepo.CheckoutDir, grantedPath))

			g.Expect(filepath.Join(srcnsRepo.CheckoutDir, mustNotExist)).NotTo(BeAnExistingFile(),
				"the config-plane namespace %q must never name a Git folder", configNS)
		}).Should(Succeed())
	})

	It("reaches every namespace the credential can read through one cluster-wide watch", func() {
		By("creating a WatchRule whose item asks for every namespace")
		// ConfigMaps only, deliberately. A cluster-wide "*" mirrors everything the credential can
		// read, and adding secrets here would file every service-account token in the cluster into
		// the fixture repository. That the widening reaches that far is the POINT of the change;
		// one type is enough to observe it.
		Expect(applyWildcardConfigMapWatchRule(
			wildcardRule, configNS, grantedTarget)).Error().
			NotTo(HaveOccurred(), "failed to apply wildcard WatchRule")

		By("asserting the wildcard is authorized")
		// The gate is what this spec is about. Whole-target Ready is deliberately NOT asserted: a
		// cluster-wide mirror of a live k3d cluster is a throughput property, not a statement about
		// "*", and gating on it would make this spec fail for reasons that have nothing to do with
		// the semantics it pins.
		verifyResourceCondition("watchrule", wildcardRule, configNS,
			"SourceNamespaceAuthorized", "True", "SourceNamespaceAllowed", "")

		By("creating ConfigMaps in two namespaces that NOTHING names")
		// Neither namespace is named by a rule item, and there is no target policy that could have
		// enumerated them. Under the previous meaning of "*" — every namespace the GitTarget's
		// allowedSourceNamespaces admitted — both of these would have been out of reach. Both
		// arriving is the redefinition, observed rather than asserted from the types.
		const wildcardCM = "srcns-wildcard-unnamed"
		const outsideCM = "srcns-wildcard-outside"
		_, err := kubectlRunInNamespace(wildcardNS, "create", "configmap", wildcardCM,
			"--from-literal=k=v")
		Expect(err).NotTo(HaveOccurred())
		_, err = kubectlRunInNamespace(outsideNS, "create", "configmap", outsideCM,
			"--from-literal=k=v")
		Expect(err).NotTo(HaveOccurred())

		By("asserting both arrive, each under its own namespace's folder")
		wantWildcard := path.Join(grantedPath, fmt.Sprintf("%s/configmaps/%s.yaml", wildcardNS, wildcardCM))
		wantOutside := path.Join(grantedPath, fmt.Sprintf("%s/configmaps/%s.yaml", outsideNS, outsideCM))
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, srcnsRepo.CheckoutDir)
			g.Expect(filepath.Join(srcnsRepo.CheckoutDir, wantWildcard)).To(BeAnExistingFile(),
				`"*" must reach %q, which no rule item names. Recent commits:\n%s`,
				wildcardNS, recentCommitDiagnostics(srcnsRepo.CheckoutDir, grantedPath))
			g.Expect(filepath.Join(srcnsRepo.CheckoutDir, wantOutside)).To(BeAnExistingFile(),
				`"*" is bounded by the source credential's RBAC and nothing else, so %q arrives `+
					"too. Recent commits:\n%s",
				outsideNS, recentCommitDiagnostics(srcnsRepo.CheckoutDir, grantedPath))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("keeps two explicitly-named source namespaces in separate folders", func() {
		// The same SHAPE a wildcard expands to — one GitTarget, one type, two source namespaces —
		// reached without the wildcard. It separates a defect in wildcard EXPANSION from one in how
		// a target handles more than one source namespace at all, which is reachable by two
		// ordinary rules and does not need `"*"`.
		const ruleA, ruleB = "srcns-two-a", "srcns-two-b"
		const cmA, cmB = "srcns-two-cm-a", "srcns-two-cm-b"

		By("creating two rules on ONE target, each naming a different source namespace")
		Expect(applyWatchRuleWithSourceNamespace(ruleA, configNS, grantedTarget, sourceNS)).Error().
			NotTo(HaveOccurred(), "failed to apply the first explicit rule")
		Expect(applyWatchRuleWithSourceNamespace(ruleB, configNS, grantedTarget, wildcardNS)).Error().
			NotTo(HaveOccurred(), "failed to apply the second explicit rule")
		verifyResourceStatus("watchrule", ruleA, configNS, "True", "Succeeded", "")
		verifyResourceStatus("watchrule", ruleB, configNS, "True", "Succeeded", "")

		By("creating one ConfigMap in each watched namespace")
		_, err := kubectlRunInNamespace(sourceNS, "create", "configmap", cmA, "--from-literal=k=v")
		Expect(err).NotTo(HaveOccurred())
		_, err = kubectlRunInNamespace(wildcardNS, "create", "configmap", cmB, "--from-literal=k=v")
		Expect(err).NotTo(HaveOccurred())

		By("asserting each lands under ITS OWN namespace's folder")
		wantA := path.Join(grantedPath, fmt.Sprintf("%s/configmaps/%s.yaml", sourceNS, cmA))
		wantB := path.Join(grantedPath, fmt.Sprintf("%s/configmaps/%s.yaml", wildcardNS, cmB))
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, srcnsRepo.CheckoutDir)
			g.Expect(filepath.Join(srcnsRepo.CheckoutDir, wantA)).To(BeAnExistingFile(),
				"the first source namespace must own its folder. Recent commits:\n%s",
				recentCommitDiagnostics(srcnsRepo.CheckoutDir, grantedPath))
			g.Expect(filepath.Join(srcnsRepo.CheckoutDir, wantB)).To(BeAnExistingFile(),
				"a SECOND source namespace on the same target must get its own folder too, not be "+
					"folded into the first's. Recent commits:\n%s",
				recentCommitDiagnostics(srcnsRepo.CheckoutDir, grantedPath))
		}).Should(Succeed())
	})

	It("refuses an override the ClusterProvider does not delegate, and writes nothing", func() {
		By("recording the repository head before the refused rule exists")
		var headBefore string
		Eventually(func(g Gomega) {
			headBefore = remoteBranchHead(g, srcnsRepo.CheckoutDir)
		}).Should(Succeed())

		By("creating a WatchRule whose override the provider does not delegate")
		Expect(applyWatchRuleWithSourceNamespace(
			refusedRule, configNS, refusedTarget, sourceNS)).Error().
			NotTo(HaveOccurred(), "the CR itself is well-formed; the REFUSAL is a reconciler verdict")

		By("asserting the terminal refusal is published with a fix-naming message")
		verifyResourceCondition("watchrule", refusedRule, configNS,
			"SourceNamespaceAuthorized", "False", "SourceNamespaceNotAllowed",
			"allowAnySourceNamespace")
		verifyResourceCondition("watchrule", refusedRule, configNS,
			"Stalled", "True", "SourceNamespaceNotAllowed", "")
		verifyResourceCondition("watchrule", refusedRule, configNS,
			"Reconciling", "False", "SourceNamespaceNotAllowed", "")

		By("creating a ConfigMap the refused rule would have mirrored")
		const cmName = "srcns-refused-cm"
		_, err := kubectlRunInNamespace(sourceNS, "create", "configmap", cmName,
			"--from-literal=should=not-be-mirrored")
		Expect(err).NotTo(HaveOccurred())

		By("asserting the refused target's folder stays empty")
		// A gate that only writes a condition is not a gate: the stream must not exist at all.
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, srcnsRepo.CheckoutDir)
			refusedDir := filepath.Join(srcnsRepo.CheckoutDir, refusedPath)
			if _, statErr := os.Stat(refusedDir); statErr == nil {
				g.Expect(findFileByBasename(refusedDir, cmName+".yaml")).To(BeEmpty(),
					"a refused WatchRule must write nothing; head before was %s", headBefore)
			}
		}, 20*time.Second, 4*time.Second).Should(Succeed())
	})
})

// applyInClusterClusterProvider applies a ClusterProvider that OMITS kubeConfig — so it names the
// operator's own cluster — admitting one namespace and optionally delegating source-namespace
// selection to the GitTargets it admits.
func applyInClusterClusterProvider(name, allowedNS string, delegate bool) (string, error) {
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: ClusterProvider
metadata:
  name: %s
spec:
  accessFrom:
    names: [%s]
  allowAnySourceNamespace: %t
`, name, allowedNS, delegate)
	return kubectlRunWithStdin("", manifest, "apply", "-f", "-")
}

// applyWildcardConfigMapWatchRule applies a WatchRule whose single item asks for ConfigMaps in
// every namespace. One type, so the cluster-wide reach is observable without mirroring every
// service-account token in the cluster into the fixture repository.
func applyWildcardConfigMapWatchRule(name, ns, target string) (string, error) {
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
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
      sourceNamespace: "*"
`, name, ns, target)
	return kubectlRunWithStdin(ns, manifest, "apply", "-f", "-")
}
