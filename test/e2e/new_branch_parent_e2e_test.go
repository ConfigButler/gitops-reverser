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

// A GitTarget whose write branch does not exist yet stays on standby, and its first commit lands
// on the parent branch's CURRENT tip.
//
// The write branch is created from the remote's default branch (its parent) only when a write has
// something to commit. Until then nothing is pushed, including for a live event the parent
// already holds. When a write does commit, publication checks the parent against the push
// session's advertisement, so a worker that checked out the parent long ago still creates the
// branch on the parent's tip rather than on its own stale checkout.
//
// The stale checkout is the whole point, so the spec builds it deliberately: the target's own
// initial snapshot clones main at A and trusts it before B is pushed. A cold worker would fetch on
// its first write and land on B without exercising the check at all.
//
// The spec asserts ancestry and content, not which fetch found B: with periodic refresh on, a
// refresh can legitimately find B before the publication does. That publication alone finds a
// moved parent, with no refresher at all, is proved deterministically by the worker tests
// TestBranchWorker_LiveWriteOnAbsentBranchStartsFromTheParentsCurrentTip and
// TestBranchWorker_NewBranchIsNotPublishedOnAnUncheckedParent.
//
// It runs twice: once with spec.parentBranch omitted (the remote's default branch, main), and once
// naming `release`, which the GitProvider does not allow writing to because it is only read. A
// third context names a parent that does not exist yet, and recovers once it is pushed.
var _ = Describe("New write branch starts from its parent", Label("manager"), func() {
	Context("with spec.parentBranch omitted", Ordered, func() {
		describeNewWriteBranchSpec("", "main", "parent-branch")
	})
	Context("with spec.parentBranch naming release", Ordered, func() {
		describeNewWriteBranchSpec("release", "release", "parent-release")
	})
	Context("with spec.parentBranch naming a branch that does not exist yet", Ordered, func() {
		describeMissingParentRecoverySpec()
	})
})

const parentBranchSelectorLabel = "e2e.configbutler.ai/parent-branch"

// applyParentConfigMap applies a ConfigMap the parent-branch specs' WatchRule selects.
func applyParentConfigMap(namespace, name string) {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
  labels:
    %s: "yes"
data:
  key: %s
`, name, namespace, parentBranchSelectorLabel, name)
	out, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "apply ConfigMap %q: %s", name, out)
}

// applyParentSpecProvider declares the GitProvider the parent-branch specs write through, allowing
// main and the write branch only: a parent other than main is read, never written.
func applyParentSpecProvider(namespace, providerName string, repo *RepoArtifacts, writeBranch string) {
	GinkgoHelper()
	provider := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitProvider
metadata:
  name: %s
  namespace: %s
spec:
  url: %s
  allowedBranches: ["main", %q]
  secretRef:
    name: %s
`, providerName, namespace, repo.RepoURLHTTP, writeBranch, repo.GitSecretHTTP)
	out, err := kubectlRunWithStdin(namespace, provider, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "apply GitProvider: %s", out)
	verifyResourceStatus("gitprovider", providerName, namespace, "True", "Succeeded", "")
}

// applyParentSpecWatchRule declares the WatchRule that selects the specs' labelled ConfigMaps.
func applyParentSpecWatchRule(namespace, ruleName, destName string) {
	GinkgoHelper()
	rule := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: %s
  namespace: %s
spec:
  gitTargetRef:
    name: %s
  rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    objectSelector:
      matchLabels:
        %s: "yes"
`, ruleName, namespace, destName, parentBranchSelectorLabel)
	out, err := kubectlRunWithStdin(namespace, rule, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "apply WatchRule: %s", out)
}

// describeMissingParentRecoverySpec: a configured parent that does not exist refuses, writes
// nothing and creates no branch. Once the parent is pushed, the target recovers with no further
// cluster edit: the branch worker finds the parent on its own probe, asks for a fresh snapshot,
// and the edit made while the parent was missing lands on the parent's tip.
func describeMissingParentRecoverySpec() {
	const (
		providerName = "missing-parent-provider"
		destName     = "missing-parent-dest"
		ruleName     = "missing-parent-rule"
		gitPath      = "apps/missing-parent"
	)
	var (
		testNs      string
		repo        *RepoArtifacts
		writeBranch string
	)

	BeforeAll(func() {
		testNs = testNamespaceFor("manager-missing-parent")
		_, _ = kubectlRun("create", "namespace", testNs)
		writeBranch = fmt.Sprintf("reverser-e2e-%d", GinkgoRandomSeed())
		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-missing-parent-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)
		applyParentSpecProvider(testNs, providerName, repo, writeBranch)
	})

	AfterAll(func() {
		cleanupWatchRule(ruleName, testNs)
		cleanupGitTarget(destName, testNs)
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
		cleanupNamespace(testNs)
	})

	It("refuses while the parent is missing, then recovers on its own once it is pushed", func() {
		commitFilesToBranchFromOutside(repo, testNs, "main", "e2e: seed main", map[string]string{
			"README.md": "main is not the parent here\n",
		})
		createGitTargetWithParentBranch(destName, testNs, providerName, gitPath, writeBranch, "release")
		applyParentSpecWatchRule(testNs, ruleName, destName)

		By("an edit made while release does not exist is not written")
		applyParentConfigMap(testNs, "made-while-missing")
		Eventually(func(g Gomega) {
			g.Expect(readyReasonOf(g, destName, testNs)).To(Equal("ParentBranchNotFound"))
		}, 3*time.Minute, 3*time.Second).Should(Succeed())
		Expect(remoteBranchHeadOf(Default, repo.CheckoutDir, writeBranch)).To(BeEmpty(), "no orphan branch")

		By("release is pushed from outside; nothing in the cluster changes after this")
		releaseTip := commitFilesToBranchFromOutside(repo, testNs, "release", "e2e: release appears",
			map[string]string{"RELEASE.md": "release\n"})

		By("the write branch is created on release's tip, carrying the edit made while it was missing")
		editPath := path.Join(gitPath, testNs, "configmaps", "made-while-missing.yaml")
		Eventually(func(g Gomega) {
			tip := remoteBranchHeadOf(g, repo.CheckoutDir, writeBranch)
			g.Expect(tip).NotTo(BeEmpty(), "the recovery creates the write branch")
			_, fetchErr := gitRun(repo.CheckoutDir, "fetch", "origin", writeBranch)
			g.Expect(fetchErr).NotTo(HaveOccurred())
			first, revErr := gitRun(repo.CheckoutDir, "rev-list", "--max-parents=1", "--reverse",
				"origin/release.."+tip)
			g.Expect(revErr).NotTo(HaveOccurred())
			commits := strings.Fields(first)
			g.Expect(commits).NotTo(BeEmpty())
			parentOfFirst, revErr := gitRun(repo.CheckoutDir, "rev-parse", commits[0]+"^")
			g.Expect(revErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(parentOfFirst)).To(Equal(releaseTip),
				"the branch's first commit sits on release's tip")
			_, showErr := gitRun(repo.CheckoutDir, "cat-file", "-e", tip+":"+editPath)
			g.Expect(showErr).NotTo(HaveOccurred(), "the edit made while release was missing is on the branch")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(readyReasonOf(g, destName, testNs)).NotTo(
				BeElementOf("ParentBranchNotFound", "RecoveringParentBranch"))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())
	})
}

func describeNewWriteBranchSpec(parentBranch, parent, slug string) {
	const (
		providerName = "parent-provider"
		destName     = "parent-dest"
		ruleName     = "parent-rule"
		gitPath      = "apps/parent"
	)
	var (
		testNs      string
		repo        *RepoArtifacts
		writeBranch string
	)

	applyConfigMap := func(name string) {
		GinkgoHelper()
		applyParentConfigMap(testNs, name)
	}

	// mirroredDocument is the document the controller writes for applyConfigMap(name).
	mirroredDocument := func(name string) string {
		return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  labels:
    %s: "yes"
  name: %s
  namespace: %s
data:
  key: %s
`, parentBranchSelectorLabel, name, testNs, name)
	}

	branchSeries := func(metric, extra string) string {
		return fmt.Sprintf(`sum(%s{provider_namespace=%q,branch=%q%s}) or vector(0)`,
			metric, testNs, writeBranch, extra)
	}
	pushes := func() float64 {
		GinkgoHelper()
		v, err := queryPrometheus(branchSeries("gitopsreverser_git_pushes_total", `,outcome="pushed"`))
		Expect(err).NotTo(HaveOccurred())
		return v
	}

	BeforeAll(func() {
		ensurePrometheusClient()
		testNs = testNamespaceFor("manager-" + slug)
		_, _ = kubectlRun("create", "namespace", testNs)
		writeBranch = fmt.Sprintf("reverser-e2e-%d", GinkgoRandomSeed())

		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-%s-%d", slug, GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		applyParentSpecProvider(testNs, providerName, repo, writeBranch)
	})

	AfterAll(func() {
		cleanupWatchRule(ruleName, testNs)
		cleanupGitTarget(destName, testNs)
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
		cleanupNamespace(testNs)
	})

	It("stays on standby, then creates the write branch on the parent's current tip", func() {
		By("seeding " + parent + " with commit A, whose folder already holds a document the cluster will match")
		if parent != "main" {
			commitFilesToBranchFromOutside(repo, testNs, "main", "e2e: seed main", map[string]string{
				"README.md": "main is not the parent here\n",
			})
		}
		hashA := commitFilesToBranchFromOutside(repo, testNs, parent, "e2e: seed A", map[string]string{
			"README.md": "seed\n",
			path.Join(gitPath, testNs, "configmaps", "standby-present.yaml"): mirroredDocument("standby-present"),
		})

		By("creating a GitTarget on an absent write branch, and a WatchRule that selects nothing yet")
		createGitTargetWithParentBranch(destName, testNs, providerName, gitPath, writeBranch, parentBranch)
		applyParentSpecWatchRule(testNs, ruleName, destName)
		verifyResourceStatus("watchrule", ruleName, testNs, "True", "Succeeded", "")
		waitForWatchRuleStreamsRunning(ruleName, testNs)

		// The initial snapshot is an in-sync no-op that still clones: this is the worker checking
		// out A and trusting it, which is the stale state the rest of the spec depends on.
		waitForMetricWithTimeout(branchSeries("gitopsreverser_git_fetches_total", ""),
			func(v float64) bool { return v > 0 },
			"the worker for the write branch checked out the parent", 2*time.Minute)

		By("status.remote names the parent the write branch would be created from")
		Eventually(func(g Gomega) {
			g.Expect(gitTargetRemoteField(g, destName, testNs, "commit")).To(BeEmpty(), "the write branch is absent")
			g.Expect(gitTargetRemoteField(g, destName, testNs, "parent.state")).To(Equal("Found"))
			g.Expect(gitTargetRemoteField(g, destName, testNs, "parent.branch")).To(Equal(parent),
				"with spec.parentBranch omitted this is the default branch the remote resolved")
			g.Expect(gitTargetRemoteField(g, destName, testNs, "parent.commit")).To(Equal(hashA))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("an idle, in-sync target creates no branch")
		Consistently(func(g Gomega) {
			g.Expect(remoteBranchHeadOf(g, repo.CheckoutDir, writeBranch)).To(BeEmpty(),
				"idle means no branch and no commit")
		}, 15*time.Second, 3*time.Second).Should(Succeed())

		By("a live event the parent already holds publishes nothing")
		pushesBefore := pushes()
		applyConfigMap("standby-present")
		// The no-diff window is still pushed, because only the remote can confirm "already
		// present". Wait for that push cycle to finish before judging the branch.
		waitForMetricWithTimeout(branchSeries("gitopsreverser_git_pushes_total", `,outcome="pushed"`),
			func(v float64) bool { return v > pushesBefore },
			"the no-diff window reached the remote", 2*time.Minute)
		Expect(remoteBranchHeadOf(Default, repo.CheckoutDir, writeBranch)).To(BeEmpty(),
			"a write that commits nothing must not create the write branch")

		By(parent + " moves to B from outside, touching the target's folder")
		hashB := commitFilesToBranchFromOutside(repo, testNs, parent, "e2e: B moves the parent", map[string]string{
			path.Join(gitPath, "NOTES.md"): "added on the parent at B\n",
		})

		By("a live edit arrives with no resync in between")
		applyConfigMap("standby-edit")

		By("the write branch is created on B and carries the edit")
		editPath := path.Join(gitPath, testNs, "configmaps", "standby-edit.yaml")
		Eventually(func(g Gomega) {
			tip := remoteBranchHeadOf(g, repo.CheckoutDir, writeBranch)
			g.Expect(tip).NotTo(BeEmpty(), "the edit creates the write branch")
			_, fetchErr := gitRun(repo.CheckoutDir, "fetch", "origin", writeBranch)
			g.Expect(fetchErr).NotTo(HaveOccurred())
			parent, revErr := gitRun(repo.CheckoutDir, "rev-parse", tip+"^")
			g.Expect(revErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(parent)).To(Equal(hashB),
				"the first commit must sit on the parent's current tip")
			for _, file := range []string{path.Join(gitPath, "NOTES.md"), editPath} {
				_, showErr := gitRun(repo.CheckoutDir, "cat-file", "-e", tip+":"+file)
				g.Expect(showErr).NotTo(HaveOccurred(), "%s must be on the new branch", file)
			}
		}, 2*time.Minute, 3*time.Second).Should(Succeed())
	})
}

// commitFilesToBranchFromOutside commits files to a branch and pushes it directly, the way another
// writer would, and returns the new tip. A branch the remote does not carry yet starts from main,
// or from nothing in an empty repository. It retries because losing a push race with the
// controller says nothing about the behavior under test.
func commitFilesToBranchFromOutside(
	repo *RepoArtifacts, namespace, branch, message string, files map[string]string,
) string {
	GinkgoHelper()
	configureRepoOriginWithCredentials(repo, namespace)

	Eventually(func() error {
		return attemptCommitFilesToBranch(repo, branch, message, files)
	}, seedPushTimeout, seedPushInterval).Should(Succeed(), "the outside writer kept losing the push race")

	head, err := gitRun(repo.CheckoutDir, "rev-parse", "HEAD")
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(head)
}

// attemptCommitFilesToBranch is one attempt, from the CURRENT remote tip. It returns errors rather
// than asserting, so Eventually can rebuild on the tip that beat it.
func attemptCommitFilesToBranch(repo *RepoArtifacts, branch, message string, files map[string]string) error {
	runGit := func(args ...string) error {
		if out, err := gitRun(repo.CheckoutDir, args...); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
		}
		return nil
	}
	if err := checkoutRemoteTipOf(repo, branch, runGit); err != nil {
		return err
	}
	for name, content := range files {
		full := filepath.Join(repo.CheckoutDir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			return err
		}
		if err := runGit("add", name); err != nil {
			return err
		}
	}
	if err := runGit("commit", "-m", message); err != nil {
		return err
	}
	return runGit("push", "origin", "HEAD:refs/heads/"+branch)
}

// checkoutRemoteTipOf checks out branch at its remote tip; a branch the remote does not carry
// starts from main, and from an orphan when main is absent too.
func checkoutRemoteTipOf(repo *RepoArtifacts, branch string, runGit func(...string) error) error {
	for _, start := range []string{branch, "main"} {
		if _, err := gitRun(repo.CheckoutDir, "fetch", "origin", start); err != nil {
			continue
		}
		if err := runGit("checkout", "-B", branch, "origin/"+start); err != nil {
			return err
		}
		return runGit("reset", "--hard", "origin/"+start)
	}
	if err := runGit("checkout", "--orphan", branch); err != nil {
		return err
	}
	_, _ = gitRun(repo.CheckoutDir, "rm", "-rf", ".")
	return nil
}
