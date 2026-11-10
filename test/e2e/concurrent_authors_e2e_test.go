// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Concurrent Authors is the flagship of docs/design/concurrent-author-windows.md: two people work on
// one GitTarget at the same time, each on objects of their own, and each saves when done. Every
// write goes through the real path (kubectl as that person → kube-apiserver → audit webhook and
// watch → attribution → branch worker), and each save must come out as one commit of its own:
// its author, its message, and exactly that person's files.
//
// The GitTarget's window is long, so no timer closes a window during the spec: only the saves do.
// Before concurrent windows, bob's first write closed alice's window (another author), so alice's
// first Deployment was committed alone under a generated message, and her save carried only what
// she wrote after bob. Both the quiet wait and the commit contents below would catch that.
//
// The branch is seeded first: a second window opens beside the first only once the folder's
// bootstrap files are committed, so on an empty repository the first save would be serial anyway.
//
// Not Serial: the spec owns its namespace and Gitea repository.
var _ = Describe("Concurrent Authors", Label("concurrent-authors", "commit-request", "audit-consumer"), Ordered,
	func() {
		const (
			basePath     = "e2e/concurrent-authors"
			commitWindow = "120s"
		)
		var p windowedDeploymentPipeline

		BeforeAll(func() {
			p = setupWindowedDeploymentPipeline("concurrent-authors", basePath, commitWindow)
		})

		AfterAll(func() {
			cleanupPipeline(p.ns, p.gitProvider, p.gitTarget, p.watchRule)
			cleanupNamespace(p.ns)
		})

		It("gives two people working at the same time one commit each", func() {
			seed := GinkgoRandomSeed()
			alice := author{user: "alice", message: "feat: add the storefront"}
			bob := author{user: "bob", message: "feat: add the order queue"}
			aliceAPI := fmt.Sprintf("storefront-api-%d", seed)
			aliceWeb := fmt.Sprintf("storefront-web-%d", seed)
			bobQueue := fmt.Sprintf("order-queue-%d", seed)
			bobCron := fmt.Sprintf("order-cleanup-%d", seed)
			file := func(name string) string {
				return path.Join(basePath, p.ns, "apps/deployments", name+".yaml")
			}

			By("seeding the branch, so the folder's bootstrap files are committed")
			seedName := fmt.Sprintf("seed-%d", seed)
			var bundle strings.Builder
			bundle.WriteString(commitRequestManifest(p.ns, "save-seed", p.gitTarget, "chore: seed", "8s"))
			bundle.WriteString("---\n")
			bundle.WriteString(deploymentManifest(p.ns, seedName))
			_, err := kubectlRunWithStdin(p.ns, bundle.String(), "apply", "-f", "-")
			Expect(err).NotTo(HaveOccurred())
			var seedSHA string
			Eventually(func(g Gomega) {
				seedSHA = expectCommitRequestCommitted(g, p.ns, "save-seed",
					recentCommitDiagnostics(p.repo.CheckoutDir, basePath))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("alice and bob each start work, interleaved")
			alice.create(p.ns, aliceAPI)
			bob.create(p.ns, bobQueue)
			alice.create(p.ns, aliceWeb)
			bob.create(p.ns, bobCron)
			alice.label(p.ns, aliceAPI, "tier=frontend")

			By("nothing is committed while both are still working")
			Consistently(func(g Gomega) {
				pullLatestRepoState(g, p.repo.CheckoutDir)
				g.Expect(headOf(g, p.repo.CheckoutDir)).To(Equal(seedSHA),
					"a window closed before its author saved\n%s",
					recentCommitDiagnostics(p.repo.CheckoutDir, basePath))
			}, 10*time.Second, 2*time.Second).Should(Succeed())

			By("alice saves, then bob saves")
			alice.save(p.ns, "save-storefront", p.gitTarget)
			bob.save(p.ns, "save-order-queue", p.gitTarget)

			var aliceSHA, bobSHA string
			Eventually(func(g Gomega) {
				diagnostics := recentCommitDiagnostics(p.repo.CheckoutDir, basePath)
				aliceSHA = expectCommitRequestCommitted(g, p.ns, "save-storefront", diagnostics)
				bobSHA = expectCommitRequestCommitted(g, p.ns, "save-order-queue", diagnostics)
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("each save is one commit: its author, its message, its files and nothing else")
			Eventually(func(g Gomega) {
				pullLatestRepoState(g, p.repo.CheckoutDir)
				diagnostics := recentCommitDiagnostics(p.repo.CheckoutDir, basePath)
				g.Expect(commitsSince(g, p.repo.CheckoutDir, seedSHA)).To(ConsistOf(aliceSHA, bobSHA),
					"exactly the two saves were committed after the seed\n%s", diagnostics)

				alice.expectCommit(g, p.repo.CheckoutDir, aliceSHA, file(aliceAPI), file(aliceWeb))
				bob.expectCommit(g, p.repo.CheckoutDir, bobSHA, file(bobQueue), file(bobCron))

				g.Expect(fileAt(g, p.repo.CheckoutDir, aliceSHA, file(aliceAPI))).To(ContainSubstring("tier: frontend"),
					"alice's update after bob's writes is in her commit")
			}, time.Minute, 2*time.Second).Should(Succeed())
		})

		// Three windows at once, saved in the reverse of the order they opened: commit order is close
		// order. Each save is awaited before the next, so the order is the spec's, not a race between
		// timers. Carol's commit lands while alice's and bob's windows stay open, and alice writes again
		// after it, so her window is proven again against the tree carol's commit made.
		It("keeps three windows open side by side, and commits each when its author saves", func() {
			seed := GinkgoRandomSeed()
			alice := author{user: "alice", message: "feat: add the checkout"}
			bob := author{user: "bob", message: "feat: add the payment worker"}
			carol := author{user: "carol", message: "feat: add the mailer"}
			aliceAPI := fmt.Sprintf("checkout-api-%d", seed)
			aliceWeb := fmt.Sprintf("checkout-web-%d", seed)
			bobWorker := fmt.Sprintf("payment-worker-%d", seed)
			carolMailer := fmt.Sprintf("mailer-%d", seed)
			file := func(name string) string {
				return path.Join(basePath, p.ns, "apps/deployments", name+".yaml")
			}
			base := pulledHead(p.repo.CheckoutDir)

			By("alice, bob and carol each start work, interleaved")
			alice.create(p.ns, aliceAPI)
			bob.create(p.ns, bobWorker)
			carol.create(p.ns, carolMailer)
			alice.create(p.ns, aliceWeb)

			By("the branch worker holds three windows open, and commits nothing")
			expectOpenWindows(p.ns, 3)
			Consistently(func(g Gomega) {
				pullLatestRepoState(g, p.repo.CheckoutDir)
				g.Expect(headOf(g, p.repo.CheckoutDir)).To(Equal(base),
					"a window closed before its author saved\n%s",
					recentCommitDiagnostics(p.repo.CheckoutDir, basePath))
			}, 6*time.Second, 2*time.Second).Should(Succeed())

			By("carol, who started last, saves first; alice's and bob's windows stay open")
			carol.save(p.ns, "save-mailer", p.gitTarget)
			carolSHA := awaitSave(p, "save-mailer")
			Eventually(func(g Gomega) {
				pullLatestRepoState(g, p.repo.CheckoutDir)
				g.Expect(commitsSince(g, p.repo.CheckoutDir, base)).To(ConsistOf(carolSHA),
					"only carol's save is committed\n%s", recentCommitDiagnostics(p.repo.CheckoutDir, basePath))
			}, 30*time.Second, 2*time.Second).Should(Succeed())
			expectOpenWindows(p.ns, 2)

			By("alice writes again after carol's commit, and her write joins her open window")
			alice.label(p.ns, aliceAPI, "tier=frontend")

			By("bob saves, then alice")
			bob.save(p.ns, "save-payment-worker", p.gitTarget)
			bobSHA := awaitSave(p, "save-payment-worker")
			alice.save(p.ns, "save-checkout", p.gitTarget)
			aliceSHA := awaitSave(p, "save-checkout")

			By("Git holds three commits in the order the windows closed, each one person's save")
			Eventually(func(g Gomega) {
				pullLatestRepoState(g, p.repo.CheckoutDir)
				g.Expect(commitsOldestFirst(g, p.repo.CheckoutDir, base)).
					To(Equal([]string{carolSHA, bobSHA, aliceSHA}), "commit order is close order\n%s",
						recentCommitDiagnostics(p.repo.CheckoutDir, basePath))
				carol.expectCommit(g, p.repo.CheckoutDir, carolSHA, file(carolMailer))
				bob.expectCommit(g, p.repo.CheckoutDir, bobSHA, file(bobWorker))
				alice.expectCommit(g, p.repo.CheckoutDir, aliceSHA, file(aliceAPI), file(aliceWeb))
				g.Expect(fileAt(g, p.repo.CheckoutDir, aliceSHA, file(aliceAPI))).To(ContainSubstring("tier: frontend"),
					"alice's write after carol's commit is in alice's commit")
			}, time.Minute, 2*time.Second).Should(Succeed())
		})

		// The footprint rule: bob edits a Deployment alice created in her still-open window. If both
		// windows stayed open and bob's closed first, alice's commit would put her older copy over
		// bob's. So bob's write closes alice's window first (path_conflict): her commit lands first,
		// under a generated message, and bob's save carries the newer copy, which Git ends with.
		It("closes the other author's window first when one edits a file in it", func() {
			seed := GinkgoRandomSeed()
			alice := author{user: "alice"}
			bob := author{user: "bob", message: "fix: label the shared cache"}
			aliceCache := fmt.Sprintf("shared-cache-%d", seed)
			bobIndexer := fmt.Sprintf("indexer-%d", seed)
			file := func(name string) string {
				return path.Join(basePath, p.ns, "apps/deployments", name+".yaml")
			}
			base := pulledHead(p.repo.CheckoutDir)

			By("alice and bob each open a window on a file of their own")
			alice.create(p.ns, aliceCache)
			bob.create(p.ns, bobIndexer)
			expectOpenWindows(p.ns, 2)

			By("bob edits alice's Deployment, which closes her window first, and saves")
			bob.label(p.ns, aliceCache, "owner=bob")
			bob.save(p.ns, "save-indexer", p.gitTarget)
			bobSHA := awaitSave(p, "save-indexer")

			By("alice's commit lands first, with her copy; bob's lands after it, with his")
			Eventually(func(g Gomega) {
				pullLatestRepoState(g, p.repo.CheckoutDir)
				diagnostics := recentCommitDiagnostics(p.repo.CheckoutDir, basePath)
				commits := commitsOldestFirst(g, p.repo.CheckoutDir, base)
				g.Expect(commits).To(HaveLen(2), "alice's window and bob's save\n%s", diagnostics)
				g.Expect(commits[1]).To(Equal(bobSHA), "bob's save lands after alice's commit\n%s", diagnostics)
				aliceSHA := commits[0]

				authorOf, err := gitRun(p.repo.CheckoutDir, "show", "-s", "--format=%an", aliceSHA)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(authorOf)).To(Equal(alice.user), "the first commit is alice's window")
				g.Expect(changedFiles(g, p.repo.CheckoutDir, aliceSHA)).To(Equal([]string{file(aliceCache)}))
				aliceCopy := fileAt(g, p.repo.CheckoutDir, aliceSHA, file(aliceCache))
				g.Expect(aliceCopy).NotTo(ContainSubstring("owner: bob"),
					"alice's commit holds her copy, from before bob's edit")

				bob.expectCommit(g, p.repo.CheckoutDir, bobSHA, file(aliceCache), file(bobIndexer))
				g.Expect(fileAt(g, p.repo.CheckoutDir, "HEAD", file(aliceCache))).To(ContainSubstring("owner: bob"),
					"Git ends with bob's newer copy")
			}, time.Minute, 2*time.Second).Should(Succeed())

			By("the forced close is counted as a path_conflict under this GitTarget")
			ensurePrometheusClient()
			waitForMetric(
				fmt.Sprintf(`sum(gitopsreverser_git_commit_windows_total{close_reason="path_conflict",`+
					`gittarget_namespace=%q,gittarget_name=%q}) or vector(0)`, p.ns, p.gitTarget),
				func(v float64) bool { return v >= 1 },
				"alice's window closed with path_conflict",
			)
		})
	})

// awaitSave waits for the named CommitRequest to be committed and pushed, and returns its commit.
func awaitSave(p windowedDeploymentPipeline, name string) string {
	GinkgoHelper()
	var sha string
	Eventually(func(g Gomega) {
		sha = expectCommitRequestCommitted(g, p.ns, name, recentCommitDiagnostics(p.repo.CheckoutDir, p.basePath))
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	return sha
}

// expectOpenWindows waits for the branch worker of namespace's GitBranch to report n commit windows
// open at once (git_commit_windows_open, read on a 5s scrape).
func expectOpenWindows(namespace string, n int) {
	GinkgoHelper()
	ensurePrometheusClient()
	waitForMetric(
		fmt.Sprintf(`max(gitopsreverser_git_commit_windows_open{gitbranch_namespace=%q}) or vector(0)`, namespace),
		func(v float64) bool { return v == float64(n) },
		fmt.Sprintf("%d commit windows open side by side", n),
	)
}

// pulledHead pulls the checkout and returns its HEAD.
func pulledHead(checkoutDir string) string {
	GinkgoHelper()
	var head string
	Eventually(func(g Gomega) {
		pullLatestRepoState(g, checkoutDir)
		head = headOf(g, checkoutDir)
	}, 30*time.Second, 2*time.Second).Should(Succeed())
	return head
}

// author is a person working through kubectl as themselves: every write is impersonated, so the
// audit event names them, and system:masters spares each spec its own RBAC.
type author struct {
	user    string
	message string
}

func (a author) kubectl(namespace, stdin string, args ...string) {
	GinkgoHelper()
	args = append(args, "--as="+a.user, "--as-group=system:masters")
	out, err := kubectlRunWithStdin(namespace, stdin, args...)
	Expect(err).NotTo(HaveOccurred(), "kubectl %v as %s: %s", args, a.user, out)
}

func (a author) create(namespace, name string) {
	GinkgoHelper()
	a.kubectl(namespace, deploymentManifest(namespace, name), "create", "-f", "-")
}

func (a author) label(namespace, name, label string) {
	GinkgoHelper()
	a.kubectl(namespace, "", "label", "deployment", name, label)
}

// save creates the person's CommitRequest. Its window attaches to the window their writes opened
// (attach: CurrentOrNext) and closes it 8s later.
func (a author) save(namespace, name, gitTarget string) {
	GinkgoHelper()
	a.kubectl(namespace, commitRequestManifest(namespace, name, gitTarget, a.message, "8s"), "create", "-f", "-")
}

// expectCommit asserts sha is this person's save: their name and address as the audit event gave
// them, their message verbatim, and exactly the files they wrote.
func (a author) expectCommit(g Gomega, checkoutDir, sha string, files ...string) {
	out, err := gitRun(checkoutDir, "show", "-s", "--format=%an <%ae>%n%B", sha)
	g.Expect(err).NotTo(HaveOccurred())
	authorLine, message, _ := strings.Cut(out, "\n")
	g.Expect(authorLine).To(Equal(fmt.Sprintf("%s <%s@noreply.cluster.local>", a.user, a.user)),
		"commit %s is %s's", sha[:8], a.user)
	g.Expect(strings.TrimSpace(message)).To(Equal(a.message), "commit %s carries %s's message", sha[:8], a.user)

	want := slices.Sorted(slices.Values(files))
	g.Expect(changedFiles(g, checkoutDir, sha)).To(Equal(want), "commit %s changes only %s's files", sha[:8], a.user)
}

// changedFiles lists the files commit sha changes, sorted.
func changedFiles(g Gomega, checkoutDir, sha string) []string {
	out, err := gitRun(checkoutDir, "show", "--name-only", "--format=", sha)
	g.Expect(err).NotTo(HaveOccurred())
	return slices.Sorted(slices.Values(strings.Fields(out)))
}

// fileAt is the content of file at revision rev.
func fileAt(g Gomega, checkoutDir, rev, file string) string {
	out, err := gitRun(checkoutDir, "show", rev+":"+file)
	g.Expect(err).NotTo(HaveOccurred(), "%s at %s", file, rev)
	return out
}

// headOf is the checkout's HEAD.
func headOf(g Gomega, checkoutDir string) string {
	out, err := gitRun(checkoutDir, "rev-parse", "HEAD")
	g.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// commitsSince lists the commits on HEAD after base.
func commitsSince(g Gomega, checkoutDir, base string) []string {
	out, err := gitRun(checkoutDir, "rev-list", base+"..HEAD")
	g.Expect(err).NotTo(HaveOccurred())
	return strings.Fields(out)
}

// commitsOldestFirst lists the commits on HEAD after base, in the order they landed.
func commitsOldestFirst(g Gomega, checkoutDir, base string) []string {
	out, err := gitRun(checkoutDir, "rev-list", "--reverse", base+"..HEAD")
	g.Expect(err).NotTo(HaveOccurred())
	return strings.Fields(out)
}
