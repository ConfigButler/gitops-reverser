// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Restart recovery for a FluxTrigger (docs/design/suspended-appliers-plan.md, step 5). The
// operation an apply runs as is recorded on the trigger before the Kustomization is resumed, so a
// manager that restarts while the apply runs follows that same operation to its end, suspends the
// Kustomization again, and asks for nothing new until it has.
//
// The apply is made slow on purpose: the Kustomization waits for its Deployment to be ready, and
// the Deployment's pod is pinned to a node label no node has, so the reconcile that read the
// resume's token keeps running across the restart. The spec then labels a node, the health check
// passes, and that reconcile ends.
//
// Serial: rolls the controller deployment and labels a node, both shared by every concurrent spec.
// See docs/spec/e2e-serial-registry.md.
var _ = Describe("Flux Restart Recovery", Label("flux-restart-recovery"), Serial, Ordered, func() {
	const (
		providerName = "flux-recovery-provider"
		destName     = "flux-recovery-dest"
		ruleName     = "flux-recovery-rule"
		gitPath      = "apps/recovery"
		folder       = "flux/recovery"
	)
	var (
		testNs, appliedNs, fluxName, nodeLabel, node string
		repo                                         *RepoArtifacts
	)

	BeforeAll(func() {
		testNs = testNamespaceFor("flux-restart-recovery")
		appliedNs = testNs + "-applied"
		fluxName = "flux-recovery-" + testNs
		nodeLabel = "e2e-recovery-" + testNs
		for _, ns := range []string{testNs, appliedNs} {
			_, _ = kubectlRun("create", "namespace", ns)
		}
		out, err := kubectlRun("get", "nodes", "-o", "jsonpath={.items[0].metadata.name}")
		Expect(err).NotTo(HaveOccurred())
		node = strings.TrimSpace(out)

		By("setting up the Gitea repo, its credentials and the GitProvider")
		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-flux-recovery-%d", GinkgoRandomSeed()))
		_, err = kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)
		createGitProviderWithURLInNamespace(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Succeeded", "")
		configureRepoOriginWithCredentials(repo, testNs)

		By("seeding the folder Flux applies with a ConfigMap, and holding its Kustomization")
		pushFileFromOutside(repo, folder+"/configmap.yaml", recoveryConfigMap(appliedNs), "e2e: seed the folder")
		applyFluxApplier(testNs, repo, fluxName, fluxName+"-git", "main", "./"+folder)
		_, err = kubectlRunInNamespace("flux-system", "patch", "kustomization", fluxName, "--type=merge", "-p",
			`{"spec":{"wait":true,"timeout":"5m"}}`)
		Expect(err).NotTo(HaveOccurred(), "the Kustomization waits for what it applies to be ready")
		createGitTarget(destName, testNs, providerName, gitPath, "main")
		createFluxTrigger(testNs, fluxName, fluxName, "")
		nameApplyTrigger(testNs, destName, "FluxTrigger", fluxName, "")
		verifyResourceCondition("gittarget", destName, testNs, "ApplyTriggerReady", "True", "Succeeded", "")
		Expect(applyFromTemplate("test/e2e/templates/manager/watchrule-resources.tmpl", struct {
			Name, Namespace, DestinationName, Resources string
		}{Name: ruleName, Namespace: testNs, DestinationName: destName, Resources: `"configmaps"`},
			testNs)).To(Succeed(), "failed to apply the WatchRule")
	})

	AfterAll(func() {
		_, _ = kubectlRun("label", "node", node, nodeLabel+"-")
		cleanupWatchRule(ruleName, testNs)
		cleanupGitTarget(destName, testNs)
		cleanupNamespacedResource(testNs, fluxTriggerResource, fluxName)
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
		cleanupNamespacedResource("flux-system", "kustomization", fluxName)
		cleanupNamespacedResource("flux-system", "gitrepository", fluxName)
		cleanupNamespacedResource("flux-system", "secret", fluxName+"-git")
		cleanupNamespace(appliedNs)
		cleanupNamespace(testNs)
	})

	It("follows the operation an apply runs as across a restart, and suspends the Kustomization after it", func() {
		By("pushing a Deployment that cannot be ready yet, then an API edit that finds the push")
		pushFileFromOutside(repo, folder+"/deployment.yaml", recoveryDeployment(appliedNs, nodeLabel),
			"e2e: another writer adds a Deployment pinned to a node label nobody has yet")
		applyConfigMap(testNs, "recovery-edit")

		By("the ForeignCommit's apply starts, and waits for the Deployment")
		var before triggerOperation
		Eventually(func(g Gomega) {
			before = readTriggerOperation(g, testNs, fluxTriggerResource, fluxName)
			g.Expect(before.Reason).To(Equal("ForeignCommit"))
			g.Expect(before.Phase).To(BeElementOf("Requested", "Running"))
			g.Expect(kustomizationSuspended(g, fluxName)).To(BeFalse(), "resumed for the apply")
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		By("restarting the manager while that apply runs")
		restartControllerDeployment()
		Expect(kustomizationSuspended(Default, fluxName)).To(BeFalse(),
			"the apply is still running: no process suspended the Kustomization")

		By("letting the Deployment become ready, which ends the reconcile that read the resume's token")
		_, err := kubectlRun("label", "node", node, nodeLabel+"=true", "--overwrite")
		Expect(err).NotTo(HaveOccurred())

		By("the restarted manager completes that same operation, and the Kustomization is suspended again")
		Eventually(func(g Gomega) {
			after := readTriggerOperation(g, testNs, fluxTriggerResource, fluxName)
			g.Expect(after.Token).To(Equal(before.Token), "the operation recorded before the restart, not a new one")
			g.Expect(after.Phase).To(Equal("Succeeded"))
			g.Expect(kustomizationSuspended(g, fluxName)).To(BeTrue())
		}, 4*time.Minute, 2*time.Second).Should(Succeed())
	})
})

// recoveryConfigMap is the ConfigMap the restart-recovery Kustomization applies from the start.
func recoveryConfigMap(namespace string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: flux-recovery-seed
  namespace: %s
data:
  color: blue
`, namespace)
}

// recoveryDeployment is a Deployment whose pod schedules only on a node carrying nodeLabel, so it is
// not ready until the spec labels one.
func recoveryDeployment(namespace, nodeLabel string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: flux-recovery-wait
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels: {app: flux-recovery-wait}
  template:
    metadata:
      labels: {app: flux-recovery-wait}
    spec:
      nodeSelector:
        %s: "true"
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.10
`, namespace, nodeLabel)
}
