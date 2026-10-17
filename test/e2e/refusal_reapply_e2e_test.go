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

// GitTarget spec.onRefusal: Reapply, with a REAL refusal and a real Flux Kustomization as the
// applier.
//
// The refusal has to be the right KIND, and choosing it is most of what this spec is about. A
// folder-level refusal (a foreign file, unparseable YAML) is normally found by the per-type
// reconcile, which blocks the collection through the event router and never reaches the live-write
// path the action hangs off. It is also the case where reapplying achieves nothing: only a human
// can repair a folder.
//
// The case the action exists for is a WRITE-BOUNDARY refusal, where the folder is accepted and
// this one edit had nowhere to land. Shape 8 of the layout corpus is exactly that: an overlay at
// apps/checkout/overlays/prod whose Deployment comes from ../../base, and a live edit to an env
// var, which no kustomize declaration in the overlay can express. Read scope reaches the base;
// write scope never does.
//
// Flux applies the overlay when its Kustomization is created, and the trigger then holds it
// suspended, so a revert of the drifted env var inside this spec is an apply GitOps Reverser started,
// and nothing else: a resume of the unchanged commit, which is all a Refusal needs.
var _ = Describe("Manager Refusal Reapply", Label("manager", "refusal-reapply"), Ordered, func() {
	var (
		testNs         string
		repo           *RepoArtifacts
		providerName   = "refusal-reapply-provider"
		destName       = "refusal-reapply-dest"
		ruleName       = "refusal-reapply-rule"
		gitPath        = "apps/checkout/overlays/prod"
		fluxName       string
		fluxSecretName string
	)

	BeforeAll(func() {
		By("creating the test namespace")
		testNs = testNamespaceFor("manager-refusal-reapply")
		_, _ = kubectlRun("create", "namespace", testNs)
		fluxName = "refusal-reapply-" + testNs
		fluxSecretName = fluxName + "-git"

		By("setting up Gitea repo and credentials")
		repo = SetupRepo(resolveE2EContext(), testNs,
			fmt.Sprintf("e2e-refusal-reapply-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		By("creating the GitProvider")
		createGitProviderWithURLInNamespace(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Succeeded", "")
	})

	AfterAll(func() {
		cleanupWatchRule(ruleName, testNs)
		cleanupGitTarget(destName, testNs)
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
		cleanupNamespacedResource("flux-system", "kustomization", fluxName)
		cleanupNamespacedResource("flux-system", "gitrepository", fluxName)
		cleanupNamespacedResource("flux-system", "secret", fluxSecretName)
		cleanupNamespace(testNs)
	})

	It("asks the applier to reapply when the live object has nowhere in Git to land", func() {
		By("seeding a base-and-overlay folder that renders into this test's namespace")
		seedFolder := writeBaseOwnedFieldFolder(testNs)
		DeferCleanup(func() { _ = os.RemoveAll(seedFolder) })
		seedRenderedFolderIntoRepo(repo, testNs, seedFolder, "apps/checkout")

		By("letting Flux apply the overlay once, before any trigger holds it")
		applyFluxApplier(testNs, repo, fluxName, fluxSecretName, "main", "./"+gitPath)
		Eventually(func(g Gomega) {
			g.Expect(checkoutLogLevel(g, testNs)).To(Equal("info"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		// The divergence is the point: a reapply that has nothing to correct would prove nothing.
		By("drifting the base-owned env var away from what Git says")
		_, err := kubectlRunInNamespace(testNs, "set", "env", "deployment/checkout", "LOG_LEVEL=debug")
		Expect(err).NotTo(HaveOccurred(), "failed to drift the base-owned env var")

		// Relative, because the seeded root commit has no parent to diff against and reads as empty.
		emptyBefore := emptyCommitCount(repo.CheckoutDir)

		By("creating the GitTarget on the OVERLAY, naming the Kustomization and asking it to reapply")
		createGitTarget(destName, testNs, providerName, gitPath, "main")
		createFluxTrigger(testNs, fluxName, fluxName, "")
		nameApplyTrigger(testNs, destName, "FluxTrigger", fluxName, "Reapply")
		verifyResourceCondition("gittarget", destName, testNs, "ApplyTriggerReady", "True", "Succeeded", "")

		// The overlay renders into THIS namespace, so the rule watches its own and needs no
		// sourceNamespace override. ConfigMaps ride along so this target has an ACCEPTED watched
		// type beside the refused one: a reconcile of one type succeeding is not evidence about
		// another that is still refused.
		Expect(applyFromTemplate("test/e2e/templates/manager/watchrule-resources.tmpl", struct {
			Name            string
			Namespace       string
			DestinationName string
			Resources       string
		}{Name: ruleName, Namespace: testNs, DestinationName: destName,
			Resources: `"deployments","configmaps"`},
			testNs)).To(Succeed(), "failed to apply the WatchRule")

		By("the write is refused at the BOUNDARY, which is the kind that earns a reapply")
		waitForGitTargetGitPathRefused(destName, testNs, "WriteBoundaryRefused")
		verifyResourceCondition("gittarget", destName, testNs, "GitPathAccepted", "False",
			"WriteBoundaryRefused", "escapes the GitTarget write scope")

		By("the applier is asked, and Flux reverts the refused edit")
		Eventually(func(g Gomega) {
			g.Expect(checkoutLogLevel(g, testNs)).To(Equal("info"),
				"a refused edit on a Reapply target must be reverted by its applier")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(applyRequestedMessages(g, testNs, destName)).To(ContainSubstring("(Refusal)"),
				"the revert must come from GitOps Reverser's Refusal request, not from Flux's own schedule")
		}, 30*time.Second, 2*time.Second).Should(Succeed())
		Expect(emptyCommitCount(repo.CheckoutDir)).To(Equal(emptyBefore),
			"the request goes to the applier; nothing moves the branch for it")

		// Past two rate-limit intervals. The revert itself is a new observation of the refusal
		// (the env var changed back), so one more request may follow it. What must never grow is
		// the count after that: a forced recheck re-observes the standing refusal every minute.
		By("and it settles instead of asking on every recheck")
		seen := map[string]struct{}{readTriggerOperation(Default, testNs, fluxTriggerResource, fluxName).Token: {}}
		Consistently(func(g Gomega) {
			seen[readTriggerOperation(g, testNs, fluxTriggerResource, fluxName).Token] = struct{}{}
			g.Expect(len(seen)).To(BeNumerically("<=", 2),
				"a standing refusal must not be requested once per recheck")
		}, 150*time.Second, 10*time.Second).Should(Succeed())
		Expect(kustomizationSuspended(Default, fluxName)).To(BeTrue(), "every operation suspends it again")

		By("a ConfigMap on the same target is mirrored normally")
		Expect(applyFromTemplate("test/e2e/templates/manager/configmap.tmpl", struct {
			Name      string
			Namespace string
		}{Name: "refusal-sibling", Namespace: testNs},
			testNs)).To(Succeed(), "failed to apply the sibling ConfigMap")
		Eventually(func(g Gomega) {
			g.Expect(runGitIn(repo.CheckoutDir, "fetch", "origin", "main")).To(Succeed())
			g.Expect(gitShowFiles(repo.CheckoutDir)).To(ContainSubstring("refusal-sibling"),
				"the accepted type must reach Git, or this proves nothing about it")
		}, 120*time.Second, 2*time.Second).Should(Succeed())

		// The target is NOT asked to converge, because this shape cannot: the live object carries
		// the API server's defaults and the base document does not, so a write for it is planned,
		// and refused, whatever the env var says. What is provable is that the operator keeps
		// evaluating and treats a genuinely new edit as new.
		By("editing the same field again, to a value nobody has been refused for yet")
		_, err = kubectlRunInNamespace(testNs, "set", "env", "deployment/checkout", "LOG_LEVEL=trace")
		Expect(err).NotTo(HaveOccurred(), "failed to make the second drifting edit")

		By("which is reverted too")
		Eventually(func(g Gomega) {
			g.Expect(checkoutLogLevel(g, testNs)).To(Equal("info"),
				"a genuinely new refused edit must be requested again")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
		Expect(emptyCommitCount(repo.CheckoutDir)).To(Equal(emptyBefore))
	})
})

// applyRequestedMessages is every ApplyRequested Event message recorded on a GitTarget.
func applyRequestedMessages(g Gomega, testNs, gitTarget string) string {
	return gitTargetEventMessages(g, testNs, gitTarget, "ApplyRequested")
}

// checkoutLogLevel reads the LOG_LEVEL env var of the live checkout Deployment.
func checkoutLogLevel(g Gomega, testNs string) string {
	out, err := kubectlRunInNamespace(testNs, "get", "deployment", "checkout", "-o",
		`jsonpath={.spec.template.spec.containers[0].env[?(@.name=="LOG_LEVEL")].value}`)
	g.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// writeBaseOwnedFieldFolder builds corpus shape 8 with the overlay pointed at the caller's
// namespace, so the rendered object lands where a plain WatchRule already watches.
//
// The shape is the point and is copied rather than invented: a base holding the Deployment, and an
// overlay that only sets a namespace and lists the base under resources. There is no images: block
// and no patch, so the overlay can express a namespace and nothing else — which is what makes an
// env-var edit have nowhere to land.
func writeBaseOwnedFieldFolder(namespace string) string {
	GinkgoHelper()

	dir, err := os.MkdirTemp("", "gitops-reverser-e2e-base-owned-*")
	Expect(err).NotTo(HaveOccurred(), "failed to create the base-owned fixture directory")
	Expect(os.MkdirAll(filepath.Join(dir, "base"), 0o755)).To(Succeed())
	Expect(os.MkdirAll(filepath.Join(dir, "overlays", "prod"), 0o755)).To(Succeed())

	Expect(os.WriteFile(filepath.Join(dir, "base", "deployment.yaml"), []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout
spec:
  replicas: 1
  selector:
    matchLabels:
      app: checkout
  template:
    metadata:
      labels:
        app: checkout
    spec:
      containers:
        - name: checkout
          image: registry.k8s.io/pause:3.9
          env:
            - name: LOG_LEVEL
              value: info
`), 0o600)).To(Succeed())

	baseKustomization := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
`
	Expect(os.WriteFile(
		filepath.Join(dir, "base", "kustomization.yaml"), []byte(baseKustomization), 0o600)).To(Succeed())

	Expect(os.WriteFile(filepath.Join(dir, "overlays", "prod", "kustomization.yaml"),
		[]byte(fmt.Sprintf(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: %s
resources:
  - ../../base
`, namespace)), 0o600)).To(Succeed())

	return dir
}

// remoteCommitCount reads the remote's commit count through the seeded checkout.
func remoteCommitCount(checkoutDir string) int {
	GinkgoHelper()
	Expect(runGitIn(checkoutDir, "fetch", "origin", "main")).To(Succeed())
	out, err := exec.Command("git", "-C", checkoutDir, "rev-list", "--count", "origin/main").CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git rev-list: %s", out)
	count := 0
	_, scanErr := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &count)
	Expect(scanErr).NotTo(HaveOccurred())
	return count
}

// emptyCommitCount counts the commits on the remote branch whose diff is empty. A refusal is answered
// by asking its applier; nothing is committed for it, so the count must not grow however often the
// refusal is answered.
func emptyCommitCount(checkoutDir string) int {
	GinkgoHelper()
	Expect(runGitIn(checkoutDir, "fetch", "origin", "main")).To(Succeed())
	out, err := exec.Command("git", "-C", checkoutDir, "rev-list", "origin/main").CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git rev-list: %s", out)
	count := 0
	for _, sha := range strings.Fields(string(out)) {
		if emptyDiffAtSHA(checkoutDir, sha) {
			count++
		}
	}
	return count
}

// gitShowFiles lists every path the remote branch holds, so a spec can say what reached Git.
func gitShowFiles(checkoutDir string) string {
	GinkgoHelper()
	out, err := exec.Command("git", "-C", checkoutDir, "ls-tree", "-r", "--name-only", "origin/main").
		CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git ls-tree: %s", out)
	return string(out)
}

func remoteHead(g Gomega, checkoutDir string) string {
	g.Expect(runGitIn(checkoutDir, "fetch", "origin", "main")).To(Succeed())
	out, err := exec.Command("git", "-C", checkoutDir, "rev-parse", "origin/main").CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(), "git rev-parse: %s", out)
	return strings.TrimSpace(string(out))
}

func emptyDiffAtSHA(checkoutDir, sha string) bool {
	GinkgoHelper()
	out, err := exec.Command("git", "-C", checkoutDir,
		"diff-tree", "--no-commit-id", "--name-only", "-r", sha).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git diff-tree: %s", out)
	return strings.TrimSpace(string(out)) == ""
}

func runGitIn(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return nil
}
