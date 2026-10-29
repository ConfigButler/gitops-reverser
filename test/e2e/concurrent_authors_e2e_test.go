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

				labelled, showErr := gitRun(p.repo.CheckoutDir, "show", aliceSHA+":"+file(aliceAPI))
				g.Expect(showErr).NotTo(HaveOccurred())
				g.Expect(labelled).To(ContainSubstring("tier: frontend"),
					"alice's update after bob's writes is in her commit")
			}, time.Minute, 2*time.Second).Should(Succeed())
		})
	})

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

	changed, err := gitRun(checkoutDir, "show", "--name-only", "--format=", sha)
	g.Expect(err).NotTo(HaveOccurred())
	paths := strings.Fields(changed)
	slices.Sort(paths)
	want := slices.Sorted(slices.Values(files))
	g.Expect(paths).To(Equal(want), "commit %s changes only %s's files", sha[:8], a.user)
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
