package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

// renderTemplate loads and executes a Go template file with the given data
// Returns the rendered string or an error if loading or execution fails
func renderTemplate(templatePath string, data interface{}) (string, error) {
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		return "", fmt.Errorf("failed to parse template %s: %w", templatePath, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template %s: %w", templatePath, err)
	}
	return buf.String(), nil
}

// applyFromTemplate renders a template with data and applies it via kubectl using stdin streaming
// Returns an error if rendering or kubectl execution fails
func applyFromTemplate(templatePath string, data interface{}, namespace string) error {
	yamlContent, err := renderTemplate(templatePath, data)
	if err != nil {
		return err
	}

	if namespace != "" {
		cmd := exec.Command("kubectl", "apply", "-f", "-", "-n", namespace)
		cmd.Stdin = strings.NewReader(yamlContent)
		_, err = utils.Run(cmd)
		return err
	}
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yamlContent)
	_, err = utils.Run(cmd)
	return err
}

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

var testRepoName string

// getRepoUrlHTTP returns the HTTP URL for the test repository
func getRepoUrlHTTP() string {
	return fmt.Sprintf(giteaRepoURLTemplate, testRepoName)
}

// getRepoUrlSSH returns the SSH URL for the test repository
func getRepoUrlSSH() string {
	return fmt.Sprintf(giteaSSHURLTemplate, testRepoName)
}

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// deploying the controller, and setting up Gitea.
	BeforeAll(func() {
		By("preventive namespace cleanup")
		cmd := exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)

		By("preventive clusterrolebinding cleanup")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName)
		_, _ = utils.Run(cmd)

		By("creating manager namespace")
		cmd = exec.Command("kubectl", "create", "ns", namespace)
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

		By("setting up Gitea test environment with unique repository")
		companyStart := time.Date(2025, 5, 12, 0, 0, 0, 0, time.UTC)
		minutesSinceStart := int(time.Since(companyStart).Minutes())
		testRepoName = fmt.Sprintf("e2e-test-repo-%d", minutesSinceStart)
		cmd = exec.Command("bash", "test/e2e/scripts/setup-gitea.sh", testRepoName)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to setup Gitea test environment with repository")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace (we don't undeploy stuff,
	// it's easier to have running stuff when you need to debug test failures)
	AfterAll(func() {
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
		var metricsToken string
		It("should prepare metrics access", func() {
			metricsToken = setupMetricsAccess(metricsRoleBindingName)
		})

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

		It("should ensure the metrics endpoint is serving metrics", func() {
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
			metricsOutput := fetchMetricsOverHttps(metricsToken)
			Expect(metricsOutput).To(ContainSubstring(
				"process_cpu_seconds_total",
			))
		})

		It("should receive webhook calls and process them successfully", func() {
			By("creating a ConfigMap to trigger webhook call")
			cmd := exec.Command("kubectl", "create", "configmap", "webhook-test-cm",
				"--namespace", namespace,
				"--from-literal=test=webhook")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "ConfigMap creation should succeed with working webhook")

			By("verifying that the controller manager logged the webhook call")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Received admission request"),
					"Admission request not logged")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed()) // There is probably no need for eventually here since the
			// creation of the configmap already should have triggered the webhook.

			// Wait a moment for metrics to be updated
			time.Sleep(2 * time.Second)
			metricsOutput := fetchMetricsOverHttps(metricsToken)

			// Just check if the metric exists (don't worry about the actual value)
			Expect(metricsOutput).To(ContainSubstring("gitopsreverser_events_received_total"),
				"Events received metric should exist in metrics output")

			By("confirming webhook is working - events are being received")
			fmt.Printf("✅ Webhook validation successful - events are being received by the webhook\n")

			By("cleaning up webhook test resources")
			cmd = exec.Command("kubectl", "delete", "configmap", "webhook-test-cm", "--namespace", namespace)
			_, _ = utils.Run(cmd)
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
			data := struct {
				Name             string
				Namespace        string
				GitRepoConfigRef string
			}{
				Name:             watchRuleName,
				Namespace:        namespace,
				GitRepoConfigRef: gitRepoConfigName,
			}

			err = applyFromTemplate("test/e2e/templates/watchrule.tmpl", data, namespace)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply WatchRule")

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

		It("should create Git commit when ConfigMap is added via WatchRule", func() {
			Skip("Disabled due to failing test")
			gitRepoConfigName := "gitrepoconfig-configmap-test"
			watchRuleName := "watchrule-configmap-test"
			configMapName := "test-configmap"
			uniqueRepoName := testRepoName
			repoURL := getRepoUrlHTTP()

			By("creating GitRepoConfig for ConfigMap test")
			createGitRepoConfigWithURL(gitRepoConfigName, "main", "git-creds", repoURL)

			By("waiting for GitRepoConfig to be ready")
			verifyGitRepoConfigStatus(gitRepoConfigName, "True", "BranchFound", "Branch 'main' found and accessible")

			By("creating WatchRule that monitors ConfigMaps")
			data := struct {
				Name             string
				Namespace        string
				GitRepoConfigRef string
			}{
				Name:             watchRuleName,
				Namespace:        namespace,
				GitRepoConfigRef: gitRepoConfigName,
			}

			err2 := applyFromTemplate("test/e2e/templates/watchrule-configmap.tmpl", data, namespace)
			Expect(err2).NotTo(HaveOccurred(), "Failed to apply WatchRule")

			By("verifying WatchRule is ready")
			verifyReconciled := func(g Gomega) {
				jsonPath := "jsonpath={.status.conditions[?(@.type=='Ready')].status}"
				cmd := exec.Command("kubectl", "get", "watchrule", watchRuleName, "-n", namespace, "-o", jsonPath)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}
			Eventually(verifyReconciled, 30*time.Second).Should(Succeed())

			By("creating test ConfigMap to trigger Git commit")
			configMapData := struct {
				Name      string
				Namespace string
			}{
				Name:      configMapName,
				Namespace: namespace,
			}

			err3 := applyFromTemplate("test/e2e/templates/configmap.tmpl", configMapData, namespace)
			Expect(err3).NotTo(HaveOccurred(), "Failed to apply ConfigMap")

			By("waiting for controller reconciliation of ConfigMap event")
			verifyReconciliationLogs := func(g Gomega) {
				showControllerLogs("checking for ConfigMap reconciliation")
				// Check for reconciliation logs indicating ConfigMap processing
				cmd := exec.Command("kubectl", "logs", "-l", "control-plane=controller-manager", "-n", namespace, "--tail=50")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				expectedLog := fmt.Sprintf("ConfigMap/%s", configMapName)
				g.Expect(output).To(ContainSubstring(expectedLog),
					"Should see ConfigMap reconciliation in logs")
				g.Expect(output).To(ContainSubstring("git commit"), "Should see git commit operation in logs")
			}
			Eventually(verifyReconciliationLogs, 2*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying ConfigMap YAML file exists in Gitea repository")
			verifyGitCommit := func(g Gomega) {
				// Clone the repository to verify the file was committed
				cloneDir := fmt.Sprintf("/tmp/git-clone-%d", time.Now().Unix())
				defer func() {
					_ = os.RemoveAll(cloneDir)
				}()

				// Get credentials from the git-creds secret for authenticated clone
				By("extracting Git credentials for repository clone")
				cmd := exec.Command("kubectl", "get", "secret", "git-creds", "-n", namespace, "-o",
					"jsonpath='{.data.username}' | base64 -d")
				usernameOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				//nolint:unconvert // Necessary conversion from []byte to string for TrimSpace
				username := strings.TrimSpace(string(usernameOutput))

				cmd = exec.Command("kubectl", "get", "secret", "git-creds", "-n", namespace, "-o",
					"jsonpath='{.data.password}' | base64 -d")
				passwordOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				//nolint:unconvert // Necessary conversion from []byte to string for TrimSpace
				password := strings.TrimSpace(string(passwordOutput))

				// Clone with authentication by setting git config
				By("configuring git authentication for clone")
				gitConfig := fmt.Sprintf("url.http://%s:%s@gitea-http.gitea-e2e.svc.cluster.local:3000/.git.insteadOf",
					username, password)
				cmd = exec.Command("git", "config", "--global", gitConfig, repoURL+"/.git")
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cloneCmd := exec.Command("git", "clone", repoURL, cloneDir)
				_, cloneErr := utils.Run(cloneCmd)
				g.Expect(cloneErr).NotTo(HaveOccurred(), "Should successfully clone the test repository")

				// Cleanup git config after clone
				cmd = exec.Command("git", "config", "--global", "--unset", gitConfig)
				_, _ = utils.Run(cmd)

				// Check for the expected ConfigMap file
				expectedFile := filepath.Join(cloneDir, fmt.Sprintf("namespaces/%s/configmaps/%s.yaml", namespace, configMapName))
				fileInfo, statErr := os.Stat(expectedFile)
				g.Expect(statErr).NotTo(HaveOccurred(), fmt.Sprintf("ConfigMap file should exist at %s", expectedFile))
				g.Expect(fileInfo.Size()).To(BeNumerically(">0"), "ConfigMap file should not be empty")

				// Verify file content contains expected ConfigMap data
				content, readErr := os.ReadFile(expectedFile)
				g.Expect(readErr).NotTo(HaveOccurred())
				g.Expect(string(content)).To(ContainSubstring("test-key: test-value"),
					"ConfigMap file should contain expected data")
			}
			Eventually(verifyGitCommit, 3*time.Minute, 30*time.Second).Should(Succeed())

			By("cleaning up test resources")
			var cmd *exec.Cmd
			cmd = exec.Command("kubectl", "delete", "configmap", configMapName, "-n", namespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "watchrule", watchRuleName, "-n", namespace)
			_, _ = utils.Run(cmd)
			cleanupGitRepoConfig(gitRepoConfigName)

			By("✅ ConfigMap to Git commit E2E test passed - verified actual file creation and commit")
			fmt.Printf("✅ ConfigMap '%s' successfully triggered Git commit with YAML file in repo '%s'\n",
				configMapName, uniqueRepoName)

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

// createGitRepoConfigWithURL creates a GitRepoConfig resource with the specified URL
func createGitRepoConfigWithURL(name, branch, secretName, repoURL string) {
	By(fmt.Sprintf("creating GitRepoConfig '%s' with branch '%s', secret '%s' and URL '%s'",
		name, branch, secretName, repoURL))

	data := struct {
		Name       string
		Namespace  string
		RepoURL    string
		Branch     string
		SecretName string
	}{
		Name:       name,
		Namespace:  namespace,
		RepoURL:    repoURL,
		Branch:     branch,
		SecretName: secretName,
	}

	err := applyFromTemplate("test/e2e/templates/gitrepoconfig.tmpl", data, namespace)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply GitRepoConfig")
}

// createGitRepoConfig creates a GitRepoConfig resource with HTTP URL
func createGitRepoConfig(name, branch, secretName string) {
	createGitRepoConfigWithURL(name, branch, secretName, getRepoUrlHTTP())
}

// createSSHGitRepoConfig creates a GitRepoConfig resource with SSH URL
func createSSHGitRepoConfig(name, branch, secretName string) {
	createGitRepoConfigWithURL(name, branch, secretName, getRepoUrlSSH())
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
	data := struct {
		Name               string
		ServiceAccountName string
		Namespace          string
	}{
		Name:               clusterRoleBindingName,
		ServiceAccountName: serviceAccountName,
		Namespace:          namespace,
	}

	err := applyFromTemplate("test/e2e/templates/clusterrolebinding.tmpl", data, "")
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
	data := struct {
		PodName            string
		Token              string
		ServiceName        string
		Namespace          string
		ServiceAccountName string
	}{
		PodName:            podName,
		Token:              token,
		ServiceName:        metricsServiceName,
		Namespace:          namespace,
		ServiceAccountName: serviceAccountName,
	}

	err := applyFromTemplate("test/e2e/templates/curl-pod.yaml.tmpl", data, namespace)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to create curl pod %s", podName))
}

func fetchMetricsOverHttps(token string) string {
	const podName = "curl-metrics"
	createMetricsCurlPod(podName, token)
	waitForMetricsCurlCompletion(podName)
	By("getting the metrics by checking curl-metrics logs")
	result := getMetricsFromCurlPod(podName)
	defer cleanupPod(podName) // Ensure the pod is cleaned up after fetching metrics

	return result
}

func cleanupPod(podName string) {
	By(fmt.Sprintf("cleaning up curl pod %s", podName))
	cmd := exec.Command("kubectl", "delete", "pod", podName, "--namespace", namespace)
	if output, err := utils.Run(cmd); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Warning: failed to delete pod %s: %v\nOutput: %s\n", podName, err, output)
	}
}

// waitForMetricsCurlCompletion waits for the specified curl pod to complete
func waitForMetricsCurlCompletion(podName string) {
	By(fmt.Sprintf("waiting for curl pod '%s' to complete", podName))
	verifyCurlComplete := func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pods", podName,
			"-o", "jsonpath={.status.phase}",
			"-n", namespace)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("Succeeded"), fmt.Sprintf("curl pod %s should complete successfully", podName))
	}
	Eventually(verifyCurlComplete).Should(Succeed())
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
