// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"path"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// This spec is the e2e regression for GitTarget isolation on rule changes (see
// docs/spec/gittarget-isolation-on-rule-change.md). The original symptom was a
// parallel-run flake: one GitTarget's ConfigMap event commit ("[CREATE] ...")
// was replaced by a reconcile commit ("reconciled N <type>") because an
// unrelated spec changed a *different* target's rules at the same time, dragging
// every target into rule-change reconcile mode.
//
// It is deliberately NOT Serial — the whole point is that isolation must hold
// under parallel execution. It runs as part of the single e2e suite, the same
// suite where the flake originally appeared.
var _ = Describe("Manager GitTarget Isolation", Label("manager"), Ordered, func() {
	const (
		providerName = "gitprovider-iso"
		targetA      = "iso-target-a"
		targetB      = "iso-target-b"
		ruleA        = "iso-rule-a"
		ruleB        = "iso-rule-b"
		pathA        = "e2e/iso-a"
		pathB        = "e2e/iso-b"
	)

	var (
		testNs  string
		isoRepo *RepoArtifacts
	)

	BeforeAll(func() {
		By("creating GitTarget isolation test namespace")
		testNs = testNamespaceFor("manager-isolation")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		By("setting up Gitea repo and credentials for isolation tests")
		isoRepo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-manager-isolation-%d", GinkgoRandomSeed()),
		)

		By("applying git secrets to test namespace")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", isoRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		By("creating shared GitProvider for isolation specs")
		createGitProviderWithURLInNamespace(
			providerName,
			testNs,
			isoRepo.GitSecretHTTP,
			isoRepo.RepoURLHTTP,
		)
		verifyResourceStatus(
			"gitprovider", providerName, testNs,
			"True", "Ready", "Repository connectivity validated",
		)

		By("creating two independent GitTargets writing to separate paths in the same repo")
		createGitTarget(targetA, testNs, providerName, pathA, "main")
		createGitTarget(targetB, testNs, providerName, pathB, "main")
		verifyResourceCondition("gittarget", targetA, testNs, "Validated", "True", "Succeeded", "")
		verifyResourceCondition("gittarget", targetB, testNs, "Validated", "True", "Succeeded", "")

		By("target A and target B both watch ConfigMaps (baseline steady state)")
		applyIsolationWatchRule(ruleA, testNs, targetA, `"configmaps"`)
		applyIsolationWatchRule(ruleB, testNs, targetB, `"configmaps"`)
		verifyResourceStatus("watchrule", ruleA, testNs, "True", "Succeeded", "")
		verifyResourceStatus("watchrule", ruleB, testNs, "True", "Succeeded", "")

		By("waiting for both targets' configmaps streams to be live before any event is created")
		waitForStreamsRunning(targetA, testNs)
		waitForStreamsRunning(targetB, testNs)
	})

	AfterAll(func() {
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("keeps target A's commits as events while target B's rules churn", func() {
		// Each iteration changes target B's effective watch plan (toggling an
		// extra GVR on/off, which also churns the global informer set) and then
		// creates a brand-new ConfigMap that target A must commit. If isolation
		// holds, target A always produces an event commit; pre-fix, target B's
		// churn would force target A into a "reconciled" reconcile commit.
		for i := range 3 {
			cmName := fmt.Sprintf("iso-cm-%d", i)

			By(fmt.Sprintf("churning target B's rule set (iteration %d)", i))
			if i%2 == 0 {
				applyIsolationWatchRule(ruleB, testNs, targetB, `"configmaps", "services"`)
			} else {
				applyIsolationWatchRule(ruleB, testNs, targetB, `"configmaps"`)
			}

			By(fmt.Sprintf("creating ConfigMap %q for target A", cmName))
			applyIsolationConfigMap(cmName, testNs)

			By("asserting target A commits it as an event commit, not a reconcile")
			relPath := path.Join(pathA, fmt.Sprintf("%s/configmaps/%s.yaml", testNs, cmName))
			assertEventCommit := func(g Gomega) {
				pullLatestRepoState(g, isoRepo.CheckoutDir)

				msg := lastCommitMessageForPath(g, isoRepo.CheckoutDir, relPath)
				g.Expect(msg).To(ContainSubstring("[CREATE]"),
					"target A's commit for %s must be a [CREATE] event commit", cmName)
				g.Expect(msg).To(ContainSubstring(fmt.Sprintf("v1/configmaps/%s", cmName)),
					"target A's commit message must name the resource path")
				g.Expect(msg).NotTo(ContainSubstring("reconciled"),
					"target A must not enter reconcile mode because of target B's unrelated rule change "+
						"(GitTarget isolation — see docs/spec/gittarget-isolation-on-rule-change.md)")
			}
			Eventually(assertEventCommit).Should(Succeed())
		}
	})
})

// applyIsolationWatchRule applies a WatchRule selecting the given resources list
// (a raw YAML array body, e.g. `"configmaps", "services"`) for the target.
func applyIsolationWatchRule(name, namespace, target, resources string) {
	data := struct {
		Name            string
		Namespace       string
		DestinationName string
		Resources       string
	}{
		Name:            name,
		Namespace:       namespace,
		DestinationName: target,
		Resources:       resources,
	}
	Expect(applyFromTemplate("test/e2e/templates/manager/watchrule-resources.tmpl", data, namespace)).
		To(Succeed(), "failed to apply isolation WatchRule %q", name)
}

// applyIsolationConfigMap creates a watched ConfigMap, attributed to a fixed
// user so the commit is a genuine event commit.
func applyIsolationConfigMap(name, namespace string) {
	data := struct {
		Name      string
		Namespace string
	}{
		Name:      name,
		Namespace: namespace,
	}
	Expect(applyFromTemplate(
		"test/e2e/templates/manager/configmap.tmpl",
		data,
		namespace,
		"--as=jane@acme.com",
	)).To(Succeed(), "failed to apply isolation ConfigMap %q", name)
}

// lastCommitMessageForPath returns the body of the most recent commit that
// touched the given repo-relative path. Scoping by path keeps each GitTarget's
// history unambiguous even when several targets share one repo.
func lastCommitMessageForPath(g Gomega, checkoutDir, relPath string) string {
	out, err := gitRun(checkoutDir, "log", "-1", "--pretty=%B", "--", relPath)
	g.Expect(err).NotTo(HaveOccurred(), "git log failed: %s", out)
	return out
}
