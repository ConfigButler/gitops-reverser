// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// This spec proves rules[].objectSelector against a real kube-apiserver: the server decides
// membership, so label entry and exit arrive as ADDED and DELETED, and a complete selected snapshot
// under prune.mode Always removes every document the selection does not return, including ones it
// never matched. Unit fakes cannot establish those server semantics.
//
// Two targets watch the same ConfigMaps with the same selector: one Always, one on the default
// (OnEvent). Each retention claim is made only after the Always target has visibly acted on the
// same snapshot, so "still present" means "decided to keep", not "not processed yet".
var _ = Describe("Manager WatchRule objectSelector", Label("manager"), Ordered, func() {
	const (
		providerName  = "gitprovider-selector"
		alwaysTarget  = "selector-always-target"
		onEventTarget = "selector-onevent-target"
		alwaysPath    = "e2e/selector-always"
		onEventPath   = "e2e/selector-onevent"
		alwaysRule    = "selector-always-rule"
		onEventRule   = "selector-onevent-rule"
		selectedLabel = "e2e.configbutler.ai/mirror"
	)

	var (
		testNs string
		repo   *RepoArtifacts
	)

	selectedBy := func(values ...string) string {
		quoted := make([]string, 0, len(values))
		for _, v := range values {
			quoted = append(quoted, strconv.Quote(v))
		}
		return fmt.Sprintf(`    objectSelector:
      matchExpressions:
        - key: %s
          operator: In
          values: [%s]
`, selectedLabel, strings.Join(quoted, ", "))
	}

	applySelectorRule := func(name, target, selector string) {
		GinkgoHelper()
		manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: %s
  namespace: %s
spec:
  gitTargetRef:
    name: %s
  rules:
  - apiGroups: [""]
    resources: ["configmaps"]
%s`, name, testNs, target, selector)
		out, err := kubectlRunWithStdin(testNs, manifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "apply WatchRule %q: %s", name, out)
	}

	applyConfigMap := func(name, labelValue string) {
		GinkgoHelper()
		labels := ""
		if labelValue != "" {
			labels = fmt.Sprintf("  labels:\n    %s: %q\n", selectedLabel, labelValue)
		}
		manifest := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
%sdata:
  key: value
`, name, testNs, labels)
		out, err := kubectlRunWithStdin(testNs, manifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "apply ConfigMap %q: %s", name, out)
	}

	both := func(name string) (string, string) {
		return pruneConfigMapPath(alwaysPath, testNs, name), pruneConfigMapPath(onEventPath, testNs, name)
	}

	stillPresent := func(relPath, why string) {
		GinkgoHelper()
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			_, statErr := os.Stat(filepath.Join(repo.CheckoutDir, relPath))
			g.Expect(statErr).NotTo(HaveOccurred(), why)
		}, 15*time.Second, 3*time.Second).Should(Succeed())
	}

	BeforeAll(func() {
		testNs = testNamespaceFor("manager-selector")
		_, _ = kubectlRun("create", "namespace", testNs)

		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-manager-selector-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		createReadyGitProvider(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)

		applyPruneGitTarget(alwaysTarget, testNs, providerName, alwaysPath, "Always")
		applyPruneGitTarget(onEventTarget, testNs, providerName, onEventPath, "")
		for _, name := range []string{alwaysTarget, onEventTarget} {
			verifyResourceCondition("gittarget", name, testNs, "Validated", "True", "Succeeded", "")
		}

		applySelectorRule(alwaysRule, alwaysTarget, selectedBy("yes"))
		applySelectorRule(onEventRule, onEventTarget, selectedBy("yes"))
		for _, name := range []string{alwaysRule, onEventRule} {
			verifyResourceStatus("watchrule", name, testNs, "True", "Succeeded", "")
			waitForWatchRuleStreamsRunning(name, testNs)
		}
	})

	AfterAll(func() {
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(90 * time.Second)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("mirrors only selected objects, and follows label entry and exit", func() {
		By("creating an unselected ConfigMap, then a selected one")
		applyConfigMap("selector-unlabelled", "")
		applyConfigMap("selector-member", "yes")
		alwaysMember, onEventMember := both("selector-member")
		waitForPruneFile(repo, alwaysMember, true)
		waitForPruneFile(repo, onEventMember, true)

		By("the unselected ConfigMap, created first, never reached either mirror")
		alwaysUnlabelled, onEventUnlabelled := both("selector-unlabelled")
		waitForPruneFile(repo, alwaysUnlabelled, false)
		waitForPruneFile(repo, onEventUnlabelled, false)

		By("removing the label is a removal under both OnEvent and Always")
		_, err := kubectlRunInNamespace(testNs, "label", "configmap", "selector-member", selectedLabel+"-")
		Expect(err).NotTo(HaveOccurred())
		waitForPruneFile(repo, alwaysMember, false)
		waitForPruneFile(repo, onEventMember, false)

		By("adding the label brings an existing object in without any other change")
		_, err = kubectlRunInNamespace(testNs, "label", "configmap", "selector-unlabelled", selectedLabel+"=yes")
		Expect(err).NotTo(HaveOccurred())
		waitForPruneFile(repo, alwaysUnlabelled, true)
		waitForPruneFile(repo, onEventUnlabelled, true)
	})

	It("sweeps never-selected documents from a complete snapshot only under Always", func() {
		By("keeping a ConfigMap in the cluster that the selector does not return")
		applyConfigMap("selector-outsider", "no")
		alwaysOutsider, onEventOutsider := both("selector-outsider")
		seedOrphanManifests(repo, testNs, map[string]string{
			alwaysOutsider:  orphanConfigMapYAML("selector-outsider", testNs),
			onEventOutsider: orphanConfigMapYAML("selector-outsider", testNs),
		})
		waitForPruneFile(repo, alwaysOutsider, true)

		By("keeping an unchanged ConfigMap in the cluster that only the widened selector returns")
		applyConfigMap("selector-widened", "also")
		alwaysWidened, onEventWidened := both("selector-widened")

		// Widening the selector is a new collection, so both targets take a complete snapshot of it.
		// The outsider still does not match: it is in the cluster but absent from the selection.
		By("widening both selectors, which starts a new collection and a complete snapshot")
		applySelectorRule(alwaysRule, alwaysTarget, selectedBy("yes", "also"))
		applySelectorRule(onEventRule, onEventTarget, selectedBy("yes", "also"))
		waitForWatchRuleStreamsRunning(alwaysRule, testNs)
		waitForWatchRuleStreamsRunning(onEventRule, testNs)

		By("the Always target removes the document the selection never returned")
		waitForPruneFile(repo, alwaysOutsider, false)

		By("the OnEvent target keeps it, and reports it as retained")
		stillPresent(onEventOutsider, "OnEvent must not infer a removal from a snapshot")
		Eventually(func(g Gomega) {
			g.Expect(retainedDocumentsOf(g, onEventTarget, testNs)).To(BeNumerically(">", 0))
		}).Should(Succeed())

		By("selected objects survive the new snapshot in both mirrors")
		alwaysSelected, onEventSelected := both("selector-unlabelled")
		stillPresent(alwaysSelected, "a selected object must survive its own snapshot")
		waitForPruneFile(repo, onEventSelected, true)

		By("the object only the widened selector returns reaches both mirrors, though it never changed")
		waitForPruneFile(repo, alwaysWidened, true)
		waitForPruneFile(repo, onEventWidened, true)

		// Narrowing is a new collection too. The widened-only object leaves the selection without any
		// change of its own, so only the snapshot can take it out, and only under Always.
		By("narrowing both selectors back")
		applySelectorRule(alwaysRule, alwaysTarget, selectedBy("yes"))
		applySelectorRule(onEventRule, onEventTarget, selectedBy("yes"))
		waitForWatchRuleStreamsRunning(alwaysRule, testNs)
		waitForWatchRuleStreamsRunning(onEventRule, testNs)

		By("the Always target removes the object the narrowed selection no longer returns")
		waitForPruneFile(repo, alwaysWidened, false)

		By("the OnEvent target keeps it, and both keep the object still selected")
		stillPresent(onEventWidened, "OnEvent must not infer a removal from a snapshot")
		stillPresent(alwaysSelected, "a selected object must survive a narrowing snapshot")
		waitForPruneFile(repo, onEventSelected, true)
	})

	// Offline recovery: while a target is suspended its writes are dropped, never replayed, so a
	// deletion and a label exit made then reach Git only through the next complete snapshot. That
	// snapshot removes both under Always and keeps both under OnEvent.
	It("recovers removals made while the targets were not writing from the next snapshot", func() {
		applyConfigMap("selector-offline-deleted", "yes")
		applyConfigMap("selector-offline-exit", "yes")
		alwaysDeleted, onEventDeleted := both("selector-offline-deleted")
		alwaysExit, onEventExit := both("selector-offline-exit")
		for _, p := range []string{alwaysDeleted, onEventDeleted, alwaysExit, onEventExit} {
			waitForPruneFile(repo, p, true)
		}

		By("suspending both targets")
		for _, target := range []string{alwaysTarget, onEventTarget} {
			_, err := kubectlRunInNamespace(testNs, "patch", "gittarget", target, "--type=merge",
				"-p", `{"spec":{"suspend":true}}`)
			Expect(err).NotTo(HaveOccurred())
			verifyResourceCondition("gittarget", target, testNs, "Ready", "True", "Suspended", "")
		}

		By("deleting one selected object and relabeling the other out of the selection")
		_, err := kubectlRunInNamespace(testNs, "delete", "configmap", "selector-offline-deleted")
		Expect(err).NotTo(HaveOccurred())
		_, err = kubectlRunInNamespace(testNs, "label", "configmap", "selector-offline-exit", selectedLabel+"-")
		Expect(err).NotTo(HaveOccurred())
		stillPresent(alwaysDeleted, "a suspended target must not write the removal")

		By("resuming, and asking for a fresh snapshot")
		for _, target := range []string{alwaysTarget, onEventTarget} {
			_, err := kubectlRunInNamespace(testNs, "patch", "gittarget", target, "--type=merge",
				"-p", `{"spec":{"suspend":false}}`)
			Expect(err).NotTo(HaveOccurred())
			_, err = kubectlRunInNamespace(testNs, "annotate", "gittarget", target,
				"reconcile.configbutler.ai/requestedAt="+time.Now().UTC().Format(time.RFC3339Nano), "--overwrite")
			Expect(err).NotTo(HaveOccurred())
		}

		By("the Always target removes both from the snapshot")
		waitForPruneFile(repo, alwaysDeleted, false)
		waitForPruneFile(repo, alwaysExit, false)

		By("the OnEvent target keeps both: the removals were never observed while it wrote")
		stillPresent(onEventDeleted, "OnEvent must not infer a removal from a snapshot")
		stillPresent(onEventExit, "OnEvent must not infer a removal from a snapshot")
	})

	It("refuses a newer rule that selects an overlapping collection differently", func() {
		const conflicting = "selector-conflicting-rule"
		applySelectorRule(conflicting, alwaysTarget, selectedBy("other"))
		verifyResourceCondition("watchrule", conflicting, testNs, "ResourcesResolved", "False",
			"ObjectSelectorConflict", "")
		verifyResourceCondition("watchrule", alwaysRule, testNs, "ResourcesResolved", "True", "", "")

		By("the older rule's mirror keeps following its own selection")
		applyConfigMap("selector-after-conflict", "yes")
		alwaysAfter, _ := both("selector-after-conflict")
		waitForPruneFile(repo, alwaysAfter, true)
		_, err := kubectlRunInNamespace(testNs, "delete", "watchrule", conflicting)
		Expect(err).NotTo(HaveOccurred())
	})
})
