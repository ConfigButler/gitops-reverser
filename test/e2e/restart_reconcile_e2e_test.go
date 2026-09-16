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

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

// This suite is a regression guard for the "startup reconcile wipes the tracked
// git tree" data-loss bug: on a plain controller restart the startup reconcile
// could observe an empty cluster and the FolderReconciler would faithfully
// delete every previously committed file.
//
// The trigger is a ClusterWatchRule that uses the documented wildcard
// `apiVersions: ["*"]`. The live audit path honours that wildcard, so the
// mirror builds up normally; the startup reconcile path did not, so it resolved
// zero GVRs, listed zero resources, and the reconciler diffed "cluster has 0"
// against "git has N" -> N deletions.
//
// IMPORTANT: the wildcard rule is what makes this test meaningful. A test that
// watches with a concrete `apiVersions: ["v1"]` rule passes even with the bug
// present — which is exactly why the existing e2e suite never caught this.
// Serial: rolls the controller deployment, which disrupts any spec running
// concurrently on another process. See docs/spec/e2e-serial-registry.md.
var _ = Describe("Restart Reconcile Safety", Label("restart-reconcile"), Serial, Ordered, func() {
	var (
		testNs        string
		restartRepo   *RepoArtifacts
		gitTargetPath = "e2e/restart-reconcile"
	)

	const (
		providerName  = "restart-reconcile-provider"
		gitTargetName = "restart-reconcile-target"
		watchRuleName = "restart-reconcile-wildcard"
	)

	// orderNames are "quiet" resources: created once and never touched again.
	// A wiped quiet resource never re-emits an audit event, so it stays
	// permanently deleted from git — the exact, easy-to-miss failure mode.
	orderNames := []string{"restart-order-alpha", "restart-order-bravo", "restart-order-charlie"}

	// terminatingOrder is deleted before the restart but held Terminating by a finalizer, so the
	// restart's replay LISTs an object that still exists in the API and is already absent from
	// Git. A snapshot that folds it into the desired set resurrects the file the live
	// deletion-as-intent rule removed — a deleted resource silently reappearing in the mirror.
	// The wipe case above and this one are the two directions the same replay can get wrong.
	const terminatingOrder = "restart-order-terminating"

	BeforeAll(func() {
		By("setting up the Prometheus client for drain-signal metrics")
		setupPrometheusClient()
		verifyPrometheusAvailable()

		By("creating the restart-reconcile test namespace")
		testNs = testNamespaceFor("restart-reconcile")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		By("setting up a dedicated Gitea repo and credentials")
		restartRepo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-restart-reconcile-%d", GinkgoRandomSeed()),
		)

		By("applying git secrets to the test namespace")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", restartRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)
	})

	AfterAll(func() {
		dumpFailureDiagnostics()
		// Release the hold unconditionally. If the spec failed before clearing it, the object
		// stays Terminating forever and cleanupNamespace below never completes.
		removeIceCreamOrderFinalizers(testNs, terminatingOrder)
		cleanupWatchRule(watchRuleName, testNs)
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(60 * time.Second)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("keeps the git mirror intact when the controller restarts", func() {
		By("installing the IceCreamOrder CRD")
		err := applyIceCreamCRD(crdGroupRestartReconcile)
		Expect(err).NotTo(HaveOccurred(), "failed to install IceCreamOrder CRD")
		Eventually(func(g Gomega) {
			output, getErr := kubectlRun(
				"get", "crd", iceCreamCRDName(crdGroupRestartReconcile),
				"-o", "jsonpath={.status.conditions[?(@.type=='Established')].status}",
			)
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(Equal("True"))
		}).Should(Succeed())

		By("creating the GitProvider, GitTarget and wildcard WatchRule")
		createGitProviderWithURLInNamespace(providerName, testNs, restartRepo.GitSecretHTTP, restartRepo.RepoURLHTTP)
		createGitTarget(gitTargetName, testNs, providerName, gitTargetPath, "main")

		wrData := struct {
			Name            string
			DestinationName string
			Namespace       string
			Group           string
		}{
			Name:            watchRuleName,
			DestinationName: gitTargetName,
			Namespace:       testNs,
			Group:           crdGroupRestartReconcile,
		}
		Expect(applyFromTemplate(
			"test/e2e/templates/restart/watchrule-wildcard.tmpl", wrData, testNs,
		)).To(Succeed(), "failed to apply wildcard WatchRule")

		verifyResourceCondition("gittarget", gitTargetName, testNs, "Validated", "True", "Succeeded", "")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Succeeded", "")

		By("creating quiet IceCreamOrder resources to build up the git mirror")
		for _, name := range orderNames {
			createIceCreamOrder(crdGroupRestartReconcile, testNs, name)
		}

		expectedFiles := make([]string, 0, len(orderNames))
		for _, name := range orderNames {
			expectedFiles = append(expectedFiles, filepath.Join(
				gitTargetPath, iceCreamInstancePath(crdGroupRestartReconcile, testNs, name),
			))
		}

		By("waiting until every order has been committed to git")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, restartRepo.CheckoutDir)
			for _, relPath := range expectedFiles {
				_, statErr := os.Stat(filepath.Join(restartRepo.CheckoutDir, relPath))
				g.Expect(statErr).NotTo(HaveOccurred(), "expected committed file %q", relPath)
			}
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("creating a finalizer-held order, then deleting it so it is Terminating across the restart")
		createIceCreamOrder(crdGroupRestartReconcile, testNs, terminatingOrder)
		addIceCreamOrderFinalizer(testNs, terminatingOrder)
		terminatingPath := filepath.Join(
			gitTargetPath, iceCreamInstancePath(crdGroupRestartReconcile, testNs, terminatingOrder),
		)
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, restartRepo.CheckoutDir)
			_, statErr := os.Stat(filepath.Join(restartRepo.CheckoutDir, terminatingPath))
			g.Expect(statErr).NotTo(HaveOccurred(), "expected committed file %q", terminatingPath)
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		_, deleteErr := kubectlRunInNamespace(testNs, "delete", "icecreamorder", terminatingOrder, "--wait=false")
		Expect(deleteErr).NotTo(HaveOccurred(), "failed to request deletion of the finalizer-held order")

		By("confirming the live path removed it from Git at intent time while it is still Terminating")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, restartRepo.CheckoutDir)
			_, statErr := os.Stat(filepath.Join(restartRepo.CheckoutDir, terminatingPath))
			g.Expect(statErr).To(HaveOccurred(), "deletion-as-intent should have removed %q", terminatingPath)
		}, 2*time.Minute, 3*time.Second).Should(Succeed())
		ts, tsErr := kubectlRunInNamespace(testNs, "get", "icecreamorder", terminatingOrder,
			"-o", "jsonpath={.metadata.deletionTimestamp}")
		Expect(tsErr).NotTo(HaveOccurred(), "the finalizer must still hold the object in the API")
		Expect(strings.TrimSpace(ts)).NotTo(BeEmpty(), "the object must be Terminating when the restart replays")

		headBeforeRestart := revParseHead(restartRepo.CheckoutDir)
		By(fmt.Sprintf("git mirror complete at %s — restarting the controller", headBeforeRestart))

		By("restarting the controller deployment (plain rollout restart)")
		restartControllerDeployment()

		// Anchor the reconcile gate to the NEW pod. restartControllerDeployment
		// blocks until the rollout completes, so exactly one non-terminating
		// controller pod remains — the freshly started one. controllerPodNames
		// already filters out pods with a deletionTimestamp.
		var newControllerPod string
		Eventually(func(g Gomega) {
			pods, podErr := controllerPodNames()
			g.Expect(podErr).NotTo(HaveOccurred())
			g.Expect(pods).To(HaveLen(1), "expected exactly one non-terminating controller pod after rollout")
			newControllerPod = pods[0]
		}, 30*time.Second, 2*time.Second).Should(Succeed())
		By(fmt.Sprintf("new controller pod after restart: %s", newControllerPod))

		// Phase 3 drain signals replace a 75 s blind wait. The reconcile counter
		// is scoped to the new pod via its `pod` target label: a counter resets to
		// 0 on a fresh pod, so `{pod="<new>"} > 0` proves the new pod completed its
		// own post-restart reconcile.
		//
		// mode="type_reconcile" is load-bearing. watch_recovery_total counts every recovery path,
		// and `replay` increments at initial-events-end — BEFORE the replay's resync has been
		// applied. An unscoped `> 0` would therefore pass as soon as the new pod finished reading,
		// not when it finished writing, which is the moment this spec exists to wait for. Only
		// type_reconcile is recorded after the resync applies on the branch worker. A sum() over a pre-restart baseline
		// cannot prove this — once Prometheus marks the old pod's series stale, the
		// new pod's first increment can bring the cross-pod sum back to the old
		// total rather than above it, so `> baseline` could never pass.
		// git_queue_depth returning to 0 then confirms that submission
		// has been committed and pushed — the exact moment any destructive commit
		// would have landed — instead of guessing with a fixed sleep.
		By("waiting for the new pod to complete its post-restart reconcile")
		waitForMetricWithTimeout(
			fmt.Sprintf(
				`sum(gitopsreverser_watch_recovery_total`+
					`{gittarget_namespace=%q,gittarget_name=%q,pod=%q,mode="type_reconcile"}) `+
					`or vector(0)`,
				testNs, gitTargetName, newControllerPod,
			),
			func(v float64) bool { return v > 0 },
			"GitTarget reconcile completed on the new controller pod",
			90*time.Second,
		)

		By("waiting for the branch worker queue to drain")
		waitForMetricWithTimeout(
			fmt.Sprintf(
				`sum(gitopsreverser_git_queue_depth`+
					`{provider_namespace=%q,provider_name=%q,branch="main",pod=%q}) or vector(0)`,
				testNs, providerName, newControllerPod,
			),
			func(v float64) bool { return v == 0 },
			"branch worker queue drained after restart",
			90*time.Second,
		)

		// A short stability window guards against a delayed second reconcile and
		// against any Prometheus cross-restart sample staleness in the gates
		// above: a quiet order, once wiped, never comes back, so a mirror that
		// stays intact for this window after the drain will stay intact.
		By("verifying the git mirror is NOT wiped by the restart, and the Terminating order stays deleted")
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, restartRepo.CheckoutDir)
			for _, relPath := range expectedFiles {
				_, statErr := os.Stat(filepath.Join(restartRepo.CheckoutDir, relPath))
				g.Expect(statErr).NotTo(HaveOccurred(),
					"file %q disappeared after the controller restart — startup reconcile wiped the mirror", relPath)
			}
			// The replay LISTed this object (it is still in the API, held by its finalizer), and
			// the reconcile gate above proves that replay's resync APPLIED — so this assertion
			// cannot pass vacuously. A snapshot that treats a Terminating object as desired
			// resurrects the file here.
			_, statErr := os.Stat(filepath.Join(restartRepo.CheckoutDir, terminatingPath))
			g.Expect(statErr).To(HaveOccurred(),
				"file %q came back after the restart — the replay snapshot treated a Terminating "+
					"object as desired state", terminatingPath)
		}, 15*time.Second, 5*time.Second).Should(Succeed())

		By("clearing the finalizer and confirming the terminal DELETED still produces no resurrection")
		removeIceCreamOrderFinalizers(testNs, terminatingOrder)
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, restartRepo.CheckoutDir)
			_, statErr := os.Stat(filepath.Join(restartRepo.CheckoutDir, terminatingPath))
			g.Expect(statErr).To(HaveOccurred(), "file %q came back once finalizers cleared", terminatingPath)
		}, 10*time.Second, 5*time.Second).Should(Succeed())

		By("confirming no commit since the restart deleted tracked files")
		deletions, logErr := gitRun(
			restartRepo.CheckoutDir,
			"log", "--diff-filter=D", "--name-only", "--pretty=format:commit %h %s",
			headBeforeRestart+"..HEAD", "--", gitTargetPath,
		)
		Expect(logErr).NotTo(HaveOccurred(), "failed to inspect git history after restart")
		Expect(strings.TrimSpace(deletions)).To(BeEmpty(),
			"a commit after the restart deleted tracked files:\n%s", deletions)
	})
})

// createIceCreamOrder applies a minimal IceCreamOrder custom resource for the
// given CRD group.
func createIceCreamOrder(group, ns, name string) {
	By(fmt.Sprintf("creating IceCreamOrder '%s/%s'", ns, name))
	manifest := fmt.Sprintf(`apiVersion: %s/v1
kind: IceCreamOrder
metadata:
  name: %s
  namespace: %s
spec:
  customerName: %s
  container: Cup
  scoops:
    - flavor: Vanilla
      quantity: 1
`, group, name, ns, name)
	_, err := kubectlRunWithStdin(ns, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply IceCreamOrder %s/%s", ns, name)
}

// addIceCreamOrderFinalizer holds an order in Terminating once it is deleted, so a replay LISTs an
// object that exists in the API and is already absent from Git.
func addIceCreamOrderFinalizer(ns, name string) {
	_, err := kubectlRunInNamespace(ns, "patch", "icecreamorder", name, "--type", "merge",
		"-p", `{"metadata":{"finalizers":["e2e.configbutler.ai/hold"]}}`)
	Expect(err).NotTo(HaveOccurred(), "failed to add finalizer to IceCreamOrder %s/%s", ns, name)
}

// removeIceCreamOrderFinalizers releases the hold so the object can actually leave the API — and
// so the namespace can be torn down, which a lingering finalizer would block.
func removeIceCreamOrderFinalizers(ns, name string) {
	_, err := kubectlRunInNamespace(ns, "patch", "icecreamorder", name, "--type", "merge",
		"-p", `{"metadata":{"finalizers":null}}`)
	if err != nil && !strings.Contains(err.Error(), "NotFound") {
		Expect(err).NotTo(HaveOccurred(), "failed to clear finalizer on IceCreamOrder %s/%s", ns, name)
	}
}

// revParseHead returns the current HEAD commit of the local checkout.
func revParseHead(checkoutDir string) string {
	output, err := gitRun(checkoutDir, "rev-parse", "HEAD")
	Expect(err).NotTo(HaveOccurred(), "failed to resolve git HEAD: %s", output)
	return strings.TrimSpace(output)
}

// controllerDeploymentName returns the name of the single controller Deployment
// in the controller namespace.
func controllerDeploymentName() (string, error) {
	output, err := kubectlRunInNamespace(
		namespace,
		"get", "deployments",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}",
	)
	if err != nil {
		return "", fmt.Errorf("get deployments in namespace %s: %w", namespace, err)
	}
	deployments := utils.GetNonEmptyLines(output)
	if len(deployments) != 1 {
		return "", fmt.Errorf("expected exactly 1 Deployment in namespace %s, found %d", namespace, len(deployments))
	}
	return deployments[0], nil
}

// restartControllerDeployment performs a `kubectl rollout restart` on the
// controller Deployment and blocks until the rollout has fully completed.
func restartControllerDeployment() {
	deploymentName, err := controllerDeploymentName()
	Expect(err).NotTo(HaveOccurred(), "failed to resolve controller deployment")

	_, err = kubectlRunInNamespace(namespace, "rollout", "restart", "deployment", deploymentName)
	Expect(err).NotTo(HaveOccurred(), "failed to issue rollout restart for %s", deploymentName)

	_, err = kubectlRunInNamespace(
		namespace, "rollout", "status", "deployment", deploymentName, "--timeout=180s",
	)
	Expect(err).NotTo(HaveOccurred(), "controller deployment %s did not become ready after restart", deploymentName)
}
