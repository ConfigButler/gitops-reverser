// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A save in progress when the controller stops: its CommitRequest is attached to an open commit
// window, and the window is still collecting. A clean shutdown closes the window under the request's
// message, pushes it, and writes the request's outcome before the process exits, so the save ends as
// one commit with the request's message and the request reports it.
//
// The pipeline is the Save Request suite's: a GitTarget with a long spec.commit.window on a Gitea
// repo of its own, so main does not exist until the window is closed, and the one commit is this
// spec's. The request's own timers are minutes long, so only the shutdown can close the window.
//
// The outcome is read while the controller is scaled to zero: the stopping process wrote it, and no
// new process could have. A rollout restart would not show that, and with the e2e manifest's default
// surge the new pod is Ready before the old one stops, so two operators would write the branch at once.
//
// Serial: scales the controller deployment, a singleton every concurrent spec depends on. See
// docs/spec/e2e-serial-registry.md.
var _ = Describe("Save Request across a controller restart", Label("commit-request-restart"), Serial, Ordered,
	func() {
		var (
			testNs        string
			repo          *RepoArtifacts
			gitProvName   string
			gitTargetName string
			watchRuleName string
		)

		const (
			basePath     = "e2e/commit-request-restart"
			commitWindow = "300s"
			// requestTimers are the request's attachTimeout and maxDuration, the schema's maximum: the
			// window it attaches to is still collecting when the controller stops.
			requestTimers = "5m"
		)

		BeforeAll(func() {
			p := setupWindowedDeploymentPipeline("commit-request-restart", basePath, commitWindow)
			testNs, repo, gitProvName, gitTargetName, watchRuleName = p.ns, p.repo, p.gitProvider, p.gitTarget,
				p.watchRule
		})

		AfterAll(func() {
			cleanupPipeline(testNs, gitProvName, gitTargetName, watchRuleName)
			cleanupNamespace(testNs)
		})

		It("closes the save's window, pushes it and writes its outcome before the controller exits", func() {
			seed := GinkgoRandomSeed()
			deployName := fmt.Sprintf("commit-request-restart-deploy-%d", seed)
			requestName := fmt.Sprintf("commit-request-restart-save-%d", seed)
			message := fmt.Sprintf("save: settled across a restart, e2e seed %d", seed)

			By("opening a commit window with an edit, and attaching a CommitRequest to it")
			applyScaleTestDeployment(testNs, deployName, 0)
			_, err := kubectlRunWithStdin(testNs,
				commitRequestManifest(testNs, requestName, gitTargetName, message, requestTimers), "apply", "-f", "-")
			Expect(err).NotTo(HaveOccurred(), "failed to apply CommitRequest %s", requestName)
			Eventually(func(g Gomega) {
				reason := commitRequestField(g, testNs, requestName,
					`{.status.conditions[?(@.type=="Reconciling")].reason}`)
				g.Expect(reason).To(Equal("CollectingWindow"), "the request is attached to the open window\n%s",
					readCommitRequestOutcome(g, testNs, requestName))
			}, 2*time.Minute, time.Second).Should(Succeed())
			Expect(remoteBranchHead(Default, repo.CheckoutDir)).To(BeEmpty(),
				"the window is still collecting: nothing is on main")

			By("stopping the controller while the window collects")
			deployment, err := controllerDeploymentName()
			Expect(err).NotTo(HaveOccurred())
			scaleController(deployment, 0)
			DeferCleanup(func() { scaleController(deployment, 1) })

			By("reading the outcome the stopping controller wrote")
			var reportedSHA string
			Eventually(func(g Gomega) {
				reportedSHA = expectCommitRequestCommitted(g, testNs, requestName,
					recentCommitDiagnostics(repo.CheckoutDir, basePath))
			}, 30*time.Second, time.Second).Should(Succeed(),
				"no controller runs now, so only the one that stopped can have written this")

			By("starting the controller again")
			scaleController(deployment, 1)

			By("verifying the save is one commit with the request's message, and the request still says so")
			Eventually(func(g Gomega) {
				pullLatestRepoState(g, repo.CheckoutDir)
				g.Expect(mustCommitCount(repo.CheckoutDir)).To(Equal(1),
					"the save is one commit\n%s", recentCommitDiagnostics(repo.CheckoutDir, basePath))
				subject, logErr := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%B")
				g.Expect(logErr).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(subject)).To(Equal(message))
				headSHA, shaErr := gitRun(repo.CheckoutDir, "rev-parse", "HEAD")
				g.Expect(shaErr).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(headSHA)).To(Equal(reportedSHA))
				expected := filepath.Join(repo.CheckoutDir, basePath,
					fmt.Sprintf("%s/apps/deployments/%s.yaml", testNs, deployName))
				_, statErr := os.Stat(expected)
				g.Expect(statErr).NotTo(HaveOccurred(), "the edit is in the save's commit: %s", expected)
			}, 2*time.Minute, 3*time.Second).Should(Succeed())
			Consistently(func(g Gomega) {
				g.Expect(expectCommitRequestCommitted(g, testNs, requestName, "")).To(Equal(reportedSHA))
				pullLatestRepoState(g, repo.CheckoutDir)
				g.Expect(mustCommitCount(repo.CheckoutDir)).To(Equal(1),
					"the new controller makes no second commit for the save\n%s",
					recentCommitDiagnostics(repo.CheckoutDir, basePath))
			}, 15*time.Second, 3*time.Second).Should(Succeed())

			By("cleaning up the Deployment and CommitRequest")
			_, _ = kubectlRunInNamespace(testNs, "delete", "deployment", deployName, "--ignore-not-found=true")
			_, _ = kubectlRunInNamespace(testNs, "delete", "commitrequest", requestName, "--ignore-not-found=true")
		})
	})
