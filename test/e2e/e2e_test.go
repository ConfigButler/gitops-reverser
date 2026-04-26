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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

const defaultE2EAgeKeyPath = "/tmp/e2e-age-key.txt"

// managerRepo holds the file-local repo fixtures for the Manager describe block.
var managerRepo *RepoArtifacts

var _ = Describe("Manager", Label("manager"), Ordered, func() {
	var controllerPodName string // Name of first controller pod for logging
	var testNs string

	// Before running the tests, set up per-run test fixtures.
	BeforeAll(func() {
		By("setting up Prometheus client for metrics testing")
		setupPrometheusClient()
		verifyPrometheusAvailable()

		By("creating test namespace")
		testNs = testNamespaceFor("manager")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		By("setting up Gitea repo and credentials for manager tests")
		managerRepo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-manager-%d", GinkgoRandomSeed()),
		)

		By("applying git secrets to test namespace")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", managerRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)
	})

	// After all tests have been executed, clean up the test namespace
	AfterAll(func() {
		cleanupNamespace(testNs)

		By("test infrastructure still running for debugging")
		fmt.Printf("\n")
		fmt.Printf("═══════════════════════════════════════════════════════════\n")
		fmt.Printf("📊 E2E Infrastructure kept running for debugging purposes:\n")
		fmt.Printf("═══════════════════════════════════════════════════════════\n")
		fmt.Printf("  Prometheus: http://localhost:19090\n")
		fmt.Printf("  Gitea:      http://localhost:13000\n")
		fmt.Printf("\n")
		fmt.Printf("═══════════════════════════════════════════════════════════\n")
		fmt.Printf("\n")
	})

	// Optimize timeouts for faster test execution
	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {

		It("should run successfully", Label("smoke"), func() {
			By("validating that the gitops-reverser pods are running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the names of the gitops-reverser pods
				podOutput, err := kubectlRunInNamespace(
					namespace,
					"get",
					"pods",
					"-l",
					"control-plane=gitops-reverser",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
				)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve gitops-reverser pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected exactly 1 controller pod running")
				controllerPodName = podNames[0] // Use first pod for logging
				g.Expect(controllerPodName).To(ContainSubstring("gitops-reverser"))

				// Validate all pods' status
				for _, podName := range podNames {
					output, err := kubectlRunInNamespace(
						namespace,
						"get",
						"pods",
						podName,
						"-o",
						"jsonpath={.status.phase}",
					)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal("Running"), fmt.Sprintf("Incorrect status for pod %s", podName))
				}
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should expose the controller service", Label("smoke"), func() {
			By("verifying controller service exists")
			_, err := kubectlRunInNamespace(namespace, "get", "svc", controllerServiceName)
			Expect(err).NotTo(HaveOccurred(), "Controller service should exist")

			By("verifying controller service routes to the controller pod")
			Eventually(func(g Gomega) {
				output, endpointsErr := kubectlRunInNamespace(
					namespace,
					"get",
					"endpoints",
					controllerServiceName,
					"-o",
					"jsonpath={.subsets[*].addresses[*].targetRef.name}",
				)
				g.Expect(endpointsErr).NotTo(HaveOccurred(), "Failed to get controller service endpoints")

				lines := utils.GetNonEmptyLines(output)
				podSet := map[string]struct{}{}
				for _, line := range lines {
					if !strings.HasPrefix(line, "Warning:") &&
						!strings.Contains(line, "deprecated") &&
						strings.Contains(line, "gitops-reverser") {
						podSet[line] = struct{}{}
					}
				}
				var podNames []string
				for podName := range podSet {
					podNames = append(podNames, podName)
				}

				g.Expect(podNames).To(HaveLen(1), "controller service should route to exactly 1 pod")
				g.Expect(podNames[0]).To(Equal(controllerPodName), "controller service should route to controller pod")
			}, 30*time.Second).Should(Succeed())
		})

		It("should expose the audit service separately", func() {
			By("verifying audit service exists")
			_, err := kubectlRunInNamespace(namespace, "get", "svc", auditServiceName)
			Expect(err).NotTo(HaveOccurred(), "Audit service should exist")

			By("verifying audit service routes to the controller pod")
			Eventually(func(g Gomega) {
				output, endpointsErr := kubectlRunInNamespace(
					namespace,
					"get",
					"endpoints",
					auditServiceName,
					"-o",
					"jsonpath={.subsets[*].addresses[*].targetRef.name}",
				)
				g.Expect(endpointsErr).NotTo(HaveOccurred(), "Failed to get audit service endpoints")

				lines := utils.GetNonEmptyLines(output)
				var podNames []string
				for _, line := range lines {
					if strings.HasPrefix(line, "Warning:") || strings.Contains(line, "deprecated") {
						continue
					}
					podNames = append(podNames, line)
				}

				g.Expect(podNames).To(HaveLen(1), "audit service should route to exactly 1 pod")
				g.Expect(podNames[0]).To(Equal(controllerPodName), "audit service should route to controller pod")
			}, 30*time.Second).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", Label("smoke"), func() {
			By("validating that the controller service is available for metrics")
			_, err := kubectlRunInNamespace(namespace, "get", "service", controllerServiceName)
			Expect(err).NotTo(HaveOccurred(), "Controller service should exist")

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				output, err := kubectlRunInNamespace(namespace, "get", "endpoints", controllerServiceName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("8443"), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				output, err := kubectlRunInNamespace(namespace, "logs", controllerPodName)
				g.Expect(err).NotTo(HaveOccurred())
				jsonMetricsLogLine := "\"logger\":\"controller-runtime.metrics\"," +
					"\"msg\":\"Serving metrics server\""
				g.Expect(output).To(
					SatisfyAny(
						ContainSubstring("controller-runtime.metrics\tServing metrics server"),
						ContainSubstring(jsonMetricsLogLine),
					),
					"Metrics server not yet started",
				)
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

			By("waiting for Prometheus to scrape controller metrics")
			waitForMetric("sum(up{job='gitops-reverser'})",
				func(v float64) bool { return v == 1 },
				"metrics endpoint should be up")

			By("verifying basic process metrics are exposed")
			waitForMetric("sum(process_cpu_seconds_total{job='gitops-reverser'})",
				func(v float64) bool { return v > 0 },
				"process metrics should exist")

			By("verifying metrics from the controller pod")
			podCount, err := queryPrometheus("sum(up{job='gitops-reverser'})")
			Expect(err).NotTo(HaveOccurred())
			Expect(podCount).To(BeEquivalentTo(1), "Should scrape from 1 controller pod")

			fmt.Printf("✅ Metrics collection verified from %.0f pods\n", podCount)
			fmt.Printf("📊 Inspect metrics: %s\n", getPrometheusURL())
		})

		It("should receive audit webhook events from kube-apiserver", Label("smoke"), func() {
			By("recording baseline audit event count")
			baselineAuditEvents, err := queryPrometheus("sum(gitopsreverser_audit_events_received_total) or vector(0)")
			Expect(err).NotTo(HaveOccurred())
			fmt.Printf("📊 Baseline audit events: %.0f\n", baselineAuditEvents)
			baselineClusterAuditEvents, err := queryPrometheus(
				"sum(gitopsreverser_audit_events_received_total{cluster_id='kind-e2e'}) or vector(0)",
			)
			Expect(err).NotTo(HaveOccurred())
			fmt.Printf("📊 Baseline kind-e2e audit events: %.0f\n", baselineClusterAuditEvents)

			By("creating a ConfigMap to trigger audit events")
			_, err = kubectlRunInNamespace(
				namespace,
				"create",
				"configmap",
				"audit-test-cm",
				"--from-literal=test=audit",
			)
			Expect(err).NotTo(HaveOccurred(), "ConfigMap creation should succeed")
			_, err = kubectlRunInNamespace(
				namespace,
				"patch",
				"configmap",
				"audit-test-cm",
				"--type=merge",
				"--patch",
				`{"data":{"test":"audit-updated"}}`,
			)
			Expect(err).NotTo(HaveOccurred(), "ConfigMap update should succeed")

			By("waiting for audit event metric to increment")
			waitForMetricWithTimeout("sum(gitopsreverser_audit_events_received_total) or vector(0)",
				func(v float64) bool { return v > baselineAuditEvents },
				"audit events should increment", 2*time.Minute)
			waitForMetricWithTimeout(
				"sum(gitopsreverser_audit_events_received_total{cluster_id='kind-e2e'}) or vector(0)",
				func(v float64) bool { return v > baselineClusterAuditEvents },
				"audit events should increment for cluster_id=kind-e2e", 2*time.Minute,
			)

			By("verifying audit events were received")
			currentAuditEvents, err := queryPrometheus("sum(gitopsreverser_audit_events_received_total) or vector(0)")
			Expect(err).NotTo(HaveOccurred())
			Expect(currentAuditEvents).To(BeNumerically(">", baselineAuditEvents),
				"Should have received audit events from kube-apiserver")

			newEvents := currentAuditEvents - baselineAuditEvents
			fmt.Printf("✅ Received %.0f new audit events from kube-apiserver\n", newEvents)
			fmt.Printf("📊 Total audit events: %.0f\n", currentAuditEvents)

			By("cleaning up audit test resources")
			_, _ = kubectlRunInNamespace(
				namespace,
				"delete",
				"configmap",
				"audit-test-cm",
				"--ignore-not-found=true",
			)
		})

		It("should validate GitProvider with real Gitea repository", Label("smoke"), func() {
			gitProviderName := "gitprovider-e2e-test"

			By("showing initial controller logs")
			showControllerLogs("before creating GitProvider")

			createGitProviderWithURLInNamespace(
				gitProviderName,
				testNs,
				managerRepo.GitSecretHTTP,
				managerRepo.RepoURLHTTP,
			)

			By("showing controller logs after GitProvider creation")
			showControllerLogs("after creating GitProvider")

			verifyResourceStatus(
				"gitprovider", gitProviderName, testNs,
				"True", "Ready", "Repository connectivity validated",
			)

			By("showing final controller logs")
			showControllerLogs("after status verification")

			_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", gitProviderName, "--ignore-not-found=true")
		})

		It("should handle GitProvider with invalid credentials", func() {
			gitProviderName := "gitprovider-invalid-test"
			createGitProviderWithURLInNamespace(
				gitProviderName,
				testNs,
				managerRepo.GitSecretInvalid,
				managerRepo.RepoURLHTTP,
			)
			verifyResourceStatus("gitprovider", gitProviderName, testNs, "False", "ConnectionFailed", "")
			_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", gitProviderName, "--ignore-not-found=true")
		})

		It("should handle GitTarget with nonexistent branch pattern", func() {
			gitProviderName := "gitprovider-branch-test"

			// GitProvider should be Ready=True (validates connectivity, not branch existence)
			createGitProviderWithURLInNamespace(
				gitProviderName,
				testNs,
				managerRepo.GitSecretHTTP,
				managerRepo.RepoURLHTTP,
			)
			verifyResourceStatus(
				"gitprovider", gitProviderName, testNs, "True", "Ready", "Repository connectivity validated",
			)

			// GitTarget with branch not matching any pattern should fail
			destName := "dest-invalid-branch"
			createGitTarget(destName, testNs, gitProviderName, "test/invalid", "different-branch")

			By("verifying GitTarget fails branch validation")
			verifyResourceStatus("gittarget", destName, testNs, "False", "ValidationFailed", "BranchNotAllowed")

			cleanupGitTarget(destName, testNs)
			_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", gitProviderName, "--ignore-not-found=true")
		})

		It("should validate GitProvider with SSH authentication", func() {
			gitProviderName := "gitprovider-ssh-test"

			By("🔐 Starting SSH authentication test")
			showControllerLogs("before SSH test")

			By("📋 Checking SSH secret exists")
			secretOutput, err := kubectlRunInNamespace(testNs, "get", "secret", managerRepo.GitSecretSSH, "-o", "yaml")
			if err != nil {
				fmt.Printf("❌ SSH secret not found: %v\n", err)
			} else {
				previewLen := minInt(300, len(secretOutput))
				fmt.Printf(
					"✅ SSH secret exists - showing first %d chars:\n%s...\n",
					previewLen,
					secretOutput[:previewLen],
				)
			}

			createGitProviderWithURLInNamespace(
				gitProviderName,
				testNs,
				managerRepo.GitSecretSSH,
				managerRepo.RepoURLSSH,
			)

			By("🔍 Controller logs after SSH GitProvider creation")
			showControllerLogs("after SSH GitProvider creation")

			verifyResourceStatus(
				"gitprovider", gitProviderName, testNs, "True", "Ready", "Repository connectivity validated",
			)

			By("✅ Final SSH test logs")
			showControllerLogs("SSH test completion")

			_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", gitProviderName, "--ignore-not-found=true")
		})

		It("should handle a normal and healthy GitProvider", Label("smoke"), func() {
			gitProviderName := "gitprovider-normal"
			createGitProviderWithURLInNamespace(
				gitProviderName,
				testNs,
				managerRepo.GitSecretHTTP,
				managerRepo.RepoURLHTTP,
			)
			verifyResourceStatus(
				"gitprovider", gitProviderName, testNs, "True", "Ready", "Repository connectivity validated",
			)
		})

		It("should reconcile a WatchRule CR", func() {
			gitProviderName := "gitprovider-normal"
			watchRuleName := "watchrule-test"

			By("creating a WatchRule that references the working GitProvider")
			destName := watchRuleName + "-dest"
			createGitTarget(destName, testNs, gitProviderName, getBaseFolder(), "main")

			data := struct {
				Name            string
				Namespace       string
				DestinationName string
			}{
				Name:            watchRuleName,
				Namespace:       testNs,
				DestinationName: destName,
			}

			err := applyFromTemplate("test/e2e/templates/watchrule.tmpl", data, testNs)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply WatchRule")

			By("verifying the WatchRule is reconciled")
			verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

			By("cleaning up test resources")
			cleanupWatchRule(watchRuleName, testNs)
			cleanupGitTarget(destName, testNs)
		})

		It("should commit encrypted Secret manifests when WatchRule includes secrets", Label("smoke"), func() {
			gitProviderName := "gitprovider-normal"
			watchRuleName := "watchrule-secret-encryption-test"
			secretName := "test-secret-encryption"

			By("creating WatchRule that includes secrets")
			destName := watchRuleName + "-dest"
			createGitTarget(destName, testNs, gitProviderName, "e2e/secret-encryption-test", "main")

			data := struct {
				Name            string
				Namespace       string
				DestinationName string
			}{
				Name:            watchRuleName,
				Namespace:       testNs,
				DestinationName: destName,
			}

			err := applyFromTemplate("test/e2e/templates/watchrule-secret.tmpl", data, testNs)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply WatchRule")
			verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")
			verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
			verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

			By("creating Secret in watched namespace")
			_, _ = kubectlRunInNamespace(testNs, "delete", "secret", secretName, "--ignore-not-found=true")

			_, err = kubectlRunInNamespace(
				testNs,
				"create",
				"secret",
				"generic",
				secretName,
				"--from-literal=password=do-not-commit",
			)
			Expect(err).NotTo(HaveOccurred(), "Secret creation should succeed")

			By("patching Secret once to avoid informer start race and force an update event")
			_, err = kubectlRunInNamespace(
				testNs,
				"patch",
				"secret",
				secretName,
				"--type=merge",
				"--patch",
				`{"stringData":{"password":"never-commit-this"}}`,
			)
			Expect(err).NotTo(HaveOccurred(), "Secret patch should succeed")

			By("verifying Secret file is committed and does not contain plaintext data")
			verifyEncryptedSecretCommitted := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				pullOutput, pullErr := pullCmd.CombinedOutput()
				if pullErr != nil {
					g.Expect(pullErr).NotTo(HaveOccurred(),
						fmt.Sprintf("Should successfully pull latest changes. Output: %s", string(pullOutput)))
				}

				expectedFile := filepath.Join(managerRepo.CheckoutDir,
					"e2e/secret-encryption-test",
					fmt.Sprintf("v1/secrets/%s/%s.sops.yaml", testNs, secretName))
				content, readErr := os.ReadFile(expectedFile)
				g.Expect(readErr).NotTo(HaveOccurred(), fmt.Sprintf("Secret file must exist at %s", expectedFile))
				g.Expect(string(content)).To(ContainSubstring("sops:"))
				g.Expect(string(content)).NotTo(ContainSubstring("do-not-commit"))

				bootstrapSOPSFile := filepath.Join(managerRepo.CheckoutDir, "e2e/secret-encryption-test", ".sops.yaml")
				bootstrapContent, bootstrapErr := os.ReadFile(bootstrapSOPSFile)
				g.Expect(bootstrapErr).NotTo(
					HaveOccurred(),
					fmt.Sprintf(".sops.yaml must exist at %s", bootstrapSOPSFile),
				)
				ageKey, ageKeyErr := readSOPSAgeKeyFromFile(getE2EAgeKeyPath())
				g.Expect(ageKeyErr).NotTo(HaveOccurred(), "Should read age private key")
				recipient, recipientErr := deriveAgeRecipient(ageKey)
				g.Expect(recipientErr).NotTo(HaveOccurred(), "Should derive age recipient")
				g.Expect(string(bootstrapContent)).To(ContainSubstring(recipient))

				decryptedOutput, decryptErr := decryptWithControllerSOPS(content, ageKey)
				g.Expect(decryptErr).NotTo(HaveOccurred(), "Should decrypt committed secret via controller sops binary")
				g.Expect(decryptedOutput).To(ContainSubstring("bmV2ZXItY29tbWl0LXRoaXM="))
			}
			Eventually(verifyEncryptedSecretCommitted, "30s", "2s").Should(Succeed())

			By("cleaning up test resources")
			_, _ = kubectlRunInNamespace(testNs, "delete", "secret", secretName, "--ignore-not-found=true")
			cleanupWatchRule(watchRuleName, testNs)
			cleanupGitTarget(destName, testNs)
		})

		It("should generate missing SOPS age secret when age.recipients.generateWhenMissing is enabled", func() {
			gitProviderName := "gitprovider-normal"
			watchRuleName := "watchrule-secret-autogen-test"
			secretName := "test-secret-autogen"
			generatedSecretName := "sops-age-key-autogen"

			By("ensuring generated encryption secret does not exist before test")
			_, _ = kubectlRunInNamespace(
				testNs,
				"delete",
				"secret",
				generatedSecretName,
				"--ignore-not-found=true",
			)

			By("creating GitTarget with age recipient auto-generation enabled")
			destName := watchRuleName + "-dest"
			createGitTargetWithEncryptionOptions(
				destName,
				testNs,
				gitProviderName,
				"e2e/secret-autogen-test",
				"main",
				generatedSecretName,
				true,
			)

			data := struct {
				Name            string
				Namespace       string
				DestinationName string
			}{
				Name:            watchRuleName,
				Namespace:       testNs,
				DestinationName: destName,
			}

			err := applyFromTemplate("test/e2e/templates/watchrule-secret.tmpl", data, testNs)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply WatchRule")
			verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

			By("validating generated encryption secret has recipient and warning annotations")
			var generatedAgeKey string
			var generatedRecipient string
			Eventually(func(g Gomega) {
				output, getErr := kubectlRunInNamespace(
					testNs,
					"get",
					"secret",
					generatedSecretName,
					"-o",
					"json",
				)
				g.Expect(getErr).NotTo(HaveOccurred())

				var secretObj map[string]interface{}
				unmarshalErr := json.Unmarshal([]byte(output), &secretObj)
				g.Expect(unmarshalErr).NotTo(HaveOccurred())

				annotations, _, annoErr := unstructured.NestedStringMap(secretObj, "metadata", "annotations")
				g.Expect(annoErr).NotTo(HaveOccurred())
				g.Expect(annotations).To(HaveKey("configbutler.ai/age-recipient"))
				g.Expect(annotations["configbutler.ai/age-recipient"]).To(HavePrefix("age1"))
				g.Expect(annotations).To(HaveKeyWithValue("configbutler.ai/backup-warning", "REMOVE_AFTER_BACKUP"))
				generatedRecipient = annotations["configbutler.ai/age-recipient"]

				secretData, found, keyErr := unstructured.NestedStringMap(secretObj, "data")
				g.Expect(keyErr).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue())
				var sopsAgeKeyB64 string
				for key, value := range secretData {
					if strings.HasSuffix(key, ".agekey") {
						sopsAgeKeyB64 = value
						break
					}
				}
				g.Expect(sopsAgeKeyB64).NotTo(BeEmpty())

				keyBytes, decodeErr := base64.StdEncoding.DecodeString(sopsAgeKeyB64)
				g.Expect(decodeErr).NotTo(HaveOccurred())
				generatedAgeKey = strings.TrimSpace(string(keyBytes))
				g.Expect(generatedAgeKey).To(HavePrefix("AGE-SECRET-KEY-"))
			}, "30s", "2s").Should(Succeed())

			By("waiting for auto-generated target bootstrap file to be present")
			Eventually(func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				pullOutput, pullErr := pullCmd.CombinedOutput()
				if pullErr != nil {
					g.Expect(pullErr).NotTo(HaveOccurred(),
						fmt.Sprintf("Should successfully pull latest changes. Output: %s", string(pullOutput)))
				}

				bootstrapSOPSFile := filepath.Join(managerRepo.CheckoutDir, "e2e/secret-autogen-test", ".sops.yaml")
				bootstrapContent, bootstrapErr := os.ReadFile(bootstrapSOPSFile)
				g.Expect(bootstrapErr).NotTo(HaveOccurred(),
					fmt.Sprintf(".sops.yaml must exist at %s", bootstrapSOPSFile))
				g.Expect(string(bootstrapContent)).To(ContainSubstring(generatedRecipient))
			}, "30s", "2s").Should(Succeed())

			By("creating Secret in watched namespace")
			_, _ = kubectlRunInNamespace(testNs, "delete", "secret", secretName, "--ignore-not-found=true")
			_, err = kubectlRunInNamespace(
				testNs,
				"create",
				"secret",
				"generic",
				secretName,
				"--from-literal=password=do-not-commit",
			)
			Expect(err).NotTo(HaveOccurred(), "Secret creation should succeed")

			By("patching Secret once to avoid informer start race and force an update event")
			_, err = kubectlRunInNamespace(
				testNs,
				"patch",
				"secret",
				secretName,
				"--type=merge",
				"--patch",
				`{"stringData":{"password":"autogen-never-commit-this"}}`,
			)
			Expect(err).NotTo(HaveOccurred(), "Secret patch should succeed")

			By("verifying committed secret is encrypted and decryptable with generated key")
			verifyEncryptedSecretCommitted := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				pullOutput, pullErr := pullCmd.CombinedOutput()
				if pullErr != nil {
					g.Expect(pullErr).NotTo(HaveOccurred(),
						fmt.Sprintf("Should successfully pull latest changes. Output: %s", string(pullOutput)))
				}

				expectedFile := filepath.Join(managerRepo.CheckoutDir,
					"e2e/secret-autogen-test",
					fmt.Sprintf("v1/secrets/%s/%s.sops.yaml", testNs, secretName))
				content, readErr := os.ReadFile(expectedFile)
				g.Expect(readErr).NotTo(HaveOccurred(), fmt.Sprintf("Secret file must exist at %s", expectedFile))
				g.Expect(string(content)).To(ContainSubstring("sops:"))
				g.Expect(string(content)).NotTo(ContainSubstring("autogen-never-commit-this"))

				bootstrapSOPSFile := filepath.Join(managerRepo.CheckoutDir, "e2e/secret-autogen-test", ".sops.yaml")
				bootstrapContent, bootstrapErr := os.ReadFile(bootstrapSOPSFile)
				g.Expect(bootstrapErr).NotTo(
					HaveOccurred(),
					fmt.Sprintf(".sops.yaml must exist at %s", bootstrapSOPSFile),
				)
				g.Expect(string(bootstrapContent)).To(ContainSubstring(generatedRecipient))

				decryptedOutput, decryptErr := decryptWithControllerSOPS(content, generatedAgeKey)
				g.Expect(decryptErr).NotTo(HaveOccurred(), "Should decrypt committed secret via generated age key")
				g.Expect(decryptedOutput).To(ContainSubstring("YXV0b2dlbi1uZXZlci1jb21taXQtdGhpcw=="))
			}
			Eventually(verifyEncryptedSecretCommitted, "30s", "2s").Should(Succeed())

			By("cleaning up test resources")
			_, _ = kubectlRunInNamespace(testNs, "delete", "secret", secretName, "--ignore-not-found=true")
			_, _ = kubectlRunInNamespace(
				testNs,
				"delete",
				"secret",
				generatedSecretName,
				"--ignore-not-found=true",
			)
			cleanupWatchRule(watchRuleName, testNs)
			cleanupGitTarget(destName, testNs)
		})

		It("should create Git commit when ConfigMap is added via WatchRule", Label("smoke"), func() {
			gitProviderName := "gitprovider-normal"
			watchRuleName := "watchrule-configmap-test"
			configMapName := "test-configmap"
			uniqueRepoName := managerRepo.RepoName

			By("creating WatchRule that monitors ConfigMaps")
			destName := watchRuleName + "-dest"
			createGitTarget(destName, testNs, gitProviderName, "e2e/configmap-test", "main")

			data := struct {
				Name            string
				Namespace       string
				DestinationName string
			}{
				Name:            watchRuleName,
				Namespace:       testNs,
				DestinationName: destName,
			}

			err2 := applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", data, testNs)
			Expect(err2).NotTo(HaveOccurred(), "Failed to apply WatchRule")

			By("verifying WatchRule is ready")
			verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
			verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

			// Let any in-flight reconciles from prior specs drain before triggering
			// our event. Without this, a stale WatchRule still in informer cache can
			// double-commit the same ConfigMap under a different commitPath and end
			// up on HEAD, masking our commit.
			time.Sleep(5 * time.Second)

			By("creating test ConfigMap to trigger Git commit")
			configMapData := struct {
				Name      string
				Namespace string
			}{
				Name:      configMapName,
				Namespace: testNs,
			}

			err3 := applyFromTemplate(
				"test/e2e/templates/manager/configmap.tmpl",
				configMapData,
				testNs,
				"--as=jane@acme.com", // Important: we validate later if the user is included in the git commit!
			)
			Expect(err3).NotTo(HaveOccurred(), "Failed to apply ConfigMap")

			By("waiting for controller reconciliation of ConfigMap event")
			verifyReconciliationLogs := func(g Gomega) {
				output, err := controllerLogs(500)
				g.Expect(err).NotTo(HaveOccurred())

				// Check for git commit operation in logs
				g.Expect(output).To(ContainSubstring("git commit"),
					"Should see git commit operation in controller logs")
			}
			Eventually(verifyReconciliationLogs).Should(Succeed())

			By("verifying ConfigMap YAML file exists in Gitea repository")
			verifyGitCommit := func(g Gomega) {
				// Use the pre-checked out repository directory
				By("using pre-checked out repository for verification")

				// Pull latest changes from the remote repository
				By("pulling latest changes from remote repository")
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				// Don't use utils.Run() here because it overwrites cmd.Dir with the project directory
				pullOutput, pullErr := pullCmd.CombinedOutput()
				if pullErr != nil {
					g.Expect(pullErr).NotTo(HaveOccurred(),
						fmt.Sprintf("Should successfully pull latest changes. Output: %s", string(pullOutput)))
				}

				// Check for the expected ConfigMap file (new API-aligned path)
				expectedFile := filepath.Join(managerRepo.CheckoutDir,
					"e2e/configmap-test",
					fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, configMapName))
				fileInfo, statErr := os.Stat(expectedFile)
				g.Expect(statErr).NotTo(HaveOccurred(), fmt.Sprintf("ConfigMap file should exist at %s", expectedFile))
				g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "ConfigMap file should not be empty")

				expectedRepoPath := path.Join(
					"e2e/configmap-test",
					fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, configMapName),
				)
				assertLatestCommitForPathTouchesOnlyWithOptional(
					g,
					managerRepo.CheckoutDir,
					expectedRepoPath,
					[]string{expectedRepoPath},
					[]string{
						path.Join("e2e/configmap-test", "README.md"),
						path.Join("e2e/configmap-test", ".sops.yaml"),
					},
				)
				assertLatestCommitTouchesNoNamespaces(
					g,
					managerRepo.CheckoutDir,
					"e2e/configmap-test/v1/configmaps",
					[]string{
						"gitops-reverser",
						"flux-system",
						"kube-system",
						"tilt-playground",
					},
				)

				// Verify file content contains expected ConfigMap data
				content, readErr := os.ReadFile(expectedFile)
				g.Expect(readErr).NotTo(HaveOccurred())
				g.Expect(string(content)).To(ContainSubstring("test-key: test-value"),
					"ConfigMap file should contain expected data")

				// Verify latest commit message contains operation, resource path
				gitLogCmd := exec.Command("git", "log", "-1", "--pretty=%B")
				gitLogCmd.Dir = managerRepo.CheckoutDir
				commitMsg, commitErr := gitLogCmd.CombinedOutput()

				if commitErr != nil {
					g.Expect(commitErr).NotTo(HaveOccurred(),
						fmt.Sprintf("Should read latest commit message. Output: %s", string(commitMsg)))
				}

				msg := string(commitMsg)
				g.Expect(msg).To(ContainSubstring("[CREATE]"),
					"Latest commit message should include operation [CREATE]")
				g.Expect(msg).To(ContainSubstring(fmt.Sprintf("v1/configmaps/%s", configMapName)),
					"Latest commit message should include resource path")

				gitLogCmd = exec.Command("git", "log", "-1", "--pretty=%an")
				gitLogCmd.Dir = managerRepo.CheckoutDir
				authorMsg, commitErr := gitLogCmd.CombinedOutput()
				if commitErr != nil {
					g.Expect(commitErr).NotTo(HaveOccurred(),
						fmt.Sprintf("Should read latest commit author. Output: %s", string(authorMsg)))
				}

				author := string(authorMsg)
				g.Expect(author).To(ContainSubstring("jane@acme.com"))
			}
			Eventually(verifyGitCommit).Should(Succeed())

			By("cleaning up test resources")
			_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", configMapName, "--ignore-not-found=true")
			cleanupWatchRule(watchRuleName, testNs)
			cleanupGitTarget(destName, testNs)

			By("✅ ConfigMap to Git commit E2E test passed - verified actual file creation and commit")
			fmt.Printf("✅ ConfigMap '%s' successfully triggered Git commit with YAML file in repo '%s'\n",
				configMapName, uniqueRepoName)
		})

		It("should delete Git file when ConfigMap is deleted via WatchRule", Label("smoke"), func() {
			gitProviderName := "gitprovider-normal"
			watchRuleName := "watchrule-delete-test"
			configMapName := "test-configmap-to-delete"
			uniqueRepoName := managerRepo.RepoName

			By("creating WatchRule that monitors ConfigMaps")
			destName := watchRuleName + "-dest"
			createGitTarget(destName, testNs, gitProviderName, "e2e/delete-test", "main")
			data := struct {
				Name            string
				Namespace       string
				DestinationName string
			}{
				Name:            watchRuleName,
				Namespace:       testNs,
				DestinationName: destName,
			}

			err2 := applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", data, testNs)
			Expect(err2).NotTo(HaveOccurred(), "Failed to apply WatchRule")

			By("verifying WatchRule is ready")
			verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
			verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

			By("creating test ConfigMap to trigger Git commit")
			configMapData := struct {
				Name      string
				Namespace string
			}{
				Name:      configMapName,
				Namespace: testNs,
			}

			err3 := applyFromTemplate("test/e2e/templates/manager/configmap.tmpl", configMapData, testNs)
			Expect(err3).NotTo(HaveOccurred(), "Failed to apply ConfigMap")

			By("waiting for ConfigMap file to appear in Git repository")
			verifyFileCreated := func(g Gomega) {
				// Pull latest changes from the remote repository
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				pullOutput, pullErr := pullCmd.CombinedOutput()
				if pullErr != nil {
					g.Expect(pullErr).NotTo(HaveOccurred(),
						fmt.Sprintf("Should successfully pull latest changes. Output: %s", string(pullOutput)))
				}

				// Check for the expected ConfigMap file (new API-aligned path)
				expectedFile := filepath.Join(managerRepo.CheckoutDir,
					"e2e/delete-test",
					fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, configMapName))
				fileInfo, statErr := os.Stat(expectedFile)
				g.Expect(statErr).NotTo(HaveOccurred(), fmt.Sprintf("ConfigMap file should exist at %s", expectedFile))
				g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "ConfigMap file should not be empty")
			}
			Eventually(verifyFileCreated).Should(Succeed())

			By("deleting the ConfigMap to trigger DELETE operation")
			_, err := kubectlRunInNamespace(testNs, "delete", "configmap", configMapName)
			Expect(err).NotTo(HaveOccurred(), "ConfigMap deletion should succeed")

			By("verifying ConfigMap file is deleted from Git repository")
			verifyFileDeleted := func(g Gomega) {
				// Pull latest changes from the remote repository
				By("pulling latest changes after deletion")
				pullLatestRepoState(g, managerRepo.CheckoutDir)

				// Check that the ConfigMap file no longer exists (new API-aligned path)
				expectedRelativePath := path.Join(
					"e2e/delete-test",
					fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, configMapName),
				)
				expectedFile := filepath.Join(managerRepo.CheckoutDir, expectedRelativePath)
				_, statErr := os.Stat(expectedFile)
				g.Expect(statErr).To(HaveOccurred(), fmt.Sprintf("ConfigMap file should NOT exist at %s", expectedFile))
				g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "Error should be 'file does not exist'")

				assertLatestCommitTouchesOnly(g, managerRepo.CheckoutDir, []string{expectedRelativePath})
				assertLatestCommitTouchesNoNamespaces(
					g,
					managerRepo.CheckoutDir,
					"e2e/delete-test/v1/configmaps",
					[]string{
						"gitops-reverser",
						"flux-system",
						"kube-system",
						"tilt-playground",
					},
				)

				// Verify git log shows DELETE commit
				By("verifying git log shows DELETE operation")
				gitLogCmd := exec.Command("git", "log", "--oneline", "-n", "5")
				gitLogCmd.Dir = managerRepo.CheckoutDir
				logOutput, logErr := gitLogCmd.CombinedOutput()
				g.Expect(logErr).NotTo(HaveOccurred(), "Should be able to read git log")
				g.Expect(string(logOutput)).To(ContainSubstring("DELETE"),
					"Git log should contain DELETE operation")
			}
			Eventually(verifyFileDeleted).Should(Succeed())

			By("cleaning up test resources")
			cleanupWatchRule(watchRuleName, testNs)
			cleanupGitTarget(destName, testNs)

			By("✅ ConfigMap deletion E2E test passed - verified file removal from Git")
			fmt.Printf("✅ ConfigMap '%s' deletion successfully triggered Git commit removing file from repo '%s'\n",
				configMapName, uniqueRepoName)
		})

		It("should create Git commit when IceCreamOrder CRD is installed via ClusterWatchRule", Label("smoke"), func() {
			gitProviderName := "gitprovider-normal"
			clusterWatchRuleName := "clusterwatchrule-crd-install"
			crdName := "icecreamorders.shop.example.com"

			By("creating ClusterWatchRule with Cluster scope for CRDs")
			destName := clusterWatchRuleName + "-dest"
			createGitTarget(destName, testNs, gitProviderName, "e2e/crd-install-test", "main")

			clusterWatchRuleData := struct {
				Name            string
				DestinationName string
				Namespace       string
			}{
				Name:            clusterWatchRuleName,
				DestinationName: destName,
				Namespace:       testNs,
			}

			err := applyFromTemplate("test/e2e/templates/manager/clusterwatchrule-crd.tmpl", clusterWatchRuleData, "")
			Expect(err).NotTo(HaveOccurred(), "Failed to apply ClusterWatchRule for CRDs")

			By("verifying ClusterWatchRule is ready")
			verifyResourceStatus("clusterwatchrule", clusterWatchRuleName, "", "True", "Ready", "")
			verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

			By("installing the IceCreamOrder CRD to trigger Git commit")
			_, err = kubectlRun("apply", "-f", "test/e2e/templates/icecreamorder-crd.yaml")
			Expect(err).NotTo(HaveOccurred(), "Failed to install CRD")

			By("waiting for CRD to be established")
			verifyCRDEstablished := func(g Gomega) {
				output, err := kubectlRun(
					"get",
					"crd",
					crdName,
					"-o",
					"jsonpath={.status.conditions[?(@.type=='Established')].status}",
				)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}
			Eventually(verifyCRDEstablished).Should(Succeed())

			By("verifying CRD YAML file exists in Git repository (NO namespace in path - cluster resource)")
			verifyGitCommit := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				pullOutput, pullErr := pullCmd.CombinedOutput()
				if pullErr != nil {
					g.Expect(pullErr).NotTo(HaveOccurred(),
						fmt.Sprintf("Should successfully pull latest changes. Output: %s", string(pullOutput)))
				}

				// CRDs are cluster-scoped, so path should NOT include namespace
				expectedFile := filepath.Join(managerRepo.CheckoutDir,
					"e2e/crd-install-test",
					"apiextensions.k8s.io/v1/customresourcedefinitions/icecreamorders.shop.example.com.yaml")
				fileInfo, statErr := os.Stat(expectedFile)
				g.Expect(statErr).NotTo(HaveOccurred(), fmt.Sprintf("CRD file should exist at %s", expectedFile))
				g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "CRD file should not be empty")

				// Verify file content
				content, readErr := os.ReadFile(expectedFile)
				g.Expect(readErr).NotTo(HaveOccurred())
				g.Expect(string(content)).To(ContainSubstring("kind: CustomResourceDefinition"),
					"File should contain CRD kind")
				g.Expect(string(content)).To(ContainSubstring("name: icecreamorders.shop.example.com"),
					"File should contain CRD name")
			}
			Eventually(verifyGitCommit).
				WithTimeout(60 * time.Second).
				WithPolling(2 * time.Second).
				Should(Succeed())

			By("cleaning up test resources")
			cleanupClusterWatchRule(clusterWatchRuleName)
			cleanupGitTarget(destName, testNs)
			// Keep CRD installed for subsequent tests

			By("✅ CRD installation via ClusterWatchRule E2E test passed")
		})

		It("should create Git commit when IceCreamOrder is added via WatchRule", func() {
			gitProviderName := "gitprovider-normal"
			watchRuleName := "watchrule-icecream-orders"

			By("installing the IceCreamOrder CRD first (needed for custom resource tests)")
			_, err := kubectlRun("apply", "-f", "test/e2e/templates/icecreamorder-crd.yaml")
			Expect(err).NotTo(HaveOccurred(), "Failed to install sample CRD")

			By("waiting for CRD to be established")
			verifyCRDEstablished := func(g Gomega) {
				output, err := kubectlRun(
					"get",
					"crd",
					"icecreamorders.shop.example.com",
					"-o",
					"jsonpath={.status.conditions[?(@.type=='Established')].status}",
				)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}
			Eventually(verifyCRDEstablished, 30*time.Second, time.Second).Should(Succeed())

			crdInstanceName := "alices-order"
			uniqueRepoName := managerRepo.RepoName

			By("creating WatchRule that monitors IceCreamOrder resources")
			destName := watchRuleName + "-dest"
			createGitTarget(destName, testNs, gitProviderName, "e2e/icecream-test", "main")

			data := struct {
				Name            string
				Namespace       string
				DestinationName string
			}{
				Name:            watchRuleName,
				Namespace:       testNs,
				DestinationName: destName,
			}

			err2 := applyFromTemplate("test/e2e/templates/watchrule-crd.tmpl", data, testNs)
			Expect(err2).NotTo(HaveOccurred(), "Failed to apply WatchRule for CRDs")

			By("verifying WatchRule is ready")
			verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
			verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

			By("creating CR with labels and annotations to trigger Git commit")
			crdInstanceData := struct {
				Name         string
				Namespace    string
				Labels       map[string]string
				Annotations  map[string]string
				CustomerName string
				Container    string
				Scoops       []struct {
					Flavor   string
					Quantity int
				}
				Toppings []string
			}{
				Name:      crdInstanceName,
				Namespace: testNs,
				Labels: map[string]string{
					"environment": "test",
					"team":        "engineering",
				},
				Annotations: map[string]string{
					"description": "Alice's favorite ice cream",
					"priority":    "high",
					"kubectl.kubernetes.io/last-applied-configuration": "should-be-filtered",
					"deployment.kubernetes.io/revision":                "should-also-be-filtered",
				},
				CustomerName: "Alice",
				Container:    "Cone",
				Scoops: []struct {
					Flavor   string
					Quantity int
				}{
					{Flavor: "Vanilla", Quantity: 2},
					{Flavor: "Chocolate", Quantity: 1},
				},
				Toppings: []string{"Sprinkles", "HotFudge"},
			}

			err3 := applyFromTemplate("test/e2e/templates/icecreamorder-instance.tmpl", crdInstanceData, testNs)
			Expect(err3).NotTo(HaveOccurred(), "Failed to apply CRD instance")

			By("waiting for controller reconciliation of CRD instance event")
			verifyReconciliationLogs := func(g Gomega) {
				output, err := kubectlRunInNamespace(
					namespace,
					"logs",
					"-l",
					"control-plane=gitops-reverser",
					"--tail=500",
					"--prefix=true",
				)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("git commit"),
					"Should see git commit operation in logs")
			}
			Eventually(verifyReconciliationLogs, 45*time.Second, 2*time.Second).Should(Succeed())

			By("verifying CRD instance YAML file exists in Gitea repository")
			verifyGitCommit := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				pullOutput, pullErr := pullCmd.CombinedOutput()
				if pullErr != nil {
					g.Expect(pullErr).NotTo(HaveOccurred(),
						fmt.Sprintf("Should successfully pull latest changes. Output: %s", string(pullOutput)))
				}

				expectedFile := filepath.Join(managerRepo.CheckoutDir,
					"e2e/icecream-test",
					fmt.Sprintf("shop.example.com/v1/icecreamorders/%s/%s.yaml", testNs, crdInstanceName))
				fileInfo, statErr := os.Stat(expectedFile)
				g.Expect(statErr).
					NotTo(HaveOccurred(), fmt.Sprintf("CRD instance file should exist at %s", expectedFile))
				g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "CRD instance file should not be empty")

				content, readErr := os.ReadFile(expectedFile)
				g.Expect(readErr).NotTo(HaveOccurred())
				contentStr := string(content)
				g.Expect(contentStr).To(ContainSubstring("kind: IceCreamOrder"),
					"CRD instance file should contain IceCreamOrder kind")
				g.Expect(contentStr).To(ContainSubstring("customerName: Alice"),
					"CRD instance file should contain customer name")
				g.Expect(contentStr).To(ContainSubstring("container: Cone"),
					"CRD instance file should contain container type")
				g.Expect(contentStr).To(ContainSubstring("flavor: Vanilla"),
					"CRD instance file should contain ice cream flavors")

				// Verify labels are present
				g.Expect(contentStr).To(ContainSubstring("environment: test"),
					"CRD instance file should contain environment label")
				g.Expect(contentStr).To(ContainSubstring("team: engineering"),
					"CRD instance file should contain team label")

				// Verify user annotations are present
				g.Expect(contentStr).To(ContainSubstring("description: Alice's favorite ice cream"),
					"CRD instance file should contain description annotation")
				g.Expect(contentStr).To(ContainSubstring("priority: high"),
					"CRD instance file should contain priority annotation")

				// Verify filtered annotations are NOT present
				g.Expect(contentStr).NotTo(ContainSubstring("kubectl.kubernetes.io/last-applied-configuration"),
					"CRD instance file should NOT contain kubectl annotation")
				g.Expect(contentStr).NotTo(ContainSubstring("deployment.kubernetes.io/revision"),
					"CRD instance file should NOT contain deployment annotation")

				// Verify status field is NOT present in Git
				g.Expect(contentStr).NotTo(ContainSubstring("status:"),
					"CRD instance file should NOT contain status field")
			}
			Eventually(verifyGitCommit).
				WithTimeout(60 * time.Second).
				WithPolling(2 * time.Second).
				Should(Succeed())

			By("verifying the IceCreamOrder still exists before status update")
			verifyCRDInstanceExists := func(g Gomega) {
				_, err := kubectlRunInNamespace(testNs, "get", "icecreamorder", crdInstanceName)
				g.Expect(err).NotTo(HaveOccurred(), "IceCreamOrder should exist before status patch")
			}
			Eventually(verifyCRDInstanceExists, 15*time.Second, time.Second).Should(Succeed())

			By("applying status update to the IceCreamOrder CR")
			statusPatch := `{"status":{"phase":"Preparing","cost":12.5,"message":"Queued for pickup"}}`
			statusOutput, statusErr := kubectlRunInNamespace(
				testNs,
				"patch",
				"icecreamorder",
				crdInstanceName,
				"--type=merge",
				"--subresource=status",
				"-p",
				statusPatch,
			)
			Expect(statusErr).NotTo(HaveOccurred(), "Status subresource patch should succeed")
			By(fmt.Sprintf("status patched successfully: %s", statusOutput))

			By("getting current git commit hash")
			gitRevCmd := exec.Command("git", "rev-parse", "HEAD")
			gitRevCmd.Dir = managerRepo.CheckoutDir
			beforeStatusCommit, _ := gitRevCmd.Output()

			By("waiting to ensure no new commit is created from status update")
			time.Sleep(10 * time.Second)

			By("verifying no new commit was created and status is not in Git")
			verifyStatusNotCommitted := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				_, _ = pullCmd.CombinedOutput()

				// Check that commit hash hasn't changed
				gitRevCmd := exec.Command("git", "rev-parse", "HEAD")
				gitRevCmd.Dir = managerRepo.CheckoutDir
				afterStatusCommit, err := gitRevCmd.Output()
				g.Expect(err).NotTo(HaveOccurred())

				By(fmt.Sprintf("Commit before status: %s", string(beforeStatusCommit)))
				By(fmt.Sprintf("Commit after status:  %s", string(afterStatusCommit)))

				// Read the file again to ensure status is still not present
				expectedFile := filepath.Join(managerRepo.CheckoutDir,
					"e2e/icecream-test",
					fmt.Sprintf("shop.example.com/v1/icecreamorders/%s/%s.yaml", testNs, crdInstanceName))
				content, readErr := os.ReadFile(expectedFile)
				g.Expect(readErr).NotTo(HaveOccurred())
				g.Expect(string(content)).NotTo(ContainSubstring("status:"),
					"CRD instance file should still NOT contain status field after status update")
				g.Expect(string(content)).NotTo(ContainSubstring("phase:"),
					"CRD instance file should NOT contain status phase content")
				g.Expect(string(content)).NotTo(ContainSubstring("Queued for pickup"),
					"CRD instance file should NOT contain status content")
			}
			Eventually(verifyStatusNotCommitted).Should(Succeed())

			By("✅ Status update verified - no Git commit created and status not in file")

			By("cleaning up IceCreamOrder instance")
			_, _ = kubectlRunInNamespace(testNs, "delete", "icecreamorder", crdInstanceName)

			By("Note: GitTarget, WatchRule, GitProvider, and CRD kept for subsequent tests")

			By("✅ IceCreamOrder to Git commit E2E test passed")
			fmt.Printf("✅ IceCreamOrder '%s' successfully triggered Git commit in repo '%s'\n",
				crdInstanceName, uniqueRepoName)
		})

		It("should update Git file when IceCreamOrder is modified via WatchRule", func() {
			crdInstanceName := "bobs-order"
			uniqueRepoName := managerRepo.RepoName

			By("creating initial IceCreamOrder instance")
			crdInstanceData := struct {
				Name         string
				Namespace    string
				Labels       map[string]string
				Annotations  map[string]string
				CustomerName string
				Container    string
				Scoops       []struct {
					Flavor   string
					Quantity int
				}
				Toppings []string
			}{
				Name:         crdInstanceName,
				Namespace:    testNs,
				Labels:       nil,
				Annotations:  nil,
				CustomerName: "Bob",
				Container:    "Cup",
				Scoops: []struct {
					Flavor   string
					Quantity int
				}{
					{Flavor: "Strawberry", Quantity: 1},
				},
				Toppings: []string{"WhippedCream"},
			}

			err := applyFromTemplate("test/e2e/templates/icecreamorder-instance.tmpl", crdInstanceData, testNs)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply initial CRD instance")

			By("waiting for initial CRD instance file to appear in Git")
			verifyInitialFile := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				_, _ = pullCmd.CombinedOutput()

				expectedFile := filepath.Join(managerRepo.CheckoutDir,
					"e2e/icecream-test",
					fmt.Sprintf("shop.example.com/v1/icecreamorders/%s/%s.yaml", testNs, crdInstanceName))
				content, readErr := os.ReadFile(expectedFile)
				g.Expect(readErr).NotTo(HaveOccurred())
				g.Expect(string(content)).To(ContainSubstring("customerName: Bob"))
				g.Expect(string(content)).To(ContainSubstring("flavor: Strawberry"))
			}
			Eventually(verifyInitialFile).Should(Succeed())

			By("updating CRD instance with new values")
			updatedCRDData := struct {
				Name         string
				Namespace    string
				Labels       map[string]string
				Annotations  map[string]string
				CustomerName string
				Container    string
				Scoops       []struct {
					Flavor   string
					Quantity int
				}
				Toppings []string
			}{
				Name:         crdInstanceName,
				Namespace:    testNs,
				Labels:       nil,
				Annotations:  nil,
				CustomerName: "Bob",
				Container:    "WaffleBowl",
				Scoops: []struct {
					Flavor   string
					Quantity int
				}{
					{Flavor: "RockyRoad", Quantity: 3},
					{Flavor: "MintChip", Quantity: 2},
				},
				Toppings: []string{"HotFudge", "Caramel", "Sprinkles"},
			}

			err = applyFromTemplate("test/e2e/templates/icecreamorder-instance.tmpl", updatedCRDData, testNs)
			Expect(err).NotTo(HaveOccurred(), "Failed to update CRD instance")

			By("verifying updated CRD instance content in Git")
			verifyUpdatedFile := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				_, _ = pullCmd.CombinedOutput()

				expectedFile := filepath.Join(managerRepo.CheckoutDir,
					"e2e/icecream-test",
					fmt.Sprintf("shop.example.com/v1/icecreamorders/%s/%s.yaml", testNs, crdInstanceName))
				content, readErr := os.ReadFile(expectedFile)
				g.Expect(readErr).NotTo(HaveOccurred())
				g.Expect(string(content)).To(ContainSubstring("container: WaffleBowl"),
					"Updated file should contain new container type")
				g.Expect(string(content)).To(ContainSubstring("flavor: RockyRoad"),
					"Updated file should contain new flavor")
				g.Expect(string(content)).To(ContainSubstring("quantity: 3"),
					"Updated file should contain new quantity")
			}
			Eventually(verifyUpdatedFile).Should(Succeed())

			By("cleaning up IceCreamOrder instance")
			_, _ = kubectlRunInNamespace(testNs, "delete", "icecreamorder", crdInstanceName)

			By("Note: GitTarget, WatchRule, GitProvider, and CRD kept for subsequent tests")

			By("✅ IceCreamOrder update E2E test passed")
			fmt.Printf("✅ IceCreamOrder '%s' update successfully reflected in Git repo '%s'\n",
				crdInstanceName, uniqueRepoName)
		})

		It("should delete Git file when IceCreamOrder is deleted via WatchRule", func() {
			crdInstanceName := "charlies-order"
			uniqueRepoName := managerRepo.RepoName

			By("creating IceCreamOrder instance")
			crdInstanceData := struct {
				Name         string
				Namespace    string
				Labels       map[string]string
				Annotations  map[string]string
				CustomerName string
				Container    string
				Scoops       []struct {
					Flavor   string
					Quantity int
				}
				Toppings []string
			}{
				Name:         crdInstanceName,
				Namespace:    testNs,
				Labels:       nil,
				Annotations:  nil,
				CustomerName: "Charlie",
				Container:    "Cone",
				Scoops: []struct {
					Flavor   string
					Quantity int
				}{
					{Flavor: "Chocolate", Quantity: 2},
				},
				Toppings: []string{"Sprinkles"},
			}

			err := applyFromTemplate("test/e2e/templates/icecreamorder-instance.tmpl", crdInstanceData, testNs)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply CR")

			By("waiting for CR file to appear in Git repository")
			verifyFileCreated := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				_, _ = pullCmd.CombinedOutput()

				expectedFile := filepath.Join(managerRepo.CheckoutDir,
					"e2e/icecream-test",
					fmt.Sprintf("shop.example.com/v1/icecreamorders/%s/%s.yaml", testNs, crdInstanceName))
				fileInfo, statErr := os.Stat(expectedFile)
				g.Expect(statErr).
					NotTo(HaveOccurred(), fmt.Sprintf("CRD instance file should exist at %s", expectedFile))
				g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "CRD instance file should not be empty")
			}
			Eventually(verifyFileCreated).Should(Succeed())

			By("deleting the CR to trigger DELETE operation")
			_, err = kubectlRunInNamespace(testNs, "delete", "icecreamorder", crdInstanceName)
			Expect(err).NotTo(HaveOccurred(), "CRD instance deletion should succeed")

			By("verifying CRD instance file is deleted from Git repository")
			verifyFileDeleted := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				_, _ = pullCmd.CombinedOutput()

				expectedFile := filepath.Join(managerRepo.CheckoutDir,
					"e2e/icecream-test",
					fmt.Sprintf("shop.example.com/v1/icecreamorders/%s/%s.yaml", testNs, crdInstanceName))
				_, statErr := os.Stat(expectedFile)
				g.Expect(statErr).
					To(HaveOccurred(), fmt.Sprintf("CRD instance file should NOT exist at %s", expectedFile))
				g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "Error should be 'file does not exist'")

				By("verifying git log shows DELETE commit")
				gitLogCmd := exec.Command("git", "log", "--oneline", "-n", "5")
				gitLogCmd.Dir = managerRepo.CheckoutDir
				logOutput, logErr := gitLogCmd.CombinedOutput()
				g.Expect(logErr).NotTo(HaveOccurred(), "Should be able to read git log")
				g.Expect(string(logOutput)).To(ContainSubstring("DELETE"),
					"Git log should contain DELETE operation")
			}
			Eventually(verifyFileDeleted).Should(Succeed())

			By("✅ IceCreamOrder deletion E2E test passed")
			fmt.Printf("✅ IceCreamOrder '%s' deletion successfully removed file from Git repo '%s'\n",
				crdInstanceName, uniqueRepoName)
		})

		It("should delete Git file when IceCreamOrder CRD is deleted via ClusterWatchRule", func() {
			gitProviderName := "gitprovider-normal"
			clusterWatchRuleName := "clusterwatchrule-crd-delete"
			crdName := "icecreamorders.shop.example.com"

			By("creating ClusterWatchRule with Cluster scope for CRDs")
			destName := clusterWatchRuleName + "-dest"
			createGitTarget(destName, testNs, gitProviderName, "e2e/crd-delete-test", "main")
			verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

			clusterWatchRuleData := struct {
				Name            string
				DestinationName string
				Namespace       string
			}{
				Name:            clusterWatchRuleName,
				DestinationName: destName,
				Namespace:       testNs,
			}

			err := applyFromTemplate("test/e2e/templates/manager/clusterwatchrule-crd.tmpl", clusterWatchRuleData, "")
			Expect(err).NotTo(HaveOccurred(), "Failed to apply ClusterWatchRule for CRDs")

			By("verifying ClusterWatchRule is ready")

			verifyResourceStatus("clusterwatchrule", clusterWatchRuleName, "", "True", "Ready", "")

			By("verifying CRD file exists in Git before deletion")
			verifyFileExists := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				_, _ = pullCmd.CombinedOutput()

				expectedFile := filepath.Join(managerRepo.CheckoutDir,
					"e2e/crd-delete-test",
					"apiextensions.k8s.io/v1/customresourcedefinitions/icecreamorders.shop.example.com.yaml")
				_, statErr := os.Stat(expectedFile)
				g.Expect(statErr).NotTo(HaveOccurred(), "CRD file should exist before deletion")
			}
			Eventually(verifyFileExists).Should(Succeed())

			By("deleting the CRD to trigger DELETE operation")
			_, deleteErr := kubectlRun("delete", "crd", crdName)
			Expect(deleteErr).NotTo(HaveOccurred(), "CRD deletion should succeed")
			By("verifying CRD file is deleted from Git repository")
			verifyFileDeleted := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				_, _ = pullCmd.CombinedOutput()

				expectedFile := filepath.Join(managerRepo.CheckoutDir,
					"e2e/crd-delete-test",
					"apiextensions.k8s.io/v1/customresourcedefinitions/icecreamorders.shop.example.com.yaml")
				_, statErr := os.Stat(expectedFile)
				g.Expect(statErr).To(HaveOccurred(), "CRD file should NOT exist after deletion")
				g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "Error should be 'file does not exist'")

				// Verify git log shows DELETE commit
				gitLogCmd := exec.Command("git", "log", "--oneline", "-n", "5")
				gitLogCmd.Dir = managerRepo.CheckoutDir
				logOutput, logErr := gitLogCmd.CombinedOutput()
				g.Expect(logErr).NotTo(HaveOccurred(), "Should be able to read git log")
				g.Expect(string(logOutput)).To(ContainSubstring("DELETE"),
					"Git log should contain DELETE operation")
			}
			Eventually(verifyFileDeleted, "60s", "1s").Should(Succeed())

			By("verifying the deleted CRD file does not reappear after terminating updates")
			Consistently(func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = managerRepo.CheckoutDir
				_, _ = pullCmd.CombinedOutput()

				expectedFile := filepath.Join(managerRepo.CheckoutDir,
					"e2e/crd-delete-test",
					"apiextensions.k8s.io/v1/customresourcedefinitions/icecreamorders.shop.example.com.yaml")
				_, statErr := os.Stat(expectedFile)
				g.Expect(statErr).To(HaveOccurred(), "CRD file must stay deleted after CRD termination updates")
				g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "CRD file must not reappear in Git")
			}, "15s", "1s").Should(Succeed())

			By("cleaning up test resources")
			cleanupClusterWatchRule(clusterWatchRuleName)
			cleanupGitTarget(destName, testNs)

			By("✅ CRD deletion via ClusterWatchRule E2E test passed")
		})

		AfterAll(func() {
			By("cleaning up shared test resources")

			// Clean up WatchRule from IceCreamOrder tests
			cleanupWatchRule("watchrule-icecream-orders", testNs)

			// Clean up GitTarget from IceCreamOrder tests
			cleanupGitTarget("watchrule-icecream-orders-dest", testNs)

			// Clean up GitProvider from IceCreamOrder tests
			_, _ = kubectlRunInNamespace(
				testNs, "delete", "gitprovider", "gitprovider-normal", "--ignore-not-found=true",
			)

			// Clean up IceCreamOrder CRD
			_, _ = kubectlRun(
				"delete",
				"crd",
				"icecreamorders.shop.example.com",
				"--ignore-not-found=true",
			)
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})
})

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
	Eventually(verifyStatus).Should(Succeed())
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

// minInt returns the minimum of two integers.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
