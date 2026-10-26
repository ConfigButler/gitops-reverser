// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	configMapGeneratorFixtureRoot = "test/e2e/fixtures/config-map-generator"
	configMapGeneratorGitPath     = "apps/coffee"
	configMapGeneratorTimeout     = 2 * time.Minute
)

// A ConfigMap a configMapGenerator renders, edited through the API, is written back into the
// generator's input file as a one-line diff, and Flux, building the folder from that commit, applies
// the same object again. Flux applies the folder before the GitTarget exists, so the ConfigMap the
// spec edits is the one Flux made, and Flux's next apply is what proves the write converges.
var _ = Describe("Manager ConfigMap Generator", Label("manager", "config-map-generator", "flux"), Ordered, func() {
	const (
		providerName = "config-map-generator-provider"
		targetName   = "config-map-generator-target"
		ruleName     = "config-map-generator-rule"
	)
	var (
		testNs, fluxName, fluxSecretName string
		repo                             *RepoArtifacts
	)

	BeforeAll(func() {
		testNs = testNamespaceFor("manager-config-map-generator")
		fluxName = "config-map-generator-" + testNs
		fluxSecretName = fluxName + "-git"
		_, _ = kubectlRun("create", "namespace", testNs)

		By("setting up the Gitea repo, its credentials and the GitProvider")
		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-config-map-generator-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)
		createReadyGitProvider(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
	})

	AfterAll(func() {
		cleanupWatchRule(ruleName, testNs)
		cleanupGitTarget(targetName, testNs)
		cleanupNamespacedResource(testNs, "gitprovider", providerName)
		cleanupNamespacedResource("flux-system", "kustomization", fluxName)
		cleanupNamespacedResource("flux-system", "gitrepository", fluxName)
		cleanupNamespacedResource("flux-system", "secret", fluxSecretName)
		cleanupNamespace(testNs)
	})

	It("writes a generated ConfigMap's edit to its env line, and Flux applies the same object", func() {
		By("seeding a folder whose ConfigMaps a configMapGenerator builds from menu.yaml and flags.env")
		fixture := renderInPlaceFixtureFolder(configMapGeneratorFixtureRoot, testNs)
		DeferCleanup(func() { _ = os.RemoveAll(fixture) })
		seedRenderedFolderIntoRepo(repo, testNs, fixture, configMapGeneratorGitPath)

		By("letting Flux build the folder and apply the generated ConfigMaps")
		applyFluxApplier(testNs, repo, fluxName, fluxSecretName, "main", "./"+configMapGeneratorGitPath)
		Eventually(func(g Gomega) {
			g.Expect(generatedFlag(g, testNs, "ENABLE_VOUCHERS")).To(Equal("true"))
		}, configMapGeneratorTimeout, 2*time.Second).Should(Succeed())
		uid := generatedFlagsUID(testNs)

		By("pointing a GitTarget at the folder: the generator is adopted, not refused")
		createValidatedGitTarget(targetName, testNs, providerName, configMapGeneratorGitPath)
		applyGeneratedConfigMapWatchRule(ruleName, testNs, targetName)
		waitForGitTargetGitPathAccepted(targetName, testNs)
		waitForStreamsRunning(targetName, testNs)
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
		}, configMapGeneratorTimeout, 2*time.Second).Should(Succeed())
		seeded := gitHead(repo.CheckoutDir)

		By("adopting the in-sync folder without a commit")
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			g.Expect(gitHead(repo.CheckoutDir)).To(Equal(seeded), "an in-sync generator has nothing to write")
		}, 15*time.Second, 3*time.Second).Should(Succeed())

		By("turning the vouchers off through the API")
		_, err := kubectlRunInNamespace(testNs, "patch", "configmap", "coffee-flags", "--type=merge",
			"-p", `{"data":{"ENABLE_VOUCHERS":"false"}}`)
		Expect(err).NotTo(HaveOccurred(), "failed to patch the generated ConfigMap")

		By("committing a one-line diff to flags.env, and no other file of the folder")
		envPath := configMapGeneratorGitPath + "/flags.env"
		var written string
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			written = gitHead(repo.CheckoutDir)
			g.Expect(written).NotTo(Equal(seeded))
		}, configMapGeneratorTimeout, 2*time.Second).Should(Succeed())
		changed, err := gitRun(repo.CheckoutDir, "show", "--format=", "--name-only", written)
		Expect(err).NotTo(HaveOccurred())
		Expect(withoutBootstrapFiles(strings.Fields(changed))).To(Equal([]string{envPath}),
			"the commit touches only the env file, beside the operator's first-write bootstrap files")
		diff, err := gitRun(repo.CheckoutDir, "show", "--format=", "--unified=0", written, "--", envPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(diffLines(diff)).To(Equal([]string{"-ENABLE_VOUCHERS=true", "+ENABLE_VOUCHERS=false"}))
		content, err := os.ReadFile(filepath.Join(repo.CheckoutDir, envPath))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(content)).To(Equal(
			"# Turn off during the conference rush.\nENABLE_VOUCHERS=false\nSHOW_BANNER=true\n"),
			"the comment and the untouched line keep their bytes")

		By("letting Flux apply that commit: it renders the same object, under the same UID")
		reconcileFluxKustomizationWithSource(fluxName)
		Eventually(func(g Gomega) {
			g.Expect(fluxLastAppliedRevision(g, fluxName)).To(ContainSubstring(written))
		}, configMapGeneratorTimeout, 2*time.Second).Should(Succeed())
		Expect(generatedFlag(Default, testNs, "ENABLE_VOUCHERS")).To(Equal("false"))
		Expect(generatedFlagsUID(testNs)).To(Equal(uid), "the hash is disabled, so the object keeps its name")

		By("converging: Flux's apply is not mirrored back as another commit")
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			g.Expect(gitHead(repo.CheckoutDir)).To(Equal(written))
		}, 20*time.Second, 4*time.Second).Should(Succeed())
	})
})

// applyGeneratedConfigMapWatchRule watches the ConfigMaps the fixture's generator labels, and no
// other ConfigMap of the namespace.
func applyGeneratedConfigMapWatchRule(name, namespace, target string) {
	GinkgoHelper()
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
      objectSelector:
        matchLabels:
          app.kubernetes.io/part-of: coffee-generator-e2e
`, name, namespace, target)
	_, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply the WatchRule")
	verifyResourceStatus("watchrule", name, namespace, "True", "Succeeded", "")
}

// generatedFlag is one key of the live coffee-flags ConfigMap.
func generatedFlag(g Gomega, namespace, key string) string {
	out, err := kubectlRunInNamespace(namespace, "get", "configmap", "coffee-flags",
		"-o", "jsonpath={.data."+key+"}")
	g.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// generatedFlagsUID is the UID of the live coffee-flags ConfigMap.
func generatedFlagsUID(namespace string) string {
	GinkgoHelper()
	out, err := kubectlRunInNamespace(namespace, "get", "configmap", "coffee-flags", "-o", "jsonpath={.metadata.uid}")
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// gitHead is the commit checked out in dir.
func gitHead(dir string) string {
	GinkgoHelper()
	out, err := gitRun(dir, "rev-parse", "HEAD")
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// withoutBootstrapFiles drops the files the operator adds to a folder with its first commit there
// (internal/git/bootstrapped-repo-template), which the folder under test was seeded without.
func withoutBootstrapFiles(paths []string) []string {
	var out []string
	for _, p := range paths {
		switch filepath.Base(p) {
		case "README.md", ".gittargetignore", ".sops.yaml":
			continue
		}
		out = append(out, p)
	}
	return out
}

// diffLines is the removed and added lines of a unified diff, without its headers.
func diffLines(diff string) []string {
	var out []string
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			continue
		}
		if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "+") {
			out = append(out, line)
		}
	}
	return out
}

// reconcileFluxKustomizationWithSource asks Flux to fetch the source and apply the Kustomization.
func reconcileFluxKustomizationWithSource(name string) {
	GinkgoHelper()
	args := []string{"reconcile", "kustomization", name, "-n", "flux-system", "--with-source", "--timeout", "90s"}
	if context := strings.TrimSpace(kubectlContext()); context != "" {
		args = append([]string{"--context", context}, args...)
	}
	output, err := exec.Command("flux", args...).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "flux %s: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
}
