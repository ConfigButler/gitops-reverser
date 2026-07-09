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

// Identity-based write exclusion is the fix for the loop a reverser forms with a GitOps
// forward leg on the same branch: the forward leg applies the branch back into the cluster,
// the reverser mirrors that apply as a live update, and the new commit re-triggers the
// forward leg.
//
// These specs stand in for the forward leg with `kubectl apply --server-side
// --field-manager=kustomize-controller`. That is exactly what a GitOps tool does, and the
// field manager is the whole signal `excludeFieldManagers` reads, so the substitution is
// faithful and needs no Flux install.
//
// The forward leg's apply changes `data`, not a label: internal/sanitize already strips
// `kustomize.toolkit.fluxcd.io/*` labels and annotations from Git content, so a stamped
// label alone would carry no change to mirror and the content dedup would drop it before any
// exclusion was consulted. A content change is the case the exclusion actually decides.
//
// Two GitTargets watch the same ConfigMaps into sibling folders: one whose rule declines the
// forward leg's writes, one whose rule does not. The control target is what makes the
// assertion sharp — once the forward leg's value appears in the mirrored folder, the event
// has demonstrably been processed, so its absence in the excluded folder is the exclusion
// doing its job rather than a race we did not wait long enough for.
//
// Not Serial: the spec owns a dedicated Gitea repo, so nothing else writes its branch.
var _ = Describe("Write exclusion", Label("manager"), Ordered, func() {
	var (
		testNs   string
		repo     *RepoArtifacts
		provider string
	)

	const (
		fluxFieldManager  = "kustomize-controller"
		humanFieldManager = "kubectl-e2e-human"

		excludedPath = "e2e/write-exclusion/excluded"
		mirroredPath = "e2e/write-exclusion/mirrored"
	)

	var excludedTarget, mirroredTarget string

	// applyRule applies a WatchRule for one GitTarget, optionally declining a field manager.
	applyRule := func(name, target, excludeFieldManager string) {
		GinkgoHelper()
		exclude := ""
		if excludeFieldManager != "" {
			exclude = fmt.Sprintf("\n      excludeFieldManagers: [%q]", excludeFieldManager)
		}
		rule := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: %s
  namespace: %s
spec:
  targetRef:
    kind: GitTarget
    name: %s
  rules:
    - resources: ["configmaps"]%s
`, name, testNs, target, exclude)
		_, err := kubectlRunWithStdin(testNs, rule, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "failed to apply WatchRule %s", name)
		verifyResourceStatus("watchrule", name, testNs, "True", "Ready", "")
	}

	BeforeAll(func() {
		By("creating the write-exclusion namespace and Git credentials")
		testNs = testNamespaceFor("write-exclusion")
		_, _ = kubectlRun("create", "namespace", testNs)
		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-write-exclusion-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to namespace")
		applySOPSAgeKeyToNamespace(testNs)

		seed := GinkgoRandomSeed()
		provider = fmt.Sprintf("write-exclusion-provider-%d", seed)
		excludedTarget = fmt.Sprintf("write-exclusion-excluded-%d", seed)
		mirroredTarget = fmt.Sprintf("write-exclusion-mirrored-%d", seed)

		createGitProviderWithURLInNamespace(provider, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		verifyResourceStatus("gitprovider", provider, testNs, "True", "Ready", "Repository connectivity validated")

		By("creating one GitTarget that declines the forward leg's writes and one that does not")
		createGitTarget(excludedTarget, testNs, provider, excludedPath, "main")
		createGitTarget(mirroredTarget, testNs, provider, mirroredPath, "main")
		verifyResourceCondition("gittarget", excludedTarget, testNs, "Validated", "True", "OK", "")
		verifyResourceCondition("gittarget", mirroredTarget, testNs, "Validated", "True", "OK", "")

		applyRule(excludedTarget+"-rule", excludedTarget, fluxFieldManager)
		applyRule(mirroredTarget+"-rule", mirroredTarget, "")

		waitForStreamsRunning(excludedTarget, testNs)
		waitForStreamsRunning(mirroredTarget, testNs)
	})

	AfterAll(func() {
		cleanupWatchRule(excludedTarget+"-rule", testNs)
		cleanupWatchRule(mirroredTarget+"-rule", testNs)
		cleanupGitTarget(excludedTarget, testNs)
		cleanupGitTarget(mirroredTarget, testNs)
		cleanupNamespace(testNs)
	})

	// applyConfigMap applies a ConfigMap under a named field manager, exactly as a GitOps
	// tool's server-side apply does.
	applyConfigMap := func(name, fieldManager, flavor string) {
		GinkgoHelper()
		manifest := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  flavor: %s
`, name, testNs, flavor)
		_, err := kubectlRunWithStdin(testNs, manifest,
			"apply", "--server-side", "--force-conflicts", "--field-manager="+fieldManager, "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "apply as %s should succeed", fieldManager)
	}

	// fileIn returns the ConfigMap's path in a GitTarget's folder, first repo-relative (for
	// messages) and then absolute (for reads).
	fileIn := func(targetPath, name string) (string, string) {
		rel := path.Join(targetPath, fmt.Sprintf("%s/configmaps/%s.yaml", testNs, name))
		return rel, filepath.Join(repo.CheckoutDir, rel)
	}

	eventuallyContains := func(absPath, want string) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			content, err := os.ReadFile(absPath)
			g.Expect(err).NotTo(HaveOccurred(), "expected a committed file at %s", absPath)
			g.Expect(string(content)).To(ContainSubstring(want))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	}

	It("mirrors a human's write, drops the forward leg's, and keeps mirroring the human's next edit", func() {
		const cmName = "excluded-forward-leg"
		excludedRel, excludedAbs := fileIn(excludedPath, cmName)
		_, mirroredAbs := fileIn(mirroredPath, cmName)

		By("a human creates the ConfigMap")
		applyConfigMap(cmName, humanFieldManager, "vanilla")

		By("the human's write reaches both folders")
		eventuallyContains(excludedAbs, "flavor: vanilla")
		eventuallyContains(mirroredAbs, "flavor: vanilla")

		By("the forward leg applies a different value back into the cluster, as it would after a Git change")
		applyConfigMap(cmName, fluxFieldManager, "chocolate")

		By("the unrestricted GitTarget mirrors that apply — which proves the event was processed")
		eventuallyContains(mirroredAbs, "flavor: chocolate")

		By("the excluding GitTarget did not: the forward leg's own apply is never committed back")
		excludedContent, err := os.ReadFile(excludedAbs)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(excludedContent)).To(ContainSubstring("flavor: vanilla"),
			"the forward leg's apply reached %s but must never reach %s — that is the loop this prevents",
			mirroredPath, excludedRel)
		Expect(string(excludedContent)).NotTo(ContainSubstring("flavor: chocolate"))

		By("a later human edit of the same object is still mirrored to the excluding target")
		applyConfigMap(cmName, humanFieldManager, "strawberry")
		eventuallyContains(excludedAbs, "flavor: strawberry")

		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
	})

	// managedFields names who last WROTE an object, not who deleted it. A field-manager
	// exclusion must therefore never suppress a delete, or a human deleting a Flux-managed
	// resource would be silently ignored — the exact failure a label selector has.
	It("still mirrors a delete of an object the excluded manager last wrote", func() {
		const cmName = "deleted-after-forward-leg"
		excludedRel, excludedAbs := fileIn(excludedPath, cmName)
		_, mirroredAbs := fileIn(mirroredPath, cmName)

		By("a human creates it, so it reaches Git")
		applyConfigMap(cmName, humanFieldManager, "vanilla")
		eventuallyContains(excludedAbs, "flavor: vanilla")

		By("the forward leg applies it, becoming the object's last writer")
		applyConfigMap(cmName, fluxFieldManager, "chocolate")
		// Gate on the control target so the forward leg's write is known to be processed
		// before the delete, rather than racing it.
		eventuallyContains(mirroredAbs, "flavor: chocolate")

		By("a human deletes it")
		_, err := kubectlRunInNamespace(testNs, "delete", "configmap", cmName)
		Expect(err).NotTo(HaveOccurred())

		By("the delete reaches Git even though the excluded manager wrote the object last")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			_, statErr := os.Stat(excludedAbs)
			g.Expect(os.IsNotExist(statErr)).To(BeTrue(),
				"the file must be removed at %s: managedFields names the last writer, not the deleter", excludedRel)
			g.Expect(latestCommitSubjectForPath(g, repo.CheckoutDir, excludedRel)).To(ContainSubstring("[DELETE]"))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())
	})
})
