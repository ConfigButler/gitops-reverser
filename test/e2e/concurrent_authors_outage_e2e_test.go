// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"path"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Concurrent Authors through an outage is the flagship of docs/design/concurrent-author-windows.md
// with Git refusing every push while two people work: alice and bob each work and save with their
// windows open side by side, and the pushes their saves make fail. The branch worker keeps both saves'
// writes, committed locally, and its retry publishes them once Git takes pushes again, with no further
// edit: each save still comes out as one commit of its own, its author, its message and its files.
//
// The outage is the GitProvider's credential going bad: every push and fetch is refused, as when the
// Git host is down, while the Gitea every other spec uses stays up. Putting it back ends the outage.
//
// Not Serial: the spec owns its namespace, its Gitea repository and the credential it breaks.
var _ = Describe("Concurrent Authors through an outage", Label("concurrent-authors-outage", "commit-request",
	"audit-consumer"), Ordered, func() {
	const (
		basePath     = "e2e/concurrent-authors-outage"
		commitWindow = "120s"
	)
	var p windowedDeploymentPipeline

	BeforeAll(func() {
		p = setupWindowedDeploymentPipeline("authors-outage", basePath, commitWindow)
	})

	AfterAll(func() {
		cleanupPipeline(p.ns, p.gitProvider, p.gitTarget, p.watchRule)
		cleanupNamespace(p.ns)
	})

	It("publishes both saves, one commit each, once Git takes pushes again", func() {
		seed := GinkgoRandomSeed()
		alice := author{user: "alice", message: "feat: add the storefront"}
		bob := author{user: "bob", message: "feat: add the order queue"}
		aliceAPI := fmt.Sprintf("storefront-api-%d", seed)
		aliceWeb := fmt.Sprintf("storefront-web-%d", seed)
		bobQueue := fmt.Sprintf("order-queue-%d", seed)
		file := func(name string) string {
			return path.Join(basePath, p.ns, "apps/deployments", name+".yaml")
		}

		By("seeding the branch, so the folder's bootstrap files are committed")
		seedName := fmt.Sprintf("seed-%d", seed)
		_, err := kubectlRunWithStdin(p.ns,
			commitRequestManifest(p.ns, "save-seed", p.gitTarget, "chore: seed", "8s")+"---\n"+
				deploymentManifest(p.ns, seedName), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		var seedSHA string
		Eventually(func(g Gomega) {
			seedSHA = expectCommitRequestCommitted(g, p.ns, "save-seed",
				recentCommitDiagnostics(p.repo.CheckoutDir, basePath))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("Git starts refusing every push and fetch")
		_, err = kubectlRunInNamespace(p.ns, "patch", "secret", p.repo.GitSecretHTTP, "--type=merge",
			"-p", `{"stringData":{"password":"refused-during-the-outage"}}`)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = kubectlRunInNamespace(p.ns, "apply", "-f", p.repo.SecretsYAML)
		})

		By("alice and bob each work, interleaved, and save")
		alice.create(p.ns, aliceAPI)
		bob.create(p.ns, bobQueue)
		alice.create(p.ns, aliceWeb)
		alice.save(p.ns, "save-storefront", p.gitTarget)
		bob.save(p.ns, "save-order-queue", p.gitTarget)

		By("nothing reaches Git while it refuses pushes, and neither save gives up")
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, p.repo.CheckoutDir)
			g.Expect(headOf(g, p.repo.CheckoutDir)).To(Equal(seedSHA), "a push landed during the outage\n%s",
				recentCommitDiagnostics(p.repo.CheckoutDir, basePath))
			for _, save := range []string{"save-storefront", "save-order-queue"} {
				outcome := readCommitRequestOutcome(g, p.ns, save)
				g.Expect(outcome.isTerminal()).To(BeFalse(), "%s ended during the outage: %s", save, outcome)
			}
		}, 30*time.Second, 3*time.Second).Should(Succeed())

		By("Git takes pushes again; the retry publishes both saves without another edit")
		_, err = kubectlRunInNamespace(p.ns, "apply", "-f", p.repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred())
		var aliceSHA, bobSHA string
		Eventually(func(g Gomega) {
			diagnostics := recentCommitDiagnostics(p.repo.CheckoutDir, basePath)
			aliceSHA = expectCommitRequestCommitted(g, p.ns, "save-storefront", diagnostics)
			bobSHA = expectCommitRequestCommitted(g, p.ns, "save-order-queue", diagnostics)
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		By("each save is one commit: its author, its message, its files and nothing else")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, p.repo.CheckoutDir)
			g.Expect(commitsSince(g, p.repo.CheckoutDir, seedSHA)).To(ConsistOf(aliceSHA, bobSHA),
				"exactly the two saves were committed after the seed\n%s",
				recentCommitDiagnostics(p.repo.CheckoutDir, basePath))
			alice.expectCommit(g, p.repo.CheckoutDir, aliceSHA, file(aliceAPI), file(aliceWeb))
			bob.expectCommit(g, p.repo.CheckoutDir, bobSHA, file(bobQueue))
		}, time.Minute, 2*time.Second).Should(Succeed())
	})
})
