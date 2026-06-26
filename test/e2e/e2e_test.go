/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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

// verifyResourceStatus verifies a resource's status conditions match expected values.
// For cluster-scoped resources, provide an empty namespace.
func verifyResourceStatus(resourceType, name, ns, expectedStatus, expectedReason, expectedMessageContains string) {
	By(
		fmt.Sprintf(
			"verifying %s '%s' in ns '%s' status is '%s' with reason '%s'",
			resourceType,
			name,
			ns,
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

		var readyStatus, readyReason, readyMessage string
		for _, cond := range conditions {
			condMap, ok := cond.(map[string]interface{})
			if !ok {
				continue
			}
			if condMap["type"] == "Ready" {
				readyStatus, _ = condMap["status"].(string)
				readyReason, _ = condMap["reason"].(string)
				readyMessage, _ = condMap["message"].(string)
				break
			}
		}

		g.Expect(readyStatus).To(Equal(expectedStatus))
		if resourceType == "gittarget" && expectedReason == "Ready" {
			g.Expect([]string{"Ready", "OK"}).To(ContainElement(readyReason))
		} else {
			g.Expect(readyReason).To(Equal(expectedReason))
		}
		if expectedMessageContains != "" {
			g.Expect(readyMessage).To(ContainSubstring(expectedMessageContains))
		}
	}
	// 90s (vs the 30s default) absorbs slow-CI and parallel load: in particular,
	// after a fresh CRD install the controller's discovery cache can lag tens of
	// seconds before it serves the new GVR and a dependent WatchRule can be
	// planned and reach Ready. The happy path still returns as soon as Ready.
	Eventually(verifyStatus, "90s", "2s").Should(Succeed())
}

// waitForStreamsReady blocks until the GitTarget reports StreamsReady=True. Specs that assert
// live per-event behavior, authorship, or ordering must create asserted resources only after this
// point so the relevant watches are past their replay watermark.
func waitForStreamsReady(name, ns string) {
	By(fmt.Sprintf("waiting for gittarget '%s' in ns '%s' to report StreamsReady=True", name, ns))
	_, err := kubectlRunInNamespace(ns, "wait", "--for=condition=StreamsReady=true",
		"gittarget/"+name, "--timeout=120s")
	Expect(err).NotTo(HaveOccurred(), "gittarget %s/%s did not reach StreamsReady=True", ns, name)
}

func waitForWatchRuleStreamsReady(name, ns string) { //nolint:unused // Helper for specs that need a rule-scoped gate.
	By(fmt.Sprintf("waiting for watchrule '%s' in ns '%s' to report StreamsReady=True", name, ns))
	_, err := kubectlRunInNamespace(ns, "wait", "--for=condition=StreamsReady=true",
		"watchrule/"+name, "--timeout=120s")
	Expect(err).NotTo(HaveOccurred(), "watchrule %s/%s did not reach StreamsReady=True", ns, name)
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
