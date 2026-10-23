// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Restart with work made while the operator was down (E9 in docs/design/applier-echoes.md). While it
// is stopped, a mirrored ConfigMap is edited and another created through the API, and another writer
// pushes a ConfigMap the GitTarget does not watch into the folder the applier applies. When it starts
// again, the edit and the new object are mirrored before anything is applied, so nothing is reverted
// on the cluster; then the Git-side change is applied, and the new object is adopted.
//
// The initial snapshot is the startup baseline: it is decided whatever Git holds, so a Git-side
// change to a watched object made while the operator was down is the API-wins case of the
// collisions table (C2). The Git-side change here is to an object the GitTarget does not watch, which
// the snapshot leaves alone.
//
// Serial: scales the controller deployment, a singleton every concurrent spec depends on. See
// docs/spec/e2e-serial-registry.md.

// restartWithWorkMadeWhileDown runs the scenario against r.
func restartWithWorkMadeWhileDown(r *echoRun) {
	GinkgoHelper()
	r.pushFromOutside("down-edited", "e0")
	r.eventuallyEverywhere("down-edited", "e0", echoApplyBudget)
	r.eventuallyAdopted("down-edited")
	unwatched := r.gitPath + "/unwatched-" + r.testNs + ".yaml"

	By("stopping the operator")
	deployment, err := controllerDeploymentName()
	Expect(err).NotTo(HaveOccurred())
	scaleController(deployment, 0)
	DeferCleanup(func() { scaleController(deployment, 1) })

	By("editing a mirrored ConfigMap and creating another through the API, and pushing an unwatched one")
	r.editThroughAPI("down-edited", "e-down")
	r.createThroughAPI("down-created", "c-down")
	pushFileFromOutside(r.repo, unwatched, fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: down-unwatched
  namespace: %s
data:
  value: g-down
`, r.testNs), "e2e: another writer adds an object the GitTarget does not watch")

	By("starting the operator again")
	scaleController(deployment, 1)

	r.eventuallyEverywhere("down-edited", "e-down", echoApplyBudget)
	r.eventuallyEverywhere("down-created", "c-down", echoApplyBudget)
	Eventually(func(g Gomega) {
		out, getErr := kubectlRunInNamespace(r.testNs, "get", "configmap", "down-unwatched",
			"--ignore-not-found", "-o", "jsonpath={.data.value}")
		g.Expect(getErr).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal("g-down"), "the Git-side change is applied")
	}, echoApplyBudget, 2*time.Second).Should(Succeed())
	r.eventuallyAdopted("down-created")
	r.consistentlyEverywhere("down-edited", "e-down")
}

// scaleController sets the controller deployment's replicas, and waits until it has that many ready.
func scaleController(deployment string, replicas int) {
	GinkgoHelper()
	_, err := kubectlRunInNamespace(namespace, "scale", "deployment", deployment,
		fmt.Sprintf("--replicas=%d", replicas))
	Expect(err).NotTo(HaveOccurred(), "failed to scale %s to %d", deployment, replicas)
	if replicas > 0 {
		_, err = kubectlRunInNamespace(namespace, "rollout", "status", "deployment", deployment, "--timeout=180s")
		Expect(err).NotTo(HaveOccurred(), "%s did not become ready", deployment)
		return
	}
	Eventually(func(g Gomega) {
		out, getErr := kubectlRunInNamespace(namespace, "get", "pods", "-o", "jsonpath={.items[*].metadata.name}")
		g.Expect(getErr).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).NotTo(ContainSubstring(deployment), "the operator's pods are gone")
	}, 2*time.Minute, time.Second).Should(Succeed())
}

var _ = Describe("Bi Directional restart with work made while down (Flux)",
	Label("bi-directional", "flux", "applier-echoes"), Serial, Ordered, func() {
		var (
			testNs, fluxName string
			r                *echoRun
		)

		BeforeAll(func() {
			skipUnlessBiDirectionalEnabled()
			allowCrossNamespaceAppliers()
			testNs = testNamespaceFor("echoes-restart-flux")
			_, _ = kubectlRun("create", "namespace", testNs)
			fluxName = "echoes-restart-" + testNs

			repo := SetupRepo(resolveE2EContext(), testNs,
				fmt.Sprintf("e2e-echoes-restart-flux-%d", GinkgoRandomSeed()))
			pipeline := newTimingPipeline(testNs, "flux-restart", repo)
			pipeline.start()
			applyFluxApplierWithPrune(testNs, repo, fluxName, fluxName+"-git", "main", "./"+pipeline.gitPath, true)
			createFluxClaim(testNs, fluxName, fluxName)
			nameApplierClaim(testNs, pipeline.targetName, "FluxClaim", fluxName, "")
			verifyResourceCondition("gittarget", pipeline.targetName, testNs,
				"ApplierClaimReady", "True", "Succeeded", "")
			r = &echoRun{timingPipeline: pipeline, applier: fluxEchoApplier(testNs, fluxName, repo)}
		})

		AfterAll(func() {
			if !biDirectionalEnabled() || r == nil {
				return
			}
			r.cleanup()
			cleanupNamespacedResource(testNs, fluxClaimResource, fluxName)
			cleanupNamespacedResource("flux-system", "kustomization", fluxName)
			cleanupNamespacedResource("flux-system", "gitrepository", fluxName)
			cleanupNamespacedResource("flux-system", "secret", fluxName+"-git")
			cleanupNamespace(testNs)
		})

		It("mirrors the work made while down before it applies, and adopts what the API created", func() {
			restartWithWorkMadeWhileDown(r)
		})
	})

var _ = Describe("Bi Directional restart with work made while down (Argo CD)",
	Label("bi-directional", "argocd", "applier-echoes"), Serial, Ordered, func() {
		var (
			testNs string
			r      *echoRun
			run    argoBiDirectionalRun
		)

		BeforeAll(func() {
			skipUnlessBiDirectionalEnabled()
			requireArgoCDInstalled()
			allowCrossNamespaceAppliers()
			testNs = testNamespaceFor("echoes-restart-argo")
			_, _ = kubectlRun("create", "namespace", testNs)

			repo := SetupRepo(resolveE2EContext(), testNs,
				fmt.Sprintf("e2e-echoes-restart-argo-%d", GinkgoRandomSeed()))
			pipeline := newTimingPipeline(testNs, "argo-restart", repo)
			run = newArgoBiDirectionalRun(testNs, repo, "argo-restart")
			pipeline.start()
			run.applyArgoRepoSecret()
			cfg := run.baseAppConfig()
			cfg.Path = pipeline.gitPath
			run.applyArgoApp(cfg)
			createArgoCDClaim(testNs, run.appName, run.argoNs, run.appName)
			nameApplierClaim(testNs, pipeline.targetName, "ArgoCDClaim", run.appName, "")
			verifyResourceCondition("gittarget", pipeline.targetName, testNs,
				"ApplierClaimReady", "True", "Succeeded", "")
			r = &echoRun{timingPipeline: pipeline, applier: argoEchoApplier(run)}
		})

		AfterAll(func() {
			if !biDirectionalEnabled() || r == nil {
				return
			}
			cleanupNamespacedResource(run.argoNs, "application.argoproj.io", run.appName)
			cleanupNamespacedResource(run.argoNs, "secret", run.repoSecretName)
			cleanupNamespacedResource(testNs, argoCDClaimResource, run.appName)
			r.cleanup()
			cleanupNamespace(testNs)
		})

		It("mirrors the work made while down before it applies, and adopts what the API created", func() {
			restartWithWorkMadeWhileDown(r)
		})
	})
