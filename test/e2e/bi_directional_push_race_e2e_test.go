// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Two Git-side pushes in quick succession must both reach the cluster, and the later one must stay
// in Git.
//
// The race, found by the Argo CD timing spec in CI: GitOps Reverser asks the applier to apply push
// A, the applier's own write to the object comes back through the watch inside the apply window,
// and it is mirrored as a pending write ("its content is the commit's, which Git already holds",
// apply_window.go). Push B lands before that write is committed. The write is rebuilt on B and
// says A's value, so the commit it makes reverts B, and the applier is then asked to apply the
// revert.
//
// Each round pushes B the moment A is live, which is when A's echo is still pending. Several
// rounds, because the window is seconds wide and a slow round can miss it.
//
// The folder is shared, as in the timing spec: GitOps Reverser mirrors a labeled ConfigMap into
// the folder the applier applies.
//
// Pending until F1 and F2 of docs/design/applier-echoes.md are built: Argo CD fails it today, in
// round 0, and Flux passes only when its echo happens to land after the push cooldown. The change
// that builds them removes Pending.

const (
	pushRaceRounds = 3
	// Long enough for a pending write to be committed and pushed, and for an apply of the revert
	// to land: a revert shows as Git or the cluster leaving B within it.
	pushRaceHold = 20 * time.Second
)

// pushRace runs the rounds against a pipeline whose applier the receiver drives.
func pushRace(pipeline *timingPipeline) {
	GinkgoHelper()
	for i := range pushRaceRounds {
		first := fmt.Sprintf("race-a-%d", i)
		second := fmt.Sprintf("race-b-%d", i)

		By("round " + strconv.Itoa(i) + ": pushing A, and waiting until the applier made it live")
		pipeline.pushFromGitSide(first)
		Eventually(func(g Gomega) {
			g.Expect(pipeline.liveValue(g)).To(Equal(first))
		}, timingReceiverBudget, 250*time.Millisecond).Should(Succeed())

		By("pushing B at once, while A's own apply is still on its way back through the mirror")
		pipeline.pushFromGitSide(second)

		By("B reaches the cluster")
		Eventually(func(g Gomega) {
			g.Expect(pipeline.liveValue(g)).To(Equal(second))
		}, timingReceiverBudget, time.Second).Should(Succeed(),
			"the later Git-side push must be applied, not reverted by the echo of the earlier one")

		By("and nothing reverts it, in Git or in the cluster")
		Consistently(func(g Gomega) {
			g.Expect(pipeline.committedValue(g)).To(Equal(second), "Git must keep the later push")
			g.Expect(pipeline.liveValue(g)).To(Equal(second))
		}, pushRaceHold, 2*time.Second).Should(Succeed())
	}
}

var _ = Describe("Bi Directional push race (Flux)", Label("bi-directional", "flux", "push-race"), Ordered, Pending,
	func() {
		var (
			testNs, fluxName string
			pipeline         *timingPipeline
		)

		BeforeAll(func() {
			skipUnlessBiDirectionalEnabled()
			allowCrossNamespaceAppliers()
			testNs = testNamespaceFor("push-race-flux")
			_, _ = kubectlRun("create", "namespace", testNs)
			fluxName = "push-race-" + testNs

			repo := SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-push-race-flux-%d", GinkgoRandomSeed()))
			pipeline = newTimingPipeline(testNs, "flux-race", repo)
			pipeline.start()

			applyFluxApplier(testNs, repo, fluxName, fluxName+"-git", "main", "./"+pipeline.gitPath)
			createFluxTrigger(testNs, fluxName, fluxName, "")
			nameApplyTrigger(testNs, pipeline.targetName, "FluxTrigger", fluxName, "")
			verifyResourceCondition("gittarget", pipeline.targetName, testNs,
				"ApplyTriggerReady", "True", "Succeeded", "")
			verifyResourceCondition(fluxTriggerResource, fluxName, testNs, "Held", "True", "Suspended", "")
		})

		AfterAll(func() {
			if !biDirectionalEnabled() {
				return
			}
			if pipeline != nil {
				pipeline.cleanup()
			}
			cleanupNamespacedResource(testNs, fluxTriggerResource, fluxName)
			cleanupNamespacedResource("flux-system", "kustomization", fluxName)
			cleanupNamespacedResource("flux-system", "gitrepository", fluxName)
			cleanupNamespacedResource("flux-system", "secret", fluxName+"-git")
			cleanupNamespace(testNs)
		})

		It("keeps a Git-side push made while the previous one is being applied", func() {
			pushRace(pipeline)
		})
	})

var _ = Describe("Bi Directional push race (Argo CD)", Label("bi-directional", "argocd", "push-race"), Ordered,
	Pending,
	func() {
		var (
			testNs   string
			pipeline *timingPipeline
			run      argoBiDirectionalRun
		)

		BeforeAll(func() {
			skipUnlessBiDirectionalEnabled()
			requireArgoCDInstalled()
			allowCrossNamespaceAppliers()
			testNs = testNamespaceFor("push-race-argo")
			_, _ = kubectlRun("create", "namespace", testNs)

			repo := SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-push-race-argo-%d", GinkgoRandomSeed()))
			pipeline = newTimingPipeline(testNs, "argo-race", repo)
			run = newArgoBiDirectionalRun(testNs, repo, "argo-race")
			pipeline.start()

			run.applyArgoRepoSecret()
			cfg := run.baseAppConfig()
			cfg.Path = pipeline.gitPath
			run.applyArgoApp(cfg)
			createArgoCDTrigger(testNs, run.appName, run.argoNs, run.appName)
			nameApplyTrigger(testNs, pipeline.targetName, "ArgoCDTrigger", run.appName, "")
			verifyResourceCondition("gittarget", pipeline.targetName, testNs,
				"ApplyTriggerReady", "True", "Succeeded", "")
		})

		AfterAll(func() {
			if !biDirectionalEnabled() {
				return
			}
			cleanupNamespacedResource(run.argoNs, "application.argoproj.io", run.appName)
			cleanupNamespacedResource(run.argoNs, "secret", run.repoSecretName)
			cleanupNamespacedResource(testNs, argoCDTriggerResource, run.appName)
			if pipeline != nil {
				pipeline.cleanup()
			}
			cleanupNamespace(testNs)
		})

		It("keeps a Git-side push made while the previous one is being applied", func() {
			pushRace(pipeline)
		})
	})
