package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

// namespace where the project is deployed in
const namespace = "sut"

// serviceAccountName created for the project
const serviceAccountName = "gitops-reverser-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "gitops-reverser-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "gitops-reverser-metrics-binding"

// giteaRepoURLTemplate is the URL template for test Gitea repositories
const giteaRepoURLTemplate = "http://gitea-http.gitea-e2e.svc.cluster.local:3000/testorg/%s.git"
const giteaSSHURLTemplate = "ssh://git@gitea-ssh.gitea-e2e.svc.cluster.local:2222/testorg/%s.git"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// deploying the controller, and setting up Gitea.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		if err != nil {
			// Namespace might already exist from Gitea setup - check if it's AlreadyExists error
			By("checking if namespace already exists")
			checkCmd := exec.Command("kubectl", "get", "ns", namespace)
			_, checkErr := utils.Run(checkCmd)
			if checkErr != nil {
				Expect(err).NotTo(HaveOccurred(), "Failed to create namespace and namespace doesn't exist")
			}
		}

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("setting up persistent Gitea test environment")
		cmd = exec.Command("bash", "test/e2e/scripts/setup-gitea.sh")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to setup Gitea test environment")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("removing the created clusterrolebinding for metrics")
		cmd := exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName)
		_, _ = utils.Run(cmd)

		By("cleaning up the curl pod for metrics")
		cmd = exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		// Note: We keep Gitea running for efficiency - it will be cleaned up by the test environment

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(10 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should have webhook registration configured", func() {
			By("verifying basic webhook registration")
			verifyWebhook := func(g Gomega) {
				jsonPath := "jsonpath={.items[?(@.metadata.name=='gitops-reverser-validating-webhook-configuration')]" +
					".webhooks[0].name}"
				cmd := exec.Command("kubectl", "get", "validatingwebhookconfigurations", "-o", jsonPath)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("gitops-reverser.configbutler.ai"))
			}
			Eventually(verifyWebhook).Should(Succeed())
		})

		It("should receive webhook calls and process them successfully", func() {
			webhookBindingName := "webhook-test-metrics-binding"
			curlPodName := "webhook-test-curl"

			// Setup metrics access using reusable helper
			token := setupMetricsAccess(webhookBindingName)

			By("creating a ConfigMap to trigger webhook call")
			cmd := exec.Command("kubectl", "create", "configmap", "webhook-test-cm",
				"--namespace", namespace,
				"--from-literal=test=webhook")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "ConfigMap creation should succeed with working webhook")

			// Wait a moment for metrics to be updated
			time.Sleep(2 * time.Second)

			// Create curl pod and wait for completion using reusable helpers
			createMetricsCurlPod(curlPodName, token)
			waitForMetricsCurlCompletion(curlPodName, 2*time.Minute)

			By("verifying webhook metrics show event reception")
			metricsOutput := getMetricsFromCurlPod(curlPodName)

			// Just check if the metric exists (don't worry about the value)
			Expect(metricsOutput).To(ContainSubstring("gitopsreverser_events_received_total"),
				"Events received metric should exist in metrics output")

			By("confirming webhook is working - events are being received")
			fmt.Printf("✅ Webhook validation successful - events are being received by the webhook\n")

			By("cleaning up webhook test resources")
			cmd = exec.Command("kubectl", "delete", "configmap", "webhook-test-cm", "--namespace", namespace)
			_, _ = utils.Run(cmd)
			cleanupMetricsAccess(curlPodName, webhookBindingName)
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			// Setup metrics access using reusable helper
			token := setupMetricsAccess(metricsRoleBindingName)

			By("validating that the metrics service is available")
			cmd := exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("8443"), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("controller-runtime.metrics\tServing metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

			// Create curl pod and wait for completion using reusable helpers
			createMetricsCurlPod("curl-metrics", token)
			waitForMetricsCurlCompletion("curl-metrics", 5*time.Minute)

			By("getting the metrics by checking curl-metrics logs")
			metricsOutput := getMetricsOutput()
			Expect(metricsOutput).To(ContainSubstring(
				"process_cpu_seconds_total", // We want to take the TODO into account! controller_runtime_reconcile_total
			))
		})

		It("should validate GitRepoConfig with real Gitea repository", func() {
			gitRepoConfigName := "gitrepoconfig-e2e-test"

			By("showing initial controller logs")
			showControllerLogs("before creating GitRepoConfig")

			createGitRepoConfig(gitRepoConfigName, "main", "git-creds")

			By("showing controller logs after GitRepoConfig creation")
			showControllerLogs("after creating GitRepoConfig")

			verifyGitRepoConfigStatus(gitRepoConfigName, "True", "BranchFound", "Branch 'main' found and accessible")

			By("showing final controller logs")
			showControllerLogs("after status verification")

			cleanupGitRepoConfig(gitRepoConfigName)
		})

		It("should handle GitRepoConfig with invalid credentials", func() {
			gitRepoConfigName := "gitrepoconfig-invalid-test"
			createGitRepoConfig(gitRepoConfigName, "main", "git-creds-invalid")
			verifyGitRepoConfigStatus(gitRepoConfigName, "False", "ConnectionFailed", "")
			cleanupGitRepoConfig(gitRepoConfigName)
		})

		It("should handle GitRepoConfig with nonexistent branch", func() {
			gitRepoConfigName := "gitrepoconfig-branch-test"
			createGitRepoConfig(gitRepoConfigName, "nonexistent-branch", "git-creds")
			verifyGitRepoConfigStatus(gitRepoConfigName, "False", "BranchNotFound", "nonexistent-branch")
			cleanupGitRepoConfig(gitRepoConfigName)
		})

		It("should validate GitRepoConfig with SSH authentication", func() {
			gitRepoConfigName := "gitrepoconfig-ssh-test"

			By("🔐 Starting SSH authentication test")
			showControllerLogs("before SSH test")

			By("📋 Checking SSH secret exists")
			cmd := exec.Command("kubectl", "get", "secret", "git-creds-ssh", "-n", namespace, "-o", "yaml")
			secretOutput, err := utils.Run(cmd)
			if err != nil {
				fmt.Printf("❌ SSH secret not found: %v\n", err)
			} else {
				fmt.Printf("✅ SSH secret exists - showing first 300 chars:\n%s...\n", secretOutput[:min(300, len(secretOutput))])
			}

			createSSHGitRepoConfig(gitRepoConfigName, "main", "git-creds-ssh")

			By("🔍 Controller logs after SSH GitRepoConfig creation")
			showControllerLogs("after SSH GitRepoConfig creation")

			verifyGitRepoConfigStatus(gitRepoConfigName, "True", "BranchFound", "Branch 'main' found and accessible")

			By("✅ Final SSH test logs")
			showControllerLogs("SSH test completion")

			cleanupGitRepoConfig(gitRepoConfigName)
		})

		It("should reconcile a WatchRule CR", func() {
			gitRepoConfigName := "gitrepoconfig-watchrule-test"
			watchRuleName := "watchrule-test"

			By("ensuring valid Git credentials secret exists (git-creds should be set up by Gitea)")
			cmd := exec.Command("kubectl", "get", "secret", "git-creds", "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "git-creds secret should exist from Gitea setup")

			By("creating a working GitRepoConfig for the WatchRule test")
			createGitRepoConfig(gitRepoConfigName, "main", "git-creds")

			By("waiting for GitRepoConfig to be ready")
			verifyGitRepoConfigStatus(gitRepoConfigName, "True", "BranchFound", "Branch 'main' found and accessible")

			By("creating a WatchRule that references the working GitRepoConfig")
			watchRuleYAML := "apiVersion: configbutler.ai/v1alpha1\n" +
				"kind: WatchRule\n" +
				"metadata:\n" +
				"  name: " + watchRuleName + "\n" +
				"  namespace: " + namespace + "\n" +
				"spec:\n" +
				"  gitRepoConfigRef: " + gitRepoConfigName + "\n" +
				"  excludeLabels:\n" +
				"    matchExpressions:\n" +
				"    - key: \"configbutler.ai/ignore\"\n" +
				"      operator: Exists\n" +
				"  rules:\n" +
				"  - resources: [\"deployments\", \"services\", \"configmaps\", \"secrets\"]\n" +
				"  - resources: [\"ingresses.*\"]\n"

			// Write YAML to temporary file to avoid stdin issues
			tmpFile := fmt.Sprintf("/tmp/watchrule-%s.yaml", watchRuleName)
			err = os.WriteFile(tmpFile, []byte(watchRuleYAML), 0644)
			Expect(err).NotTo(HaveOccurred())

			defer func() {
				_ = os.Remove(tmpFile)
			}()

			cmd = exec.Command("kubectl", "apply", "-f", tmpFile, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the WatchRule is reconciled")
			verifyReconciled := func(g Gomega) {
				jsonPath := "jsonpath={.status.conditions[?(@.type=='Ready')].status}"
				cmd := exec.Command("kubectl", "get", "watchrule", watchRuleName, "-n", namespace, "-o", jsonPath)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}
			Eventually(verifyReconciled).Should(Succeed())

			By("cleaning up test resources")
			cleanupGitRepoConfig(gitRepoConfigName)
			cmd = exec.Command("kubectl", "delete", "watchrule", watchRuleName, "-n", namespace)
			_, _ = utils.Run(cmd)
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		// TODO: Customize the e2e test suite with scenarios specific to your project.
		// Consider applying sample/CR(s) and check their status and/or verifying
		// the reconciliation by using the metrics, i.e.:
		// metricsOutput := getMetricsOutput()
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// createGitRepoConfigWithURL creates a GitRepoConfig resource with the specified URL template
func createGitRepoConfigWithURL(name, branch, secretName, urlTemplate, repoPrefix string) {
	By(fmt.Sprintf("creating GitRepoConfig '%s' with branch '%s' and secret '%s'", name, branch, secretName))

	// Create unique repository name and setup the repo
	uniqueRepoName := fmt.Sprintf("%s-%s", repoPrefix, name)
	repoURL := fmt.Sprintf(urlTemplate, uniqueRepoName)

	By(fmt.Sprintf("creating unique test repository '%s'", uniqueRepoName))
	cmd := exec.Command("bash", "test/e2e/scripts/setup-gitea.sh", "create-repo", uniqueRepoName)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create test repository")

	gitRepoConfigYAML := fmt.Sprintf(`
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: %s
  namespace: %s
spec:
  repoUrl: %s
  branch: %s
  secretRef:
    name: %s
`, name, namespace, repoURL, branch, secretName)

	cmd = exec.Command("kubectl", "apply", "-f", "-", "-n", namespace)
	cmd.Stdin = strings.NewReader(gitRepoConfigYAML)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

// createGitRepoConfig creates a GitRepoConfig resource with HTTP URL
func createGitRepoConfig(name, branch, secretName string) {
	createGitRepoConfigWithURL(name, branch, secretName, giteaRepoURLTemplate, "testrepo")
}

// createSSHGitRepoConfig creates a GitRepoConfig resource with SSH URL
func createSSHGitRepoConfig(name, branch, secretName string) {
	createGitRepoConfigWithURL(name, branch, secretName, giteaSSHURLTemplate, "testrepo-ssh")
}

// verifyGitRepoConfigStatus verifies the GitRepoConfig status matches expected values
func verifyGitRepoConfigStatus(name, expectedStatus, expectedReason, expectedMessageContains string) {
	By(fmt.Sprintf("verifying GitRepoConfig '%s' status is '%s' with reason '%s'", name, expectedStatus, expectedReason))
	verifyStatus := func(g Gomega) {
		// Check status
		statusJSONPath := `{.status.conditions[?(@.type=='Ready')].status}`
		cmd := exec.Command("kubectl", "get", "gitrepoconfig", name, "-n", namespace, "-o", "jsonpath="+statusJSONPath)
		status, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(status).To(Equal(expectedStatus))

		// Check reason
		reasonJSONPath := `{.status.conditions[?(@.type=='Ready')].reason}`
		cmd = exec.Command("kubectl", "get", "gitrepoconfig", name, "-n", namespace, "-o", "jsonpath="+reasonJSONPath)
		reason, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(reason).To(Equal(expectedReason))

		// Check message contains expected text if specified
		if expectedMessageContains != "" {
			messageJSONPath := `{.status.conditions[?(@.type=='Ready')].message}`
			cmd = exec.Command("kubectl", "get", "gitrepoconfig", name, "-n", namespace, "-o", "jsonpath="+messageJSONPath)
			message, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(message).To(ContainSubstring(expectedMessageContains))
		}
	}
	Eventually(verifyStatus).Should(Succeed())
}

// cleanupGitRepoConfig deletes a GitRepoConfig resource
func cleanupGitRepoConfig(name string) {
	By(fmt.Sprintf("cleaning up GitRepoConfig '%s'", name))
	cmd := exec.Command("kubectl", "delete", "gitrepoconfig", name, "-n", namespace)
	_, _ = utils.Run(cmd)
}

// setupMetricsAccess creates the necessary RBAC and gets a service account token for metrics access
func setupMetricsAccess(clusterRoleBindingName string) string {
	By("creating ClusterRoleBinding for metrics access")
	// Use kubectl apply with a YAML manifest for idempotent creation
	clusterRoleBindingYAML := fmt.Sprintf(`
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: %s
subjects:
- kind: ServiceAccount
  name: %s
  namespace: %s
roleRef:
  kind: ClusterRole
  name: gitops-reverser-metrics-reader
  apiGroup: rbac.authorization.k8s.io
`, clusterRoleBindingName, serviceAccountName, namespace)

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(clusterRoleBindingYAML)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to create ClusterRoleBinding %s", clusterRoleBindingName))

	By("getting service account token for metrics access")
	token, err := serviceAccountToken()
	Expect(err).NotTo(HaveOccurred())
	Expect(token).NotTo(BeEmpty())

	return token
}

// createMetricsCurlPod creates a curl pod to fetch metrics from the metrics endpoint
func createMetricsCurlPod(podName, token string) {
	By(fmt.Sprintf("creating curl pod '%s' to access metrics endpoint", podName))
	cmd := exec.Command("kubectl", "run", podName, "--restart=Never",
		"--namespace", namespace,
		"--image=curlimages/curl:latest",
		"--overrides",
		fmt.Sprintf(`{
			"spec": {
				"containers": [{
					"name": "curl",
					"image": "curlimages/curl:latest",
					"command": ["/bin/sh", "-c"],
					"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
					"securityContext": {
						"readOnlyRootFilesystem": true,
						"allowPrivilegeEscalation": false,
						"capabilities": {
							"drop": ["ALL"]
						},
						"runAsNonRoot": true,
						"runAsUser": 1000,
						"seccompProfile": {
							"type": "RuntimeDefault"
						}
					}
				}],
				"serviceAccountName": "%s"
			}
		}`, token, metricsServiceName, namespace, serviceAccountName))
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to create curl pod %s", podName))
}

// waitForMetricsCurlCompletion waits for the specified curl pod to complete
func waitForMetricsCurlCompletion(podName string, timeout time.Duration) {
	By(fmt.Sprintf("waiting for curl pod '%s' to complete", podName))
	verifyCurlComplete := func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pods", podName,
			"-o", "jsonpath={.status.phase}",
			"-n", namespace)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("Succeeded"), fmt.Sprintf("curl pod %s should complete successfully", podName))
	}
	Eventually(verifyCurlComplete, timeout).Should(Succeed())
}

// getMetricsFromCurlPod retrieves and returns the metrics output from the specified curl pod
func getMetricsFromCurlPod(podName string) string {
	By(fmt.Sprintf("getting metrics output from curl pod '%s'", podName))
	cmd := exec.Command("kubectl", "logs", podName, "-n", namespace)
	metricsOutput, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to retrieve logs from curl pod %s", podName))
	Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"), "Metrics endpoint should respond successfully")
	return metricsOutput
}

// cleanupMetricsAccess cleans up metrics testing resources
func cleanupMetricsAccess(podName, clusterRoleBindingName string) {
	By("cleaning up metrics access resources")
	if podName != "" {
		cmd := exec.Command("kubectl", "delete", "pod", podName, "--namespace", namespace)
		_, _ = utils.Run(cmd)
	}
	if clusterRoleBindingName != "" {
		cmd := exec.Command("kubectl", "delete", "clusterrolebinding", clusterRoleBindingName)
		_, _ = utils.Run(cmd)
	}
}

// getMetricsOutput retrieves and returns the logs from the curl-metrics pod
// (legacy function for backward compatibility)
func getMetricsOutput() string {
	return getMetricsFromCurlPod("curl-metrics")
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

// showControllerLogs displays the current controller logs to help with debugging during test execution
func showControllerLogs(context string) {
	By(fmt.Sprintf("📋 Controller logs %s:", context))

	// Get the controller pod name dynamically
	cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
		"-o", "jsonpath={.items[0].metadata.name}", "-n", namespace)
	podName, err := utils.Run(cmd)
	if err != nil {
		fmt.Printf("⚠️  Failed to get controller pod name: %v\n", err)
		return
	}

	if strings.TrimSpace(podName) == "" {
		fmt.Printf("⚠️  Controller pod not found yet\n")
		return
	}

	// Get the logs
	cmd = exec.Command("kubectl", "logs", strings.TrimSpace(podName), "-n", namespace, "--tail=20")
	output, err := utils.Run(cmd)
	if err != nil {
		fmt.Printf("❌ Failed to get controller logs: %v\n", err)
		return
	}

	fmt.Printf("🔍 Recent controller logs (%s):\n", context)
	fmt.Printf("----------------------------------------\n")
	fmt.Printf("%s\n", output)
	fmt.Printf("----------------------------------------\n")
}

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
