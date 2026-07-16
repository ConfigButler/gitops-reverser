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

// These two specs are the positive counterpart to the unsupported-folder refusal: when a
// GitTarget's path holds a Helm values.yaml that a release names as a path — an Argo CD
// Application through helm.valueFiles, or a Flux HelmRelease through spec.chart.spec.valuesFiles —
// the values file is read-only context: understood, never written, and never a refusal for the
// folder it sits in. The whole folder is ADOPTED, the operator mirrors a live object into it
// (which a refused folder never does), and every seeded file is left byte-for-byte untouched.
//
// They prove docs/design/support-boundary/values-file-projection.md §2 (Move 1) end to end, for
// both the Argo and the Flux spelling. Each folder is seeded so the reference is correct both for
// the tool (repo-root-relative) and for the operator's subtree scan (co-located beside the
// release). Each spec owns its repo/provider/namespace (one repo per Describe, the suite
// convention) so the initial seed push never races the operator's mirror pushes on a shared branch.

var _ = Describe("Manager Values File Acceptance (Argo)", Label("manager", "values-file"), Ordered, func() {
	var env valuesFileEnv

	BeforeAll(func() { env = provisionValuesFileEnv("argo") })

	It("adopts a folder whose values.yaml an Argo CD Application names, leaving it untouched", func() {
		assertReferencedValuesFileAdopted(env, referencedValuesCase{
			caseName:    "argo",
			fixtureRoot: "test/e2e/fixtures/values-file-folder",
			gitPath:     "platform/cert-manager",
			// The co-located ClusterIssuer used to be taken down with the values file; both must
			// now survive adoption (an unwatched type is never swept by the configmaps resync).
			survivors: map[string]string{
				"platform/cert-manager/values.yaml":        "e2e-readonly-context-marker",
				"platform/cert-manager/clusterissuer.yaml": "letsencrypt-production",
				"platform/cert-manager/application.yaml":   "kind: Application",
			},
		})
	})
})

var _ = Describe("Manager Values File Acceptance (Flux)", Label("manager", "values-file"), Ordered, func() {
	var env valuesFileEnv

	BeforeAll(func() { env = provisionValuesFileEnv("flux") })

	It("adopts a folder whose values.yaml a Flux HelmRelease names, leaving it untouched", func() {
		assertReferencedValuesFileAdopted(env, referencedValuesCase{
			caseName:    "flux",
			fixtureRoot: "test/e2e/fixtures/values-file-folder-flux",
			gitPath:     "apps/ingress-nginx",
			survivors: map[string]string{
				"apps/ingress-nginx/values.yaml":      "e2e-readonly-context-marker",
				"apps/ingress-nginx/helmrelease.yaml": "kind: HelmRelease",
			},
		})
	})
})

// valuesFileEnv is one spec's isolated Git + operator wiring: its own namespace, Gitea repo, and
// Ready GitProvider. Keeping it per-Describe means each spec pushes its seed to its own branch, so
// the seed never contends with the operator's mirror pushes.
type valuesFileEnv struct {
	testNs       string
	providerName string
	repo         *RepoArtifacts
}

// provisionValuesFileEnv creates the namespace, repo, credentials, SOPS key, and Ready GitProvider
// for one values-file spec, and registers their teardown via DeferCleanup (so it runs after the
// Describe's specs). caseName keeps the namespace/provider/repo names distinct between the two specs.
func provisionValuesFileEnv(caseName string) valuesFileEnv {
	GinkgoHelper()

	By("creating the values-file test namespace")
	testNs := testNamespaceFor("manager-values-file-" + caseName)
	_, _ = kubectlRun("create", "namespace", testNs)

	By("setting up Gitea repo and credentials")
	repo := SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-values-file-%s-%d", caseName, GinkgoRandomSeed()))

	_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
	Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")

	// createGitTarget enables SOPS encryption referencing the shared sops-age-key secret, so it
	// must exist or the GitTarget's EncryptionConfigured gate (and thus Ready) never goes True —
	// independent of the acceptance under test.
	applySOPSAgeKeyToNamespace(testNs)

	providerName := caseName + "-values-provider"
	By("creating the GitProvider")
	createGitProviderWithURLInNamespace(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
	verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")

	DeferCleanup(func() {
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
		cleanupNamespace(testNs)
	})

	return valuesFileEnv{testNs: testNs, providerName: providerName, repo: repo}
}

// referencedValuesCase parameterises the shared adoption proof over the Argo and Flux spellings.
type referencedValuesCase struct {
	caseName    string
	fixtureRoot string
	gitPath     string
	// survivors maps a repo-relative file path to a substring that must persist unchanged after
	// adoption — the read-only values file and every co-located file the folder-wide refusal used
	// to take down with it.
	survivors map[string]string
}

// assertReferencedValuesFileAdopted seeds a release folder, points a GitTarget at it, and proves
// the folder is adopted (not refused as non-krm-yaml): the operator mirrors a live ConfigMap into
// it — which a refused folder never does — while leaving the values file and its co-located
// siblings byte-for-byte untouched. Per-spec resources are torn down via DeferCleanup.
func assertReferencedValuesFileAdopted(env valuesFileEnv, tc referencedValuesCase) {
	GinkgoHelper()

	destName := tc.caseName + "-values-dest"
	ruleName := tc.caseName + "-values-rule"
	cmName := tc.caseName + "-values-demo"

	By("seeding the Git repository with the release, its values.yaml, and any co-located manifest")
	seedRenderedFolderIntoRepo(env.repo, env.testNs, tc.fixtureRoot, tc.gitPath)

	By("the GitTarget accepts the path instead of refusing it as non-krm-yaml")
	createGitTarget(destName, env.testNs, env.providerName, tc.gitPath, "main")
	DeferCleanup(func() { cleanupGitTarget(destName, env.testNs) })
	err := applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", struct {
		Name            string
		Namespace       string
		DestinationName string
	}{Name: ruleName, Namespace: env.testNs, DestinationName: destName}, env.testNs)
	Expect(err).NotTo(HaveOccurred(), "failed to apply ConfigMap WatchRule")
	DeferCleanup(func() { cleanupWatchRule(ruleName, env.testNs) })

	waitForGitTargetGitPathAccepted(destName, env.testNs)
	verifyResourceCondition("gittarget", destName, env.testNs, "Validated", "True", "OK", "")
	verifyResourceStatus("watchrule", ruleName, env.testNs, "True", "Ready", "")
	waitForStreamsRunning(destName, env.testNs)

	By("mirroring a live ConfigMap into the adopted folder — a refused folder writes nothing")
	_, err = kubectlRunInNamespace(env.testNs, "create", "configmap", cmName, "--from-literal=color=blue")
	Expect(err).NotTo(HaveOccurred(), "ConfigMap creation should succeed")
	DeferCleanup(func() {
		_, _ = kubectlRunInNamespace(env.testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
	})

	mirrored := filepath.Join(
		env.repo.CheckoutDir,
		tc.gitPath,
		fmt.Sprintf("%s/configmaps/%s.yaml", env.testNs, cmName),
	)
	Eventually(func(g Gomega) {
		pullLatestRepoState(g, env.repo.CheckoutDir)
		content, readErr := os.ReadFile(mirrored)
		g.Expect(readErr).NotTo(HaveOccurred(), "the operator must adopt the folder and mirror %s", mirrored)
		g.Expect(string(content)).To(ContainSubstring("color: blue"))
	}, 90*time.Second, 2*time.Second).Should(Succeed())

	By("leaving the read-only values file and every co-located manifest byte-for-byte untouched")
	Consistently(func(g Gomega) {
		pullLatestRepoState(g, env.repo.CheckoutDir)
		for rel, want := range tc.survivors {
			content, readErr := os.ReadFile(filepath.Join(env.repo.CheckoutDir, rel))
			g.Expect(readErr).NotTo(HaveOccurred(), "%s must remain in the repo, never swept", rel)
			g.Expect(string(content)).To(ContainSubstring(want),
				"a read-only context file must never be edited or replaced by the operator: %s", rel)
		}
	}, 15*time.Second, 3*time.Second).Should(Succeed())
}

// waitForGitTargetGitPathAccepted is the positive counterpart to waitForGitTargetGitPathRefused:
// the GitTarget reports the path accepted (GitPathAccepted=True) and is healthy (Ready, not
// stalled), so the acceptance gate passed on the seeded folder rather than refusing it.
func waitForGitTargetGitPathAccepted(name, namespace string) {
	GinkgoHelper()

	verifyResourceCondition("gittarget", name, namespace, "GitPathAccepted", "True", "", "", "150s")
	verifyResourceCondition("gittarget", name, namespace, "Ready", "True", "", "", "150s")
	verifyResourceCondition("gittarget", name, namespace, "Stalled", "False", "", "", "150s")
}
