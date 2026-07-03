// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"filippo.io/age"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

const defaultE2EAgeKeyPath = "/tmp/e2e-age-key.txt"

func readSOPSAgeKeyFromFile(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read age key file: %w", err)
	}

	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "AGE-SECRET-KEY-") {
			return trimmed, nil
		}
	}

	return "", fmt.Errorf("no AGE-SECRET-KEY found in %s", path)
}

func getE2EAgeKeyPath() string {
	if value := strings.TrimSpace(os.Getenv("E2E_AGE_KEY_FILE")); value != "" {
		return value
	}
	return defaultE2EAgeKeyPath
}

func deriveAgeRecipient(identityString string) (string, error) {
	identity, err := age.ParseX25519Identity(strings.TrimSpace(identityString))
	if err != nil {
		return "", fmt.Errorf("parse age identity: %w", err)
	}
	return identity.Recipient().String(), nil
}

func decryptWithControllerSOPS(ciphertext []byte, ageKey string) (string, error) {
	podName, err := discoverControllerPodName(namespace)
	if err != nil {
		return "", err
	}

	cmd := kubectlCmdInNamespace(
		context.Background(),
		namespace,
		"exec",
		"-i",
		podName,
		"--",
		"env", fmt.Sprintf("SOPS_AGE_KEY=%s", ageKey),
		"/usr/local/bin/sops", "--decrypt", "--input-type", "yaml", "--output-type", "yaml", "/dev/stdin",
	)
	cmd.Stdin = bytes.NewReader(ciphertext)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("decrypt with controller sops failed: %w: %s", err, strings.TrimSpace(string(output)))
	}

	return string(output), nil
}

func discoverControllerPodName(ns string) (string, error) {
	deploymentsOutput, err := kubectlRunInNamespace(
		ns,
		"get",
		"deployments",
		"-o",
		"jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}",
	)
	if err != nil {
		return "", fmt.Errorf("get deployments in namespace %s: %w", ns, err)
	}

	deployments := utils.GetNonEmptyLines(deploymentsOutput)
	if len(deployments) != 1 {
		return "", fmt.Errorf("expected exactly 1 Deployment in namespace %s, found %d", ns, len(deployments))
	}
	deploymentName := deployments[0]

	deploymentOutput, err := kubectlRunInNamespace(ns, "get", "deployment", deploymentName, "-o", "json")
	if err != nil {
		return "", fmt.Errorf("get deployment %s/%s: %w", ns, deploymentName, err)
	}

	var deploymentObj unstructured.Unstructured
	if unmarshalErr := json.Unmarshal([]byte(deploymentOutput), &deploymentObj); unmarshalErr != nil {
		return "", fmt.Errorf("unmarshal deployment %s/%s: %w", ns, deploymentName, unmarshalErr)
	}

	matchLabels, found, matchLabelErr := unstructured.NestedStringMap(
		deploymentObj.Object,
		"spec",
		"selector",
		"matchLabels",
	)
	if matchLabelErr != nil {
		return "", fmt.Errorf("read deployment selector labels %s/%s: %w", ns, deploymentName, matchLabelErr)
	}
	if !found || len(matchLabels) == 0 {
		return "", fmt.Errorf("deployment selector labels are empty for %s/%s", ns, deploymentName)
	}

	keys := make([]string, 0, len(matchLabels))
	for key := range matchLabels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	selectorParts := make([]string, 0, len(keys))
	for _, key := range keys {
		selectorParts = append(selectorParts, fmt.Sprintf("%s=%s", key, matchLabels[key]))
	}
	selector := strings.Join(selectorParts, ",")

	podOutput, err := kubectlRunInNamespace(
		ns,
		"get",
		"pods",
		"-l",
		selector,
		"-o",
		"jsonpath={.items[0].metadata.name}",
	)
	if err != nil {
		return "", fmt.Errorf("get controller pod for selector %q in namespace %s: %w", selector, ns, err)
	}
	podName := strings.TrimSpace(podOutput)
	if podName == "" {
		return "", fmt.Errorf("controller pod name is empty for selector %q in namespace %s", selector, ns)
	}

	return podName, nil
}

const (
	// defaultResourceConditionTimeout / resourceConditionPollInterval back the shared
	// condition-wait helpers below. 90s (vs Gomega's 30s default) absorbs slow-CI and
	// parallel load: after a fresh CRD install the controller's discovery cache can lag
	// tens of seconds before it serves the new GVR and a dependent rule can be planned
	// and reach Ready. The happy path still returns as soon as the condition matches.
	defaultResourceConditionTimeout = "90s"
	resourceConditionPollInterval   = "2s"
)

// resourceConditionTimeout returns the caller-supplied override, or the shared default
// when none (or an empty string) is given.
func resourceConditionTimeout(override []string) string {
	if len(override) > 0 && override[0] != "" {
		return override[0]
	}
	return defaultResourceConditionTimeout
}

// verifyResourceStatus verifies a resource's status conditions match expected values.
// For cluster-scoped resources, provide an empty namespace.
func verifyResourceStatus(resourceType, name, ns, expectedStatus, expectedReason, expectedMessageContains string) {
	verifyResourceCondition(
		resourceType,
		name,
		ns,
		"Ready",
		expectedStatus,
		expectedReason,
		expectedMessageContains,
	)
}

// verifyResourceCondition blocks until the named resource publishes conditionType with the
// expected status (and, when non-empty, reason and message substring). It is the single
// canonical "wait for a CRD status condition" primitive — an empty expectedReason skips the
// reason check (status-only gate), mirroring expectedMessageContains. Pass an optional timeout
// string (e.g. "150s") to override the shared default for slower transitions.
func verifyResourceCondition(
	resourceType, name, ns, conditionType, expectedStatus, expectedReason, expectedMessageContains string,
	timeout ...string,
) {
	By(
		fmt.Sprintf(
			"verifying %s '%s' in ns '%s' condition %s is '%s' with reason '%s'",
			resourceType,
			name,
			ns,
			conditionType,
			expectedStatus,
			expectedReason,
		),
	)
	verifyStatus := func(g Gomega) {
		args := []string{"get", resourceType, name, "-o", "json"}
		if ns != "" {
			args = append(args, "-n", ns)
		}

		output, err := kubectlRun(args...)
		g.Expect(err).NotTo(HaveOccurred())

		var obj unstructured.Unstructured
		g.Expect(json.Unmarshal([]byte(output), &obj)).To(Succeed())

		conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		g.Expect(found).To(BeTrue(), "status.conditions not found")

		var conditionStatus, conditionReason, conditionMessage string
		for _, cond := range conditions {
			condMap, ok := cond.(map[string]interface{})
			if !ok {
				continue
			}
			if condMap["type"] == conditionType {
				conditionStatus, _ = condMap["status"].(string)
				conditionReason, _ = condMap["reason"].(string)
				conditionMessage, _ = condMap["message"].(string)
				break
			}
		}

		g.Expect(conditionStatus).To(Equal(expectedStatus))
		switch {
		case expectedReason == "":
			// Status-only gate: caller does not assert a reason.
		case resourceType == "gittarget" && conditionType == "Ready" && expectedReason == "Ready":
			g.Expect([]string{"Ready", "OK"}).To(ContainElement(conditionReason))
		default:
			g.Expect(conditionReason).To(Equal(expectedReason))
		}
		if expectedMessageContains != "" {
			g.Expect(conditionMessage).To(ContainSubstring(expectedMessageContains))
		}
	}
	Eventually(verifyStatus, resourceConditionTimeout(timeout), resourceConditionPollInterval).Should(Succeed())
}

// waitForStreamsRunning blocks until the GitTarget reports StreamsRunning=True. Specs that assert
// live per-event behavior, authorship, or ordering must create asserted resources only after this
// point so the relevant watches are past their replay watermark.
func waitForStreamsRunning(name, ns string) {
	GinkgoHelper()
	verifyResourceCondition("gittarget", name, ns, "StreamsRunning", "True", "", "", "120s")
}

func waitForWatchRuleStreamsRunning(name, ns string) { //nolint:unused // Helper for specs that need a rule-scoped gate.
	GinkgoHelper()
	verifyResourceCondition("watchrule", name, ns, "StreamsRunning", "True", "", "", "120s")
}

// createReadyGitProvider creates a GitProvider (branch "main", no commit window) and blocks until
// it validates repo connectivity (Ready=True). It folds the create+verify pair most specs repeat.
func createReadyGitProvider(name, ns, secretName, repoURL string) {
	GinkgoHelper()
	createGitProviderWithURLInNamespace(name, ns, secretName, repoURL)
	verifyResourceStatus("gitprovider", name, ns, "True", "Ready", "")
}

// createReadyGitProviderWithCommitWindow is createReadyGitProvider with an explicit commit window.
func createReadyGitProviderWithCommitWindow(name, ns, secretName, repoURL, commitWindow string) {
	GinkgoHelper()
	createGitProviderWithCommitWindow(name, ns, secretName, repoURL, commitWindow)
	verifyResourceStatus("gitprovider", name, ns, "True", "Ready", "")
}

// createValidatedGitTarget creates a GitTarget on the "main" branch and blocks until it accepts
// its config (Validated=True). A GitTarget only reaches Ready once a WatchRule wires watches and
// their streams finish replay, so Validated is the meaningful "config accepted" gate at creation
// time; callers gate on the live data plane separately via waitForStreamsRunning.
func createValidatedGitTarget(name, ns, providerName, path string) {
	GinkgoHelper()
	createGitTarget(name, ns, providerName, path, "main")
	verifyResourceCondition("gittarget", name, ns, "Validated", "True", "OK", "")
}

// cleanupPipeline deletes the three namespaced pipeline resources in dependency order (rule, then
// target, then provider) so finalizers settle before the namespace is removed. Callers delete the
// namespace separately (see cleanupNamespace).
func cleanupPipeline(ns, provider, target, watchRule string) {
	GinkgoHelper()
	cleanupWatchRule(watchRule, ns)
	cleanupGitTarget(target, ns)
	cleanupNamespacedResource(ns, "gitprovider", provider)
}

// showControllerLogs displays the current controller logs to help with debugging during test execution.
func showControllerLogs(context string) {
	By(fmt.Sprintf("📋 Controller logs %s:", context))

	// Get the controller pod name dynamically
	podName, err := kubectlRunInNamespace(
		namespace,
		"get",
		"pods",
		"-l",
		"control-plane=gitops-reverser",
		"-o",
		"jsonpath={.items[0].metadata.name}",
	)
	if err != nil {
		fmt.Printf("⚠️  Failed to get controller pod name: %v\n", err)
		return
	}

	if strings.TrimSpace(podName) == "" {
		fmt.Printf("⚠️  Controller pod not found yet\n")
		return
	}

	// Get the logs
	output, err := kubectlRunInNamespace(namespace, "logs", strings.TrimSpace(podName), "--tail=20")
	if err != nil {
		fmt.Printf("❌ Failed to get controller logs: %v\n", err)
		return
	}

	fmt.Printf("🔍 Recent controller logs (%s):\n", context)
	fmt.Printf("----------------------------------------\n")
	fmt.Printf("%s\n", output)
	fmt.Printf("----------------------------------------\n")
}
