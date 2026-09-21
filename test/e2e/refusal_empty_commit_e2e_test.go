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

// GitTarget spec.onRefusal: PushEmptyCommit, with a REAL refusal.
//
// The refusal has to be the right KIND, and choosing it is most of what this spec is about. A
// folder-level refusal (a foreign file, unparseable YAML) is normally found by the per-type
// reconcile, which blocks the cell through the event router and never reaches the live-write path
// the action hangs off. It is also the case where an empty commit achieves nothing: only a human
// can repair a folder.
//
// The case the action exists for is a WRITE-BOUNDARY refusal, where the folder is accepted and
// this one edit had nowhere to land. Shape 8 of the layout corpus is exactly that: an overlay at
// apps/checkout/overlays/prod whose Deployment comes from ../../base, and a live edit to an env
// var, which no kustomize declaration in the overlay can express. Read scope reaches the base;
// write scope never does.
//
// The other half of the loop, that a new revision alone makes Flux re-apply and revert, is proven
// against real Flux in the bi-directional corner. The seam between the two specs is a commit on
// the branch with an empty diff: this one proves the operator produces it, that one proves Flux
// acts on it. Splitting there is deliberate, because a single spec would need real Flux driving a
// kustomize overlay to say anything this pair does not.
var _ = Describe("Manager Refusal Empty Commit", Label("manager", "refusal-commit"), Ordered, func() {
	var (
		testNs       string
		repo         *RepoArtifacts
		providerName = "refusal-commit-provider"
		destName     = "refusal-commit-dest"
		ruleName     = "refusal-commit-rule"
		gitPath      = "apps/checkout/overlays/prod"
	)

	BeforeAll(func() {
		By("creating the test namespace")
		testNs = testNamespaceFor("manager-refusal-commit")
		_, _ = kubectlRun("create", "namespace", testNs)

		By("setting up Gitea repo and credentials")
		repo = SetupRepo(resolveE2EContext(), testNs,
			fmt.Sprintf("e2e-refusal-commit-%d", GinkgoRandomSeed()))
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
		cleanupNamespace(testNs)
	})

	It("commits an empty commit when the live object has nowhere in Git to land", func() {
		By("seeding a base-and-overlay folder that renders into this test's namespace")
		seedFolder := writeBaseOwnedFieldFolder(testNs)
		DeferCleanup(func() { _ = os.RemoveAll(seedFolder) })
		seedRenderedFolderIntoRepo(repo, testNs, seedFolder, "apps/checkout")

		// Applied with the env var ALREADY diverged from what the folder renders. The divergence is
		// the point: an empty commit asks the reconciler to re-apply, and without something to
		// correct the commit would be asking for nothing.
		By("applying the rendered Deployment, drifted from what Git says")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", filepath.Join(seedFolder, "base", "deployment.yaml"))
		Expect(err).NotTo(HaveOccurred(), "failed to apply the base Deployment")
		_, err = kubectlRunInNamespace(testNs, "set", "env", "deployment/checkout", "LOG_LEVEL=debug")
		Expect(err).NotTo(HaveOccurred(), "failed to drift the base-owned env var")

		seedCount := remoteCommitCount(repo.CheckoutDir)

		By("creating the GitTarget on the OVERLAY, asking it to commit on refusal")
		createGitTarget(destName, testNs, providerName, gitPath, "main")
		_, err = kubectlRunInNamespace(testNs, "patch", "gittarget", destName,
			"--type=merge", "-p", `{"spec":{"onRefusal":"PushEmptyCommit"}}`)
		Expect(err).NotTo(HaveOccurred(), "failed to set spec.onRefusal")

		// The overlay renders into THIS namespace, so the rule watches its own and needs no
		// sourceNamespace override (which would need authorizing, and would be testing that
		// instead of this).
		Expect(applyFromTemplate("test/e2e/templates/manager/watchrule-resources.tmpl", struct {
			Name            string
			Namespace       string
			DestinationName string
			Resources       string
		}{Name: ruleName, Namespace: testNs, DestinationName: destName, Resources: `"deployments"`},
			testNs)).To(Succeed(), "failed to apply the Deployment WatchRule")

		By("the write is refused at the BOUNDARY, which is the kind that earns a commit")
		waitForGitTargetGitPathRefused(destName, testNs, "WriteBoundaryRefused")
		verifyResourceCondition("gittarget", destName, testNs, "GitPathAccepted", "False",
			"WriteBoundaryRefused", "escapes the GitTarget write scope")

		By("the operator commits a commit that changes no file")
		var head string
		Eventually(func(g Gomega) {
			g.Expect(remoteCommitCount(repo.CheckoutDir)).To(BeNumerically(">", seedCount),
				"a refused write on an opted-in target must produce a commit")
			head = remoteHead(g, repo.CheckoutDir)
			g.Expect(emptyDiffAtSHA(repo.CheckoutDir, head)).To(BeTrue(),
				"the operator's commit must change no file; a refused write writes nothing")
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		Expect(commitMessageAtSHA(repo.CheckoutDir, head)).To(ContainSubstring(destName),
			"the commit must name the GitTarget whose write was refused, or git log reads as a stray no-op")

		// Past TWO full trailing intervals, not one. A window that ends inside the second interval
		// only proves there was no immediate repeat, which the rate limit guarantees for free; a
		// commit-per-interval loop is not visible until the third commit would be due. The refusal
		// is still standing throughout, and a forced recheck re-observes it every minute, so this
		// is the worst case the dedupe exists for: rechecking an unchanged refusal must not keep
		// moving the branch.
		//
		// The bound is the initial commit plus one trailing commit. Two, not one, because the live
		// write and the per-type reconcile observe the same refusal from different populations, so
		// the second of them can legitimately be a first observation of its own. What must never
		// grow is the count per interval after that.
		By("and it settles past two trailing intervals instead of committing on a loop")
		Consistently(func(g Gomega) {
			g.Expect(remoteCommitCount(repo.CheckoutDir)).To(BeNumerically("<=", seedCount+2),
				"a standing refusal must not produce a commit per interval forever")
		}, 150*time.Second, 10*time.Second).Should(Succeed())

		// A quiet branch on its own is two readings, and only one of them is the feature: the
		// refusal was recognised as one already answered, or the target stopped evaluating
		// anything at all. A review asked for the second to be ruled out. Editing the SAME field
		// to a NEW value does that and covers the dedupe's other half at the same time — a second
		// real edit is a second thing to revert, and the commit made for the first says nothing
		// about it.
		//
		// The target is NOT asked to converge, because this shape cannot: the live object carries
		// the API server's defaults and the base document does not, so a write for it is planned,
		// and refused, whatever the env var says. Correcting the value would leave the refusal
		// exactly where it is. What is provable here is that the operator keeps evaluating and
		// still distinguishes a new edit from a repeat.
		By("editing the same field again, to a value nobody has been refused for yet")
		beforeSecondEdit := remoteCommitCount(repo.CheckoutDir)
		_, err = kubectlRunInNamespace(testNs, "set", "env", "deployment/checkout", "LOG_LEVEL=trace")
		Expect(err).NotTo(HaveOccurred(), "failed to make the second drifting edit")

		By("which earns a commit of its own")
		Eventually(func(g Gomega) {
			g.Expect(remoteCommitCount(repo.CheckoutDir)).To(BeNumerically(">", beforeSecondEdit),
				"a genuinely new refused edit must still move the branch")
			g.Expect(emptyDiffAtSHA(repo.CheckoutDir, remoteHead(g, repo.CheckoutDir))).To(BeTrue(),
				"and it must still be a commit that changes no file")
		}, 120*time.Second, 2*time.Second).Should(Succeed())

		By("and then settles again, rather than resuming the loop")
		settled := remoteCommitCount(repo.CheckoutDir)
		Consistently(func(g Gomega) {
			g.Expect(remoteCommitCount(repo.CheckoutDir)).To(BeNumerically("<=", settled+1),
				"a second standing refusal must settle exactly as the first one did")
		}, 150*time.Second, 10*time.Second).Should(Succeed())
	})
})

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

func commitMessageAtSHA(checkoutDir, sha string) string {
	GinkgoHelper()
	out, err := exec.Command("git", "-C", checkoutDir, "log", "-1", "--format=%B", sha).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git log: %s", out)
	return string(out)
}

func runGitIn(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return nil
}
