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

// This spec covers WatchRule.spec.sourceNamespace end to end (see
// docs/design/watchrule-source-namespace/historical-top-level-source-namespace-baseline.md; the
// field moves onto spec.rules[] in pr4-cluster-scope-only.md).
//
// The load-bearing assertion is the GIT PATH one. The whole design rests on a claim that is
// invisible from the API types: sourceNamespace changes which namespace is WATCHED and nothing
// about where objects are WRITTEN, because Git placement is rebuilt from each mirrored object's own
// metadata.namespace rather than from any config-plane namespace. If that ever regresses, a rule in
// tenant-* would start filing another namespace's objects under its own folder — silently, and in a
// tenant's repository. Nothing else in the suite would catch it.
//
// The refusal spec is its safety twin: an unauthorized override must publish a terminal condition
// AND write nothing at all.
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
		grantedPath     = "e2e/srcns-granted"
		refusedPath     = "e2e/srcns-refused"
	)

	var (
		// configNS holds the WatchRules and GitTargets; sourceNS is the namespace they WATCH. The
		// two differ on purpose — that separation is the entire feature.
		configNS  string
		sourceNS  string
		srcnsRepo *RepoArtifacts
	)

	BeforeAll(func() {
		By("creating separate config-plane and source namespaces")
		configNS = testNamespaceFor("srcns-config")
		sourceNS = testNamespaceFor("srcns-source")
		_, _ = kubectlRun("create", "namespace", configNS)
		_, _ = kubectlRun("create", "namespace", sourceNS)

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
			"True", "Ready", "Repository connectivity validated")

		By("declaring two in-cluster ClusterProviders that differ ONLY in the delegation flag")
		// Both omit kubeConfig, so both name the operator's own cluster: this is deliberately the
		// SHARPER of the two cases the design describes, where an authorized override bypasses live
		// namespace RBAC. Dedicated providers keep the shared "default" one untouched.
		Expect(applyInClusterClusterProvider(delegatingCP, configNS, true)).Error().
			NotTo(HaveOccurred(), "failed to apply delegating ClusterProvider")
		Expect(applyInClusterClusterProvider(nonDelegatingCP, configNS, false)).Error().
			NotTo(HaveOccurred(), "failed to apply non-delegating ClusterProvider")

		By("creating a GitTarget whose policy admits the source namespace, and one that is refused")
		Expect(applyGitTargetWithSourceNamespaces(
			configNS, grantedTarget, providerName, grantedPath, delegatingCP, sourceNS)).Error().
			NotTo(HaveOccurred(), "failed to apply granted GitTarget")
		Expect(applyGitTargetWithSourceNamespaces(
			configNS, refusedTarget, providerName, refusedPath, nonDelegatingCP, sourceNS)).Error().
			NotTo(HaveOccurred(), "failed to apply refused GitTarget")

		verifyResourceCondition("gittarget", grantedTarget, configNS, "Validated", "True", "OK", "")
		verifyResourceCondition("gittarget", refusedTarget, configNS, "Validated", "True", "OK", "")
	})

	AfterAll(func() {
		deleteClusterProvider(delegatingCP)
		deleteClusterProvider(nonDelegatingCP)
		cleanupNamespace(configNS)
		cleanupNamespace(sourceNS)
	})

	SetDefaultEventuallyTimeout(60 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("mirrors an authorized source namespace under THAT namespace's Git folder", func() {
		By("creating a WatchRule in the config namespace that watches the source namespace")
		Expect(applyWatchRuleWithSourceNamespace(
			grantedRule, configNS, grantedTarget, sourceNS)).Error().
			NotTo(HaveOccurred(), "failed to apply granted WatchRule")

		By("asserting the override is authorized")
		verifyResourceCondition("watchrule", grantedRule, configNS,
			"SourceNamespaceAuthorized", "True", "SourceNamespaceAllowed", "")
		verifyResourceStatus("watchrule", grantedRule, configNS, "True", "Ready", "")
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
			"allowWatchRuleSourceNamespaceOverride")
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
  allowedNamespaces:
    names: [%s]
  allowWatchRuleSourceNamespaceOverride: %t
`, name, allowedNS, delegate)
	return kubectlRunWithStdin("", manifest, "apply", "-f", "-")
}

// applyGitTargetWithSourceNamespaces applies a GitTarget that declares an allowedSourceNamespaces
// policy naming one source namespace by exact name.
func applyGitTargetWithSourceNamespaces(
	ns, name, gitProvider, targetPath, clusterProvider, sourceNS string,
) (string, error) {
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: %s
  namespace: %s
spec:
  providerRef:
    kind: GitProvider
    name: %s
  branch: main
  path: %s
  clusterProviderRef:
    name: %s
  allowedSourceNamespaces:
    names: [%s]
`, name, ns, gitProvider, targetPath, clusterProvider, sourceNS)
	return kubectlRunWithStdin(ns, manifest, "apply", "-f", "-")
}

// applyWatchRuleWithSourceNamespace applies a WatchRule that watches a namespace OTHER than its own.
func applyWatchRuleWithSourceNamespace(name, ns, target, sourceNS string) (string, error) {
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: %s
  namespace: %s
spec:
  targetRef:
    kind: GitTarget
    name: %s
  sourceNamespace: %s
  rules:
    - resources: ["configmaps"]
`, name, ns, target, sourceNS)
	return kubectlRunWithStdin(ns, manifest, "apply", "-f", "-")
}
