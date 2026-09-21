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

		By("and it does not keep committing: one refusal is one commit")
		Consistently(func(g Gomega) {
			g.Expect(remoteCommitCount(repo.CheckoutDir)).To(Equal(seedCount+1),
				"a single refusal must not produce a stream of commits")
		}, 20*time.Second, 4*time.Second).Should(Succeed())
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
