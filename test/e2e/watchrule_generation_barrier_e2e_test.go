// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// waitForWatchRuleStreamsRunning is a barrier other specs trust: they change a rule and then write
// an object whose mirroring depends on that change having taken effect. The barrier is only worth
// anything if it refuses to pass on the PREVIOUS generation's answer, because between an edit
// landing and its reconcile a rule still carries conditions computed for the old spec — and those
// can legitimately read StreamsRunning=True.
//
// That window is short and cannot be widened from outside, so this spec does not try to catch it.
// It manufactures the same state directly: a status that says StreamsRunning=True while
// observedGeneration lags metadata.generation. The barrier must refuse it.
//
// The manufactured state is stable rather than racy because the WatchRule controller's For()
// predicate is GenerationChangedPredicate — a status-only write enqueues no reconcile, so nothing
// corrects the patch until the periodic requeue minutes later.
var _ = Describe("WatchRule generation barrier", Label("manager"), Ordered, func() {
	var (
		testNs       string
		repo         *RepoArtifacts
		providerName = "generation-barrier-provider"
		targetName   = "generation-barrier-target"
		ruleName     = "generation-barrier-rule"
	)

	const gitPath = "e2e/generation-barrier"

	BeforeAll(func() {
		By("creating the generation-barrier test namespace")
		testNs = testNamespaceFor("manager-generation-barrier")
		_, _ = kubectlRun("create", "namespace", testNs)

		By("setting up the Gitea repo and credentials")
		repo = SetupRepo(resolveE2EContext(), testNs,
			fmt.Sprintf("e2e-generation-barrier-%d", GinkgoRandomSeed()))
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to the test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		By("creating the GitProvider, GitTarget and WatchRule")
		createReadyGitProvider(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		createGitTarget(targetName, testNs, providerName, gitPath, "main")
		verifyResourceCondition("gittarget", targetName, testNs, "Validated", "True", "Succeeded", "")

		applyIsolationWatchRule(ruleName, testNs, targetName, `"configmaps"`)
	})

	AfterAll(func() {
		cleanupPipeline(testNs, providerName, targetName, ruleName)
		cleanupNamespace(testNs)
	})

	It("refuses to pass while the rule's published status describes an earlier generation", func() {
		By("waiting for the barrier to pass normally")
		waitForWatchRuleStreamsRunning(ruleName, testNs)

		// Ready, not just StreamsRunning. A rule that is still converging requeues every 10s, and
		// that reconcile would restore the observedGeneration this spec is about to rewind — the
		// manufactured state has to outlive the assertions made against it. A converged rule
		// requeues at the steady interval (minutes), which is the margin this needs.
		By("waiting for the rule to converge, so nothing is due to reconcile it")
		verifyResourceStatus("watchrule", ruleName, testNs, "True", "Succeeded", "")

		By("reading back the state the barrier accepted")
		accepted := getWatchRule(ruleName, testNs)
		generation, found, _ := unstructured.NestedInt64(accepted.Object, "metadata", "generation")
		Expect(found).To(BeTrue(), "the rule must carry a generation")
		running, why := streamsRunningAtCurrentGeneration(accepted)
		Expect(running).To(BeTrue(), "precondition: %s", why)

		// Only observedGeneration moves. StreamsRunning stays True, which is the whole point: a
		// barrier that looked at the condition alone would still pass here.
		By("rewinding status.observedGeneration while leaving StreamsRunning=True")
		patch := fmt.Sprintf(`{"status":{"observedGeneration":%d}}`, generation-1)
		_, err := kubectlRunInNamespace(testNs, "patch", "watchrule", ruleName,
			"--subresource=status", "--type=merge", "-p", patch)
		Expect(err).NotTo(HaveOccurred(), "failed to rewind the rule's observed generation")

		By("the barrier now refuses the rule")
		stale := getWatchRule(ruleName, testNs)
		staleRunning, staleWhy := streamsRunningAtCurrentGeneration(stale)
		Expect(staleRunning).To(BeFalse(),
			"a rule whose status describes an earlier generation must not satisfy the barrier")
		Expect(staleWhy).To(ContainSubstring("status is stale"),
			"the refusal must name the staleness, so a spec that trips on it is diagnosable")

		// The condition itself is untouched, so this proves the generation check did the refusing
		// rather than the rule having genuinely stopped streaming.
		Expect(watchRuleConditionStatus(stale, "StreamsRunning")).To(Equal("True"),
			"the manufactured state must keep StreamsRunning=True, or the assertion above is vacuous")

		// A status-only write enqueues no reconcile, so the refusal is a stable fact rather than a
		// race the controller is about to win.
		By("the refusal is stable, not a race with the controller")
		Consistently(func(g Gomega) {
			held, _ := streamsRunningAtCurrentGeneration(getWatchRule(ruleName, testNs))
			g.Expect(held).To(BeFalse())
		}, 6*time.Second, 2*time.Second).Should(Succeed())

		// This step is load-bearing, not cleanup. At this point status still reads
		// StreamsRunning=True against the rewound observedGeneration, so a barrier that looked at
		// the condition alone would return the instant it was called. It cannot: the edit bumps
		// metadata.generation, so the barrier has to wait out a real reconcile before the answer
		// describes the spec that is on the object.
		//
		// It is also why no separate "first observe it is not applied" wait is needed anywhere.
		// The API server bumps metadata.generation synchronously with the spec write while
		// status.observedGeneration cannot move until the controller acts, so an edited rule is
		// refused from the moment the apply returns. There is no transient window to catch, and a
		// wait for the unapplied state would only add a way to hang when the controller is quick.
		//
		// A real spec edit is also the only thing the controller's For() predicate reacts to.
		// Re-applying the SAME spec would not bump the generation, and the rule would sit refused
		// until its periodic requeue.
		By("a real spec change restores it, and the barrier passes on the new generation")
		applyIsolationWatchRule(ruleName, testNs, targetName, `"configmaps","services"`)
		waitForWatchRuleStreamsRunning(ruleName, testNs)
	})
})

// getWatchRule fetches one WatchRule as an unstructured object.
func getWatchRule(name, ns string) unstructured.Unstructured {
	GinkgoHelper()
	output, err := kubectlRunInNamespace(ns, "get", "watchrule", name, "-o", "json")
	Expect(err).NotTo(HaveOccurred(), "failed to read watchrule %s/%s", ns, name)
	var obj unstructured.Unstructured
	Expect(json.Unmarshal([]byte(output), &obj)).To(Succeed())
	return obj
}

// watchRuleConditionStatus returns one condition's status, or "" when the condition is absent.
func watchRuleConditionStatus(obj unstructured.Unstructured, conditionType string) string {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, cond := range conditions {
		condMap, ok := cond.(map[string]interface{})
		if !ok || condMap["type"] != conditionType {
			continue
		}
		status, _ := condMap["status"].(string)
		return status
	}
	return ""
}

// The barrier's predicate, exercised directly. The spec above proves the manufactured state is
// reachable and stable against a real API server; this proves the predicate's verdicts, including
// the cases a cluster will not produce on demand.
var _ = Describe("WatchRule generation barrier predicate", Label("manager"), func() {
	ruleWithOtherCondition := func(generation, observed int64) unstructured.Unstructured {
		return unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"generation": generation},
			"status": map[string]interface{}{
				"observedGeneration": observed,
				"conditions": []interface{}{
					map[string]interface{}{"type": "Ready", "status": "True"},
				},
			},
		}}
	}

	rule := func(generation, observed any, conditionStatus string) unstructured.Unstructured {
		obj := unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{},
			"status":   map[string]interface{}{},
		}}
		if generation != nil {
			obj.Object["metadata"].(map[string]interface{})["generation"] = generation
		}
		if observed != nil {
			obj.Object["status"].(map[string]interface{})["observedGeneration"] = observed
		}
		if conditionStatus != "" {
			obj.Object["status"].(map[string]interface{})["conditions"] = []interface{}{
				map[string]interface{}{
					"type": "StreamsRunning", "status": conditionStatus, "message": "0/1 streams running",
				},
			}
		}
		return obj
	}

	DescribeTable("accepts only a current generation that is streaming",
		func(obj unstructured.Unstructured, wantRunning bool, wantWhy string) {
			running, why := streamsRunningAtCurrentGeneration(obj)
			Expect(running).To(Equal(wantRunning))
			if wantWhy != "" {
				Expect(why).To(ContainSubstring(wantWhy))
			}
		},
		Entry("current generation, streaming", rule(int64(3), int64(3), "True"), true, ""),
		Entry("stale generation, still reporting streaming",
			rule(int64(3), int64(2), "True"), false, "status is stale"),
		Entry("current generation, not streaming",
			rule(int64(3), int64(3), "False"), false, "StreamsRunning is False"),
		Entry("never reconciled", rule(int64(1), nil, ""), false, "status.observedGeneration is absent"),
		// Neither field present would compare 0 == 0 under a defaulting read, which is the exact
		// shape a barrier must not accept.
		Entry("no generation at all", rule(nil, nil, "True"), false, "metadata.generation is absent"),
		Entry("no generation, but an observed one", rule(nil, int64(0), "True"), false,
			"metadata.generation is absent"),
		Entry("current generation, no conditions at all",
			rule(int64(2), int64(2), ""), false, "status.conditions is absent"),
		Entry("current generation, other conditions but no StreamsRunning",
			ruleWithOtherCondition(int64(2), int64(2)), false, "no StreamsRunning condition"),
	)
})
