// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// This spec is the end-to-end proof for GitTarget.spec.suspend and status.placement
// (docs/layout/model.md, "status.placement, and the post-scan pass"). It is here rather than
// only in the unit suite because the claim it makes is a wiring claim, and the unit tests cannot
// reach the wiring: a suspended target has to still DECLARE, still start its watches, still
// resync, and still have the resulting layout report reach the GitTarget's status through the
// watch plane — while writing nothing. Every one of those hops is real-cluster machinery.
//
// The negative half ("nothing was committed") is paired with a BARRIER, because a negative claim
// is worthless on its own: it passes just as well when the pipeline is asleep. Here the barrier is
// a co-resident ACTIVE GitTarget in the same repository, fed by the same ConfigMap events. Once
// the active target's file has landed, the pipeline has demonstrably run, so the suspended
// target's empty folder means it decided not to write rather than that nothing happened yet.
var _ = Describe("Manager GitTarget suspend", Label("manager", "suspend"), Ordered, func() {
	const (
		providerName = "gitprovider-suspend"

		suspendedTarget = "suspend-suspended-target"
		activeTarget    = "suspend-active-target"

		suspendedPath = "e2e/suspend-suspended"
		activePath    = "e2e/suspend-active"

		suspendedRule = "suspend-suspended-rule"
		activeRule    = "suspend-active-rule"

		configMapName = "suspend-probe"
	)

	var (
		testNs      string
		suspendRepo *RepoArtifacts
	)

	BeforeAll(func() {
		By("creating the suspend test namespace")
		testNs = testNamespaceFor("manager-suspend")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		By("setting up the Gitea repo and credentials")
		suspendRepo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-manager-suspend-%d", GinkgoRandomSeed()),
		)
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", suspendRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")

		createReadyGitProvider(providerName, testNs, suspendRepo.GitSecretHTTP, suspendRepo.RepoURLHTTP)

		By("creating one suspended and one active GitTarget in the same repository")
		applySuspendGitTarget(suspendedTarget, testNs, providerName, suspendedPath, true)
		applySuspendGitTarget(activeTarget, testNs, providerName, activePath, false)
		for _, name := range []string{suspendedTarget, activeTarget} {
			verifyResourceCondition("gittarget", name, testNs, "Validated", "True", "Succeeded", "")
		}

		By("both targets watch ConfigMaps in this namespace")
		applyIsolationWatchRule(suspendedRule, testNs, suspendedTarget, `"configmaps"`)
		applyIsolationWatchRule(activeRule, testNs, activeTarget, `"configmaps"`)
		for _, name := range []string{suspendedRule, activeRule} {
			verifyResourceStatus("watchrule", name, testNs, "True", "Succeeded", "")
		}

		By("waiting for both targets' ConfigMap streams to be live before any event is created")
		for _, name := range []string{suspendedTarget, activeTarget} {
			waitForStreamsRunning(name, testNs)
		}
	})

	AfterAll(func() {
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(90 * time.Second)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	// The half a unit test cannot make: a suspended target keeps its watches and keeps scanning,
	// so the layout it resolved reaches its status through the real watch plane.
	It("publishes status.placement for a target that has never written", func() {
		By("the suspended target resolves its folder and says so")
		verifyResourceCondition("gittarget", suspendedTarget, testNs,
			"LayoutResolved", "True", "None",
			"no kustomization governs this folder", "150s")

		By("the stanza carries the scan's own facts, not a placement's")
		// resolvedAtRevision is deliberately NOT asserted here. The repository's branch has no
		// commit yet at this point — nothing has written to it — so the scan honestly read the
		// folder at no revision, and reporting an empty one is the correct answer rather than a
		// missing one. It is asserted below, once the active target has produced a commit.
		Eventually(func(g Gomega) {
			placement := placementStatusOf(g, suspendedTarget, testNs)
			g.Expect(placement).NotTo(BeNil(), "status.placement must be published")
			g.Expect(placement).To(HaveKeyWithValue("resolvedAt", Not(BeEmpty())))
			g.Expect(placement).To(HaveKeyWithValue("mode", "Plain"),
				"no kustomization governs this folder, so it is written as plain files")
		}).Should(Succeed())

		By("and restates nothing the spec already carries")
		// The rule the stanza is held to: a status field earns its place only if a reader cannot
		// get it from the spec in the same GET. This asserts the removals stay removed, which
		// prose in the API doc cannot.
		Eventually(func(g Gomega) {
			placement := placementStatusOf(g, suspendedTarget, testNs)
			for _, key := range []string{"serializeNamespace", "byTypeEntries", "examples"} {
				g.Expect(placement).NotTo(HaveKey(key),
					"status.placement must not restate the spec")
			}
		}).Should(Succeed())
	})

	// Ready=True with reason Suspended. Not writing is the configured outcome, so no condition
	// goes False for it — that is what keeps the conditions that mean a broken mirror meaningful.
	It("reports a suspended target as Ready with reason Suspended", func() {
		verifyResourceCondition("gittarget", suspendedTarget, testNs,
			"Ready", "True", "Suspended", "writes nothing", "150s")
	})

	// The write gate, with its barrier.
	It("writes nothing while the active target in the same repo writes", func() {
		By("creating a ConfigMap both targets watch")
		applySuspendConfigMap(configMapName, testNs, "15m")

		By("BARRIER: the active target commits it, so the pipeline has demonstrably run")
		waitForPruneFile(suspendRepo, suspendConfigMapPath(activePath, testNs, configMapName), true)

		By("the suspended target now dates its resolution to a real revision")
		// The branch has a commit now, so the scan has a revision to name. Before the barrier it
		// did not, which is why this assertion lives here rather than with the rest of the stanza.
		// The reconcile request is what makes this prompt rather than a wait on the periodic pass,
		// and asserting through it is the point: it is how an operator re-reads a folder someone
		// else changed without waiting for the periodic cadence.
		requestReconcile(suspendedTarget, testNs)
		Eventually(func(g Gomega) {
			placement := placementStatusOf(g, suspendedTarget, testNs)
			g.Expect(placement).To(HaveKeyWithValue("resolvedAtRevision", Not(BeEmpty())),
				"the scan names the revision it read")
		}).Should(Succeed())

		By("and publishes no retention while suspended")
		// Nothing sweeps while writes are off, so nothing is measured. A published zero would
		// read as "converged" when it means "not counted", so the stanza is absent instead.
		Consistently(func(g Gomega) {
			g.Expect(statusStanzaOf(g, suspendedTarget, testNs, "retention")).To(BeNil(),
				"a suspended target measures no retention, so it reports none")
		}, "20s", "4s").Should(Succeed())

		By("and the suspended target's folder is still empty")
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, suspendRepo.CheckoutDir)
			_, statErr := os.Stat(filepath.Join(
				suspendRepo.CheckoutDir, suspendConfigMapPath(suspendedPath, testNs, configMapName)))
			g.Expect(os.IsNotExist(statErr)).To(BeTrue(),
				"a suspended target must not write, even for an event it observed")
		}, 20*time.Second, 4*time.Second).Should(Succeed())
	})

	// Clearing suspend resumes from the CURRENT cluster state rather than replaying the events
	// suppressed while suspended, which is why the file that appears is the ConfigMap's latest
	// value and not the one it had when it was created.
	It("resumes writing when suspend is cleared", func() {
		By("changing the ConfigMap while the target is still suspended")
		applySuspendConfigMap(configMapName, testNs, "30m")

		By("clearing spec.suspend")
		_, err := kubectlRunInNamespace(testNs, "patch", "gittarget", suspendedTarget,
			"--type", "merge", "-p", `{"spec":{"suspend":false}}`)
		Expect(err).NotTo(HaveOccurred(), "failed to clear spec.suspend")

		By("requesting a reconcile so the resume does not wait for the periodic pass")
		requestReconcile(suspendedTarget, testNs)

		By("the document lands, carrying the value the cluster holds NOW")
		relPath := suspendConfigMapPath(suspendedPath, testNs, configMapName)
		waitForPruneFile(suspendRepo, relPath, true)
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, suspendRepo.CheckoutDir)
			g.Expect(readRepoFile(g, filepath.Join(suspendRepo.CheckoutDir, relPath))).
				To(ContainSubstring("30m"),
					"resuming replays the current state, not the events suppressed while suspended")
		}).Should(Succeed())

		By("and Ready is no longer reported as Suspended")
		Eventually(func(g Gomega) {
			g.Expect(readyReasonOf(g, suspendedTarget, testNs)).NotTo(Equal("Suspended"))
		}).Should(Succeed())
	})
})

// applySuspendGitTarget applies a GitTarget with spec.suspend set explicitly.
func applySuspendGitTarget(name, namespace, providerName, targetPath string, suspend bool) {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: %s
  namespace: %s
spec:
  providerRef:
    name: %s
  branch: main
  path: %s
  commit:
    window: "0s"
  suspend: %t
`, name, namespace, providerName, targetPath, suspend)
	out, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(),
		"failed to apply GitTarget %q with suspend %t: %s", name, suspend, out)
}

// applySuspendConfigMap applies the ConfigMap both targets watch, with a value the spec can
// distinguish between writes.
func applySuspendConfigMap(name, namespace, timeout string) {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  timeout: %q
`, name, namespace, timeout)
	out, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply ConfigMap %s/%s: %s", namespace, name, out)
}

// requestReconcile stamps the reconcile-request annotation with a fresh value, which is what makes
// the controller re-read the folder now instead of on the periodic cadence.
func requestReconcile(name, namespace string) {
	GinkgoHelper()
	patch := fmt.Sprintf(`{"metadata":{"annotations":{"reconcile.configbutler.ai/requestedAt":%q}}}`,
		time.Now().UTC().Format(time.RFC3339Nano))
	_, err := kubectlRunInNamespace(namespace, "patch", "gittarget", name, "--type", "merge", "-p", patch)
	Expect(err).NotTo(HaveOccurred(), "failed to request a reconcile of %q", name)
}

// suspendConfigMapPath is the canonical mirror path for a ConfigMap under a GitTarget folder.
func suspendConfigMapPath(basePath, ns, name string) string {
	return path.Join(basePath, fmt.Sprintf("%s/configmaps/%s.yaml", ns, name))
}

// statusStanzaOf reads one named stanza under a GitTarget's status, nil when it has none —
// which is a meaningful answer for both callers: an unpublished stanza and a zeroed one say
// different things.
func statusStanzaOf(g Gomega, name, namespace, stanza string) map[string]interface{} {
	GinkgoHelper()
	out, err := kubectlRunInNamespace(namespace, "get", "gittarget", name, "-o", "json")
	g.Expect(err).NotTo(HaveOccurred(), "failed to read GitTarget %q", name)

	var obj unstructured.Unstructured
	g.Expect(json.Unmarshal([]byte(out), &obj.Object)).To(Succeed())
	value, found, err := unstructured.NestedMap(obj.Object, "status", stanza)
	g.Expect(err).NotTo(HaveOccurred())
	if !found {
		return nil
	}
	return value
}

// placementStatusOf reads a GitTarget's status.placement stanza, nil when it has none.
func placementStatusOf(g Gomega, name, namespace string) map[string]interface{} {
	GinkgoHelper()
	return statusStanzaOf(g, name, namespace, "placement")
}

// readyReasonOf reads the reason on a GitTarget's Ready condition.
func readyReasonOf(g Gomega, name, namespace string) string {
	GinkgoHelper()
	out, err := kubectlRunInNamespace(namespace, "get", "gittarget", name,
		"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].reason}`)
	g.Expect(err).NotTo(HaveOccurred(), "failed to read Ready reason of %q", name)
	return strings.TrimSpace(out)
}
