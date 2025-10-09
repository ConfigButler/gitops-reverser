package e2e

import (
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

// giteaRepoURLTemplate is the URL template for test Gitea repositories.
const giteaRepoURLTemplate = "http://gitea-http.gitea-e2e.svc.cluster.local:3000/testorg/%s.git"
const giteaSSHURLTemplate = "ssh://git@gitea-ssh.gitea-e2e.svc.cluster.local:2222/testorg/%s.git"

var testRepoName string
var checkoutDir string

// getRepoUrlHTTP returns the HTTP URL for the test repository.
func getRepoURLHTTP() string {
	return fmt.Sprintf(giteaRepoURLTemplate, testRepoName)
}

// getRepoUrlSSH returns the SSH URL for the test repository.
func getRepoURLSSH() string {
	return fmt.Sprintf(giteaSSHURLTemplate, testRepoName)
}

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string // Name of first controller pod for logging

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// deploying the controller, and setting up Gitea.
	BeforeAll(func() {
		By("preventive namespace cleanup")
		var err error
		cmd := exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)

		By("creating manager namespace")
		cmd = exec.Command("kubectl", "create", "ns", namespace)
		_, err = utils.Run(cmd)
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

		By("waiting for certificate secrets to be created by cert-manager")
		waitForCertificateSecrets()

		By("setting up Gitea test environment with unique repository")
		companyStart := time.Date(2025, 5, 12, 0, 0, 0, 0, time.UTC)
		minutesSinceStart := int(time.Since(companyStart).Minutes())
		testRepoName = fmt.Sprintf("e2e-test-%d", minutesSinceStart)
		checkoutDir = fmt.Sprintf("/tmp/gitops-reverser/%s", testRepoName)
		cmd = exec.Command("bash", "test/e2e/scripts/setup-gitea.sh", testRepoName, checkoutDir)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to setup Gitea test environment with repository")

		By("setting up Prometheus client for metrics testing")
		setupPrometheusClient()
		verifyPrometheusAvailable()
	})

	// After all tests have been executed, infrastructure remains running for debugging
	AfterAll(func() {
		By("test infrastructure still running for debugging")
		fmt.Printf("\n")
		fmt.Printf("═══════════════════════════════════════════════════════════\n")
		fmt.Printf("📊 E2E Infrastructure kept running for debugging purposes:\n")
		fmt.Printf("═══════════════════════════════════════════════════════════\n")
		fmt.Printf("  Prometheus: http://localhost:9090\n")
		fmt.Printf("  Gitea:      http://localhost:3000\n")
		fmt.Printf("\n")
		fmt.Printf("═══════════════════════════════════════════════════════════\n")
		fmt.Printf("\n")
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

	// Optimize timeouts for faster test execution
	SetDefaultEventuallyTimeout(
		30 * time.Second,
	) // Increased for reliability but still faster than before
	SetDefaultEventuallyPollingInterval(500 * time.Millisecond) // Faster polling

	Context("Manager", func() {

		It("should run successfully", func() {
			By("validating that the controller-manager pods are running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the names of the controller-manager pods
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
				g.Expect(podNames).To(HaveLen(2), "expected 2 controller pods running for HA")
				controllerPodName = podNames[0] // Use first pod for logging
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate all pods' status
				for _, podName := range podNames {
					cmd = exec.Command("kubectl", "get",
						"pods", podName, "-o", "jsonpath={.status.phase}",
						"-n", namespace,
					)
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal("Running"), fmt.Sprintf("Incorrect status for pod %s", podName))
				}
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should identify leader pod with role=leader label", func() {
			By("verifying that exactly one pod has the role=leader label")
			verifyLeaderLabel := func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager,role=leader",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve leader pod information")
				leaderPods := utils.GetNonEmptyLines(podOutput)
				g.Expect(leaderPods).To(HaveLen(1), "expected exactly 1 leader pod")

				leaderPodName := leaderPods[0]
				g.Expect(leaderPodName).To(ContainSubstring("controller-manager"))

				// Update controllerPodName to use the leader pod for subsequent tests
				controllerPodName = leaderPodName

				By(fmt.Sprintf("Leader pod identified: %s", leaderPodName))
			}
			Eventually(verifyLeaderLabel, 30*time.Second).Should(Succeed())
		})

		It("should route webhook traffic only to leader pod", func() {
			By("verifying webhook service selects only the leader pod")
			verifyWebhookService := func(g Gomega) {
				// Get webhook service endpoints
				cmd := exec.Command("kubectl", "get", "endpoints",
					"gitops-reverser-webhook-service", "-n", namespace,
					"-o", "jsonpath={.subsets[*].addresses[*].targetRef.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get webhook service endpoints")

				// Filter out kubectl deprecation warnings from output
				lines := utils.GetNonEmptyLines(output)
				var podNames []string
				for _, line := range lines {
					// Skip warning lines
					if !strings.HasPrefix(line, "Warning:") &&
						!strings.Contains(line, "deprecated") &&
						strings.Contains(line, "controller-manager") {
						podNames = append(podNames, line)
					}
				}

				// Should only have one endpoint (the leader pod)
				g.Expect(podNames).To(HaveLen(1), "webhook service should route to exactly 1 pod (leader)")

				// Verify it's the leader pod
				g.Expect(podNames[0]).To(Equal(controllerPodName), "webhook should route to leader pod")

				By(fmt.Sprintf("✅ Webhook service correctly routes to leader pod: %s", controllerPodName))
			}
			Eventually(verifyWebhookService, 30*time.Second).Should(Succeed())
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

			By("waiting for Prometheus to scrape controller metrics")
			waitForMetric("up{job='gitops-reverser-metrics'}",
				func(v float64) bool { return v == 1 },
				30*time.Second,
				"metrics endpoint should be up")

			By("verifying basic process metrics are exposed")
			waitForMetric("process_cpu_seconds_total{job='gitops-reverser-metrics'}",
				func(v float64) bool { return v > 0 },
				30*time.Second,
				"process metrics should exist")

			By("verifying metrics from both controller pods")
			podCount, err := queryPrometheus("count(up{job='gitops-reverser-metrics'})")
			Expect(err).NotTo(HaveOccurred())
			Expect(podCount).To(Equal(2.0), "Should scrape from 2 controller pods")

			fmt.Printf("✅ Metrics collection verified from %.0f pods\n", podCount)
			fmt.Printf("📊 Inspect metrics: %s\n", getPrometheusURL())
		})

		It("should receive webhook calls and process them successfully", func() {
			By("recording baseline webhook event count")
			baselineEvents, err := queryPrometheus("sum(gitopsreverser_events_received_total) or vector(0)")
			Expect(err).NotTo(HaveOccurred())
			fmt.Printf("📊 Baseline events: %.0f\n", baselineEvents)

			By("creating a ConfigMap to trigger webhook call")
			cmd := exec.Command("kubectl", "create", "configmap", "webhook-test-cm",
				"--namespace", namespace,
				"--from-literal=test=webhook")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "ConfigMap creation should succeed with working webhook")

			By("verifying that the controller manager logged the webhook call")
			verifyWebhookLogged := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Received admission request"),
					"Admission request not logged")
			}
			Eventually(verifyWebhookLogged).Should(Succeed())

			By("waiting for webhook event metric to increment")
			waitForMetric("sum(gitopsreverser_events_received_total) or vector(0)",
				func(v float64) bool { return v > baselineEvents },
				30*time.Second,
				"webhook events should increment")

			By("verifying only leader pod received webhook events")
			leaderEvents, err := queryPrometheus(
				"sum(gitopsreverser_events_received_total{role='leader'}) or vector(0)",
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(leaderEvents).To(BeNumerically(">", baselineEvents),
				"Leader should have processed webhook events")
			fmt.Printf("✅ Leader processed %.0f events\n", leaderEvents-baselineEvents)

			By("confirming follower pod has no new webhook events")
			followerEvents, err := queryPrometheus(
				"sum(gitopsreverser_events_received_total{role!='leader'}) or vector(0)",
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(followerEvents).To(Equal(0.0),
				"Follower should not process webhook events")

			fmt.Printf("✅ Webhook routing validated - only leader receives events\n")
			fmt.Printf("📊 Inspect metrics: %s\n", getPrometheusURL())

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
				fmt.Printf("✅ SSH secret exists - showing first 300 chars:\n%s...\n", secretOutput[:minInt(300, len(secretOutput))])
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
			gitRepoConfigName := "gitrepoconfig-configmap-test"
			watchRuleName := "watchrule-configmap-test"
			configMapName := "test-configmap"
			uniqueRepoName := testRepoName
			repoURL := getRepoURLHTTP()

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
			Eventually(verifyReconciled, 15*time.Second, time.Second).Should(Succeed())

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
				// Get controller logs from all pods (leader will have the reconciliation logs)
				cmd := exec.Command("kubectl", "logs", "-l", "control-plane=controller-manager",
					"-n", namespace, "--tail=500", "--prefix=true")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				// Check for git commit operation in logs
				g.Expect(output).To(ContainSubstring("git commit"),
					"Should see git commit operation in logs from leader pod")
			}
			Eventually(verifyReconciliationLogs, 45*time.Second, 2*time.Second).Should(Succeed())

			By("verifying ConfigMap YAML file exists in Gitea repository")
			verifyGitCommit := func(g Gomega) {
				// Use the pre-checked out repository directory
				By("using pre-checked out repository for verification")

				// Pull latest changes from the remote repository
				By("pulling latest changes from remote repository")
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = checkoutDir
				// Don't use utils.Run() here because it overwrites cmd.Dir with the project directory
				pullOutput, pullErr := pullCmd.CombinedOutput()
				if pullErr != nil {
					g.Expect(pullErr).NotTo(HaveOccurred(),
						fmt.Sprintf("Should successfully pull latest changes. Output: %s", string(pullOutput)))
				}

				// Check for the expected ConfigMap file
				expectedFile := filepath.Join(checkoutDir,
					fmt.Sprintf("namespaces/%s/configmaps/%s.yaml", namespace, configMapName))
				fileInfo, statErr := os.Stat(expectedFile)
				g.Expect(statErr).NotTo(HaveOccurred(), fmt.Sprintf("ConfigMap file should exist at %s", expectedFile))
				g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "ConfigMap file should not be empty")

				// Verify file content contains expected ConfigMap data
				content, readErr := os.ReadFile(expectedFile)
				g.Expect(readErr).NotTo(HaveOccurred())
				g.Expect(string(content)).To(ContainSubstring("test-key: test-value"),
					"ConfigMap file should contain expected data")
			}
			Eventually(verifyGitCommit, 180*time.Second, 5*time.Second).Should(Succeed())

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

		It("should delete Git file when ConfigMap is deleted via WatchRule", func() {
			gitRepoConfigName := "gitrepoconfig-delete-test"
			watchRuleName := "watchrule-delete-test"
			configMapName := "test-configmap-to-delete"
			uniqueRepoName := testRepoName
			repoURL := getRepoURLHTTP()

			By("creating GitRepoConfig for deletion test")
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
			Eventually(verifyReconciled, 15*time.Second, time.Second).Should(Succeed())

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

			By("waiting for ConfigMap file to appear in Git repository")
			verifyFileCreated := func(g Gomega) {
				// Pull latest changes from the remote repository
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = checkoutDir
				pullOutput, pullErr := pullCmd.CombinedOutput()
				if pullErr != nil {
					g.Expect(pullErr).NotTo(HaveOccurred(),
						fmt.Sprintf("Should successfully pull latest changes. Output: %s", string(pullOutput)))
				}

				// Check for the expected ConfigMap file
				expectedFile := filepath.Join(checkoutDir,
					fmt.Sprintf("namespaces/%s/configmaps/%s.yaml", namespace, configMapName))
				fileInfo, statErr := os.Stat(expectedFile)
				g.Expect(statErr).NotTo(HaveOccurred(), fmt.Sprintf("ConfigMap file should exist at %s", expectedFile))
				g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "ConfigMap file should not be empty")
			}
			Eventually(verifyFileCreated, 180*time.Second, 5*time.Second).Should(Succeed())

			By("deleting the ConfigMap to trigger DELETE operation")
			var cmd *exec.Cmd
			cmd = exec.Command("kubectl", "delete", "configmap", configMapName, "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "ConfigMap deletion should succeed")

			By("verifying ConfigMap file is deleted from Git repository")
			verifyFileDeleted := func(g Gomega) {
				// Pull latest changes from the remote repository
				By("pulling latest changes after deletion")
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = checkoutDir
				pullOutput, pullErr := pullCmd.CombinedOutput()
				if pullErr != nil {
					g.Expect(pullErr).NotTo(HaveOccurred(),
						fmt.Sprintf("Should successfully pull latest changes. Output: %s", string(pullOutput)))
				}

				// Check that the ConfigMap file no longer exists
				expectedFile := filepath.Join(checkoutDir,
					fmt.Sprintf("namespaces/%s/configmaps/%s.yaml", namespace, configMapName))
				_, statErr := os.Stat(expectedFile)
				g.Expect(statErr).To(HaveOccurred(), fmt.Sprintf("ConfigMap file should NOT exist at %s", expectedFile))
				g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "Error should be 'file does not exist'")

				// Verify git log shows DELETE commit
				By("verifying git log shows DELETE operation")
				gitLogCmd := exec.Command("git", "log", "--oneline", "-n", "5")
				gitLogCmd.Dir = checkoutDir
				logOutput, logErr := gitLogCmd.CombinedOutput()
				g.Expect(logErr).NotTo(HaveOccurred(), "Should be able to read git log")
				g.Expect(string(logOutput)).To(ContainSubstring("DELETE"),
					"Git log should contain DELETE operation")
			}
			Eventually(verifyFileDeleted, 180*time.Second, 5*time.Second).Should(Succeed())

			By("cleaning up test resources")
			cmd = exec.Command("kubectl", "delete", "watchrule", watchRuleName, "-n", namespace)
			_, _ = utils.Run(cmd)
			cleanupGitRepoConfig(gitRepoConfigName)

			By("✅ ConfigMap deletion E2E test passed - verified file removal from Git")
			fmt.Printf("✅ ConfigMap '%s' deletion successfully triggered Git commit removing file from repo '%s'\n",
				configMapName, uniqueRepoName)
		})

		It("should create Git commit when CRD instance is added via WatchRule", func() {
			gitRepoConfigName := "gitrepoconfig-crd-test"
			watchRuleName := "watchrule-crd-test"
			crdInstanceName := "test-myapp"
			uniqueRepoName := testRepoName
			repoURL := getRepoURLHTTP()

			By("installing the sample CRD")
			cmd := exec.Command("kubectl", "apply", "-f", "test/e2e/templates/sample-crd.yaml")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to install sample CRD")

			By("waiting for CRD to be established")
			verifyCRDEstablished := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "crd", "myapps.example.com",
					"-o", "jsonpath={.status.conditions[?(@.type=='Established')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}
			Eventually(verifyCRDEstablished, 30*time.Second, time.Second).Should(Succeed())

			By("creating GitRepoConfig for CRD test")
			createGitRepoConfigWithURL(gitRepoConfigName, "main", "git-creds", repoURL)

			By("waiting for GitRepoConfig to be ready")
			verifyGitRepoConfigStatus(gitRepoConfigName, "True", "BranchFound", "Branch 'main' found and accessible")

			By("creating WatchRule that monitors custom resources")
			data := struct {
				Name             string
				Namespace        string
				GitRepoConfigRef string
			}{
				Name:             watchRuleName,
				Namespace:        namespace,
				GitRepoConfigRef: gitRepoConfigName,
			}

			err2 := applyFromTemplate("test/e2e/templates/watchrule-crd.tmpl", data, namespace)
			Expect(err2).NotTo(HaveOccurred(), "Failed to apply WatchRule for CRDs")

			By("verifying WatchRule is ready")
			verifyReconciled := func(g Gomega) {
				jsonPath := "jsonpath={.status.conditions[?(@.type=='Ready')].status}"
				cmd := exec.Command("kubectl", "get", "watchrule", watchRuleName, "-n", namespace, "-o", jsonPath)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}
			Eventually(verifyReconciled, 15*time.Second, time.Second).Should(Succeed())

			By("creating CRD instance to trigger Git commit")
			crdInstanceData := struct {
				Name      string
				Namespace string
				Replicas  int
				Image     string
				Message   string
			}{
				Name:      crdInstanceName,
				Namespace: namespace,
				Replicas:  3,
				Image:     "nginx:1.21",
				Message:   "Initial CRD test instance",
			}

			err3 := applyFromTemplate("test/e2e/templates/myapp-instance.tmpl", crdInstanceData, namespace)
			Expect(err3).NotTo(HaveOccurred(), "Failed to apply CRD instance")

			By("waiting for controller reconciliation of CRD instance event")
			verifyReconciliationLogs := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", "-l", "control-plane=controller-manager",
					"-n", namespace, "--tail=500", "--prefix=true")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("git commit"),
					"Should see git commit operation in logs")
			}
			Eventually(verifyReconciliationLogs, 45*time.Second, 2*time.Second).Should(Succeed())

			By("verifying CRD instance YAML file exists in Gitea repository")
			verifyGitCommit := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = checkoutDir
				pullOutput, pullErr := pullCmd.CombinedOutput()
				if pullErr != nil {
					g.Expect(pullErr).NotTo(HaveOccurred(),
						fmt.Sprintf("Should successfully pull latest changes. Output: %s", string(pullOutput)))
				}

				expectedFile := filepath.Join(checkoutDir,
					fmt.Sprintf("namespaces/%s/myapps/%s.yaml", namespace, crdInstanceName))
				fileInfo, statErr := os.Stat(expectedFile)
				g.Expect(statErr).
					NotTo(HaveOccurred(), fmt.Sprintf("CRD instance file should exist at %s", expectedFile))
				g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "CRD instance file should not be empty")

				content, readErr := os.ReadFile(expectedFile)
				g.Expect(readErr).NotTo(HaveOccurred())
				g.Expect(string(content)).To(ContainSubstring("kind: MyApp"),
					"CRD instance file should contain MyApp kind")
				g.Expect(string(content)).To(ContainSubstring("replicas: 3"),
					"CRD instance file should contain expected spec")
				g.Expect(string(content)).To(ContainSubstring("nginx:1.21"),
					"CRD instance file should contain expected image")
			}
			Eventually(verifyGitCommit, 180*time.Second, 5*time.Second).Should(Succeed())

			By("cleaning up test resources")
			var cmd2 *exec.Cmd
			cmd2 = exec.Command("kubectl", "delete", "myapp", crdInstanceName, "-n", namespace)
			_, _ = utils.Run(cmd2)
			cmd2 = exec.Command("kubectl", "delete", "watchrule", watchRuleName, "-n", namespace)
			_, _ = utils.Run(cmd2)
			cleanupGitRepoConfig(gitRepoConfigName)

			By("✅ CRD instance to Git commit E2E test passed")
			fmt.Printf("✅ CRD instance '%s' successfully triggered Git commit in repo '%s'\n",
				crdInstanceName, uniqueRepoName)
		})

		It("should update Git file when CRD instance is modified via WatchRule", func() {
			gitRepoConfigName := "gitrepoconfig-crd-update-test"
			watchRuleName := "watchrule-crd-update-test"
			crdInstanceName := "test-myapp-update"
			uniqueRepoName := testRepoName
			repoURL := getRepoURLHTTP()

			By("creating GitRepoConfig for CRD update test")
			createGitRepoConfigWithURL(gitRepoConfigName, "main", "git-creds", repoURL)

			By("waiting for GitRepoConfig to be ready")
			verifyGitRepoConfigStatus(gitRepoConfigName, "True", "BranchFound", "Branch 'main' found and accessible")

			By("creating WatchRule that monitors custom resources")
			data := struct {
				Name             string
				Namespace        string
				GitRepoConfigRef string
			}{
				Name:             watchRuleName,
				Namespace:        namespace,
				GitRepoConfigRef: gitRepoConfigName,
			}

			err := applyFromTemplate("test/e2e/templates/watchrule-crd.tmpl", data, namespace)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply WatchRule for CRDs")

			By("verifying WatchRule is ready")
			verifyReconciled := func(g Gomega) {
				jsonPath := "jsonpath={.status.conditions[?(@.type=='Ready')].status}"
				cmd := exec.Command("kubectl", "get", "watchrule", watchRuleName, "-n", namespace, "-o", jsonPath)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}
			Eventually(verifyReconciled, 15*time.Second, time.Second).Should(Succeed())

			By("creating initial CRD instance")
			crdInstanceData := struct {
				Name      string
				Namespace string
				Replicas  int
				Image     string
				Message   string
			}{
				Name:      crdInstanceName,
				Namespace: namespace,
				Replicas:  2,
				Image:     "nginx:1.20",
				Message:   "Initial version",
			}

			err = applyFromTemplate("test/e2e/templates/myapp-instance.tmpl", crdInstanceData, namespace)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply initial CRD instance")

			By("waiting for initial CRD instance file to appear in Git")
			verifyInitialFile := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = checkoutDir
				_, _ = pullCmd.CombinedOutput()

				expectedFile := filepath.Join(checkoutDir,
					fmt.Sprintf("namespaces/%s/myapps/%s.yaml", namespace, crdInstanceName))
				content, readErr := os.ReadFile(expectedFile)
				g.Expect(readErr).NotTo(HaveOccurred())
				g.Expect(string(content)).To(ContainSubstring("replicas: 2"))
				g.Expect(string(content)).To(ContainSubstring("nginx:1.20"))
			}
			Eventually(verifyInitialFile, 180*time.Second, 5*time.Second).Should(Succeed())

			By("updating CRD instance with new values")
			updatedCRDData := struct {
				Name      string
				Namespace string
				Replicas  int
				Image     string
				Message   string
			}{
				Name:      crdInstanceName,
				Namespace: namespace,
				Replicas:  5,
				Image:     "nginx:1.22",
				Message:   "Updated version with more replicas",
			}

			err = applyFromTemplate("test/e2e/templates/myapp-instance.tmpl", updatedCRDData, namespace)
			Expect(err).NotTo(HaveOccurred(), "Failed to update CRD instance")

			By("verifying updated CRD instance content in Git")
			verifyUpdatedFile := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = checkoutDir
				_, _ = pullCmd.CombinedOutput()

				expectedFile := filepath.Join(checkoutDir,
					fmt.Sprintf("namespaces/%s/myapps/%s.yaml", namespace, crdInstanceName))
				content, readErr := os.ReadFile(expectedFile)
				g.Expect(readErr).NotTo(HaveOccurred())
				g.Expect(string(content)).To(ContainSubstring("replicas: 5"),
					"Updated file should contain new replica count")
				g.Expect(string(content)).To(ContainSubstring("nginx:1.22"),
					"Updated file should contain new image version")
				g.Expect(string(content)).To(ContainSubstring("Updated version"),
					"Updated file should contain new message")
			}
			Eventually(verifyUpdatedFile, 180*time.Second, 5*time.Second).Should(Succeed())

			By("cleaning up test resources")
			var cmd *exec.Cmd
			cmd = exec.Command("kubectl", "delete", "myapp", crdInstanceName, "-n", namespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "watchrule", watchRuleName, "-n", namespace)
			_, _ = utils.Run(cmd)
			cleanupGitRepoConfig(gitRepoConfigName)

			By("✅ CRD instance update E2E test passed")
			fmt.Printf("✅ CRD instance '%s' update successfully reflected in Git repo '%s'\n",
				crdInstanceName, uniqueRepoName)
		})

		It("should delete Git file when CRD instance is deleted via WatchRule", func() {
			gitRepoConfigName := "gitrepoconfig-crd-delete-test"
			watchRuleName := "watchrule-crd-delete-test"
			crdInstanceName := "test-myapp-to-delete"
			uniqueRepoName := testRepoName
			repoURL := getRepoURLHTTP()

			By("creating GitRepoConfig for CRD deletion test")
			createGitRepoConfigWithURL(gitRepoConfigName, "main", "git-creds", repoURL)

			By("waiting for GitRepoConfig to be ready")
			verifyGitRepoConfigStatus(gitRepoConfigName, "True", "BranchFound", "Branch 'main' found and accessible")

			By("creating WatchRule that monitors custom resources")
			data := struct {
				Name             string
				Namespace        string
				GitRepoConfigRef string
			}{
				Name:             watchRuleName,
				Namespace:        namespace,
				GitRepoConfigRef: gitRepoConfigName,
			}

			err := applyFromTemplate("test/e2e/templates/watchrule-crd.tmpl", data, namespace)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply WatchRule for CRDs")

			By("verifying WatchRule is ready")
			verifyReconciled := func(g Gomega) {
				jsonPath := "jsonpath={.status.conditions[?(@.type=='Ready')].status}"
				cmd := exec.Command("kubectl", "get", "watchrule", watchRuleName, "-n", namespace, "-o", jsonPath)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"))
			}
			Eventually(verifyReconciled, 15*time.Second, time.Second).Should(Succeed())

			By("creating CRD instance")
			crdInstanceData := struct {
				Name      string
				Namespace string
				Replicas  int
				Image     string
				Message   string
			}{
				Name:      crdInstanceName,
				Namespace: namespace,
				Replicas:  3,
				Image:     "nginx:1.21",
				Message:   "To be deleted",
			}

			err = applyFromTemplate("test/e2e/templates/myapp-instance.tmpl", crdInstanceData, namespace)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply CRD instance")

			By("waiting for CRD instance file to appear in Git repository")
			verifyFileCreated := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = checkoutDir
				_, _ = pullCmd.CombinedOutput()

				expectedFile := filepath.Join(checkoutDir,
					fmt.Sprintf("namespaces/%s/myapps/%s.yaml", namespace, crdInstanceName))
				fileInfo, statErr := os.Stat(expectedFile)
				g.Expect(statErr).
					NotTo(HaveOccurred(), fmt.Sprintf("CRD instance file should exist at %s", expectedFile))
				g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "CRD instance file should not be empty")
			}
			Eventually(verifyFileCreated, 180*time.Second, 5*time.Second).Should(Succeed())

			By("deleting the CRD instance to trigger DELETE operation")
			var cmd *exec.Cmd
			cmd = exec.Command("kubectl", "delete", "myapp", crdInstanceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "CRD instance deletion should succeed")

			By("verifying CRD instance file is deleted from Git repository")
			verifyFileDeleted := func(g Gomega) {
				pullCmd := exec.Command("git", "pull")
				pullCmd.Dir = checkoutDir
				_, _ = pullCmd.CombinedOutput()

				expectedFile := filepath.Join(checkoutDir,
					fmt.Sprintf("namespaces/%s/myapps/%s.yaml", namespace, crdInstanceName))
				_, statErr := os.Stat(expectedFile)
				g.Expect(statErr).
					To(HaveOccurred(), fmt.Sprintf("CRD instance file should NOT exist at %s", expectedFile))
				g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "Error should be 'file does not exist'")

				By("verifying git log shows DELETE commit")
				gitLogCmd := exec.Command("git", "log", "--oneline", "-n", "5")
				gitLogCmd.Dir = checkoutDir
				logOutput, logErr := gitLogCmd.CombinedOutput()
				g.Expect(logErr).NotTo(HaveOccurred(), "Should be able to read git log")
				g.Expect(string(logOutput)).To(ContainSubstring("DELETE"),
					"Git log should contain DELETE operation")
			}
			Eventually(verifyFileDeleted, 180*time.Second, 5*time.Second).Should(Succeed())

			By("cleaning up test resources")
			cmd = exec.Command("kubectl", "delete", "watchrule", watchRuleName, "-n", namespace)
			_, _ = utils.Run(cmd)
			cleanupGitRepoConfig(gitRepoConfigName)

			By("✅ CRD instance deletion E2E test passed")
			fmt.Printf("✅ CRD instance '%s' deletion successfully removed file from Git repo '%s'\n",
				crdInstanceName, uniqueRepoName)
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})
})

// createGitRepoConfigWithURL creates a GitRepoConfig resource with the specified URL.
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

// createGitRepoConfig creates a GitRepoConfig resource with HTTP URL.
func createGitRepoConfig(name, branch, secretName string) {
	createGitRepoConfigWithURL(name, branch, secretName, getRepoURLHTTP())
}

// createSSHGitRepoConfig creates a GitRepoConfig resource with SSH URL.
func createSSHGitRepoConfig(name, branch, secretName string) {
	createGitRepoConfigWithURL(name, branch, secretName, getRepoURLSSH())
}

// verifyGitRepoConfigStatus verifies the GitRepoConfig status matches expected values.
func verifyGitRepoConfigStatus(name, expectedStatus, expectedReason, expectedMessageContains string) {
	By(
		fmt.Sprintf(
			"verifying GitRepoConfig '%s' status is '%s' with reason '%s'",
			name,
			expectedStatus,
			expectedReason,
		),
	)
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
			cmd = exec.Command(
				"kubectl",
				"get",
				"gitrepoconfig",
				name,
				"-n",
				namespace,
				"-o",
				"jsonpath="+messageJSONPath,
			)
			message, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(message).To(ContainSubstring(expectedMessageContains))
		}
	}
	Eventually(verifyStatus).Should(Succeed())
}

// cleanupGitRepoConfig deletes a GitRepoConfig resource.
func cleanupGitRepoConfig(name string) {
	By(fmt.Sprintf("cleaning up GitRepoConfig '%s'", name))
	cmd := exec.Command("kubectl", "delete", "gitrepoconfig", name, "-n", namespace)
	_, _ = utils.Run(cmd)
}

// showControllerLogs displays the current controller logs to help with debugging during test execution.
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

// minInt returns the minimum of two integers.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
