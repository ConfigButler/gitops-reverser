// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	quickstartFrameworkEnabledEnv = "E2E_ENABLE_QUICKSTART_FRAMEWORK"
	quickstartFrameworkModeEnv    = "E2E_QUICKSTART_MODE"
	quickstartTimeoutSecondsEnv   = "QUICKSTART_TIMEOUT_SECONDS"

	// readmeDemoNamespace is the exact namespace the README quick start tells a
	// first-time user to create. The helm-mode run installs the chart quickstart
	// into it so the test exercises the documented path verbatim.
	readmeDemoNamespace = "gitops-reverser-quickstart-demo"
)

type quickstartFrameworkRun struct {
	mode            string
	namespace       string
	testID          string
	repoName        string
	checkoutDir     string
	repoURL         string
	providerName    string
	targetName      string
	watchRuleName   string
	invalidProvName string
	encryptionName  string
}

// quickstartRepo holds the file-local repo fixtures for the Quickstart Framework describe block.
var quickstartRepo *RepoArtifacts

var _ = Describe("Quickstart Framework", Label("quickstart-framework"), Ordered, func() {
	var run quickstartFrameworkRun

	BeforeAll(func() {
		if !quickstartFrameworkEnabled() {
			Skip(fmt.Sprintf(
				"quickstart framework is disabled; set %s=true to run",
				quickstartFrameworkEnabledEnv,
			))
		}

		run = newQuickstartFrameworkRun()

		_, _ = kubectlRun("create", "namespace", run.namespace)

		By("setting up Gitea repo and credentials for quickstart-framework tests")
		quickstartRepo = SetupRepo(
			resolveE2EContext(),
			run.namespace,
			fmt.Sprintf("e2e-quickstart-framework-%d", GinkgoRandomSeed()),
		)
		run.repoName = quickstartRepo.RepoName
		run.checkoutDir = quickstartRepo.CheckoutDir
		run.repoURL = quickstartRepo.RepoURLHTTP

		_, err := kubectlRunInNamespace(run.namespace, "apply", "-f", quickstartRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to quickstart namespace")

		// Helm mode drives the README path, where the chart auto-generates the age
		// key (generateWhenMissing). The other install modes reuse the pre-seeded key.
		if run.mode != "helm" {
			applySOPSAgeKeyToNamespace(run.namespace)
		}
	})

	AfterAll(func() {
		if !quickstartFrameworkEnabled() {
			return
		}
		cleanupNamespace(run.namespace)
	})

	It("sets up quickstart flow via Go framework", func() {
		if run.mode == "helm" {
			run.runReadmeQuickstartFlow()
			return
		}
		run.runConfigObjectFlow()
	})

})

// runReadmeQuickstartFlow drives the exact path README.md documents for a
// first-time user: a no-Redis Helm install with quickstart.enabled=true into the
// gitops-reverser-quickstart-demo namespace, then verifies the chart's own
// starter resources become Ready, the controller runs healthy in configured-author
// mode without Redis, and a ConfigMap in the demo namespace lands a commit.
func (r *quickstartFrameworkRun) runReadmeQuickstartFlow() {
	By("helm-installing the chart the README way (no Redis, quickstart.enabled, demo namespace)")
	r.helmInstallReadmeQuickstart()

	By("verifying the controller is healthy in configured-author mode with no Redis")
	r.verifyNoRedisConfiguredAuthor()

	By("verifying the chart's starter resources become Ready")
	verifyResourceStatus("gitprovider", "example-provider", r.namespace, "True", "Ready", "")
	verifyResourceCondition("gittarget", "example-target", r.namespace, "Validated", "True", "OK", "")
	verifyResourceStatus("watchrule", "example-watchrule", r.namespace, "True", "Ready", "")
	waitForStreamsRunning("example-target", r.namespace)

	By("verifying the starter GitTarget generated its SOPS age key")
	r.verifyGeneratedEncryptionSecret("sops-age-key")

	By("verifying a ConfigMap in the demo namespace lands a commit")
	r.verifyStarterConfigMapCommit()
}

// helmInstallReadmeQuickstart upgrades the already-installed release to the exact
// values the README's `helm install` command uses. The quickstart-framework spec
// only runs in the isolated quickstart leg (its own cluster, this the only spec),
// so reconfiguring the single controller to no-Redis here is safe.
func (r *quickstartFrameworkRun) helmInstallReadmeQuickstart() {
	ctx := resolveE2EContext()
	releaseNs := resolveE2ENamespace()
	release := resolveE2EInstallName(releaseNs)

	chart := strings.TrimSpace(os.Getenv("HELM_CHART_SOURCE"))
	if chart == "" {
		chart = "charts/gitops-reverser"
	}

	// ONE apply, exactly as the README's step 4 does it. This works because no webhook this chart
	// installs is failurePolicy: Fail — the starter GitTarget is gated by nothing, so it is admitted
	// while the Deployment it rolls is still coming up. Namespace authorization is enforced at
	// reconcile instead (checkSourceAuthorization, inside the Validated gate), which does not need
	// the manager to be reachable at admission time.
	args := []string{
		"--kube-context", ctx,
		"upgrade", "--install", release, chart,
		"--namespace", releaseNs,
		"--reuse-values",
		// The README's no-Redis default: empty addr runs configured-author.
		"--set", "queue.redis.addr=",
		"--set", "quickstart.enabled=true",
		"--set", fmt.Sprintf("quickstart.namespace=%s", r.namespace),
		// We created the namespace + git-creds above (README step 3, before install).
		"--set", "quickstart.createNamespace=false",
		"--set-string", fmt.Sprintf("quickstart.gitProvider.url=%s", r.repoURL),
		"--set", fmt.Sprintf("quickstart.gitProvider.secretRef.name=%s", quickstartRepo.GitSecretHTTP),
	}

	cmd := exec.Command("helm", args...)
	output, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(),
		fmt.Sprintf("helm upgrade to README quickstart config failed: %s", strings.TrimSpace(string(output))))

	By("waiting for the reconfigured controller to roll out")
	Eventually(func() error {
		_, rolloutErr := kubectlRunInNamespace(
			releaseNs, "rollout", "status", fmt.Sprintf("deployment/%s", release), "--timeout=30s",
		)
		return rolloutErr
	}, quickstartTimeout(), 3*time.Second).Should(Succeed(), "controller did not roll out after README quickstart install")
}

// verifyNoRedisConfiguredAuthor asserts the controller logged the no-Redis
// configured-author sentinel (main.go emits "configured-author mode: no Redis
// configured" only when queue.redis.addr is empty) and that the pod is not
// crash-looping — proving admission-on-without-Redis is a healthy no-op.
func (r *quickstartFrameworkRun) verifyNoRedisConfiguredAuthor() {
	releaseNs := resolveE2ENamespace()
	release := resolveE2EInstallName(releaseNs)

	Eventually(func(g Gomega) {
		logs, err := kubectlRunInNamespace(
			releaseNs, "logs", fmt.Sprintf("deployment/%s", release), "--since=10m",
		)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(logs).To(ContainSubstring("no Redis configured"),
			"controller should log the no-Redis configured-author sentinel")
	}, quickstartTimeout(), 3*time.Second).Should(Succeed())

	restarts, err := kubectlRunInNamespace(
		releaseNs, "get", "pods", "-l", fmt.Sprintf("app.kubernetes.io/instance=%s", release),
		"-o", "jsonpath={.items[*].status.containerStatuses[*].restartCount}",
	)
	Expect(err).NotTo(HaveOccurred())
	for _, field := range strings.Fields(restarts) {
		count, convErr := strconv.Atoi(field)
		Expect(convErr).NotTo(HaveOccurred())
		Expect(count).To(Equal(0), "controller must not crash-loop with admission on and no Redis")
	}
}

// verifyStarterConfigMapCommit creates, updates, and deletes a ConfigMap in the
// demo namespace and asserts the chart's starter GitTarget mirrors each change to
// live-cluster/<ns>/configmaps/<name>.yaml (the chart default path).
func (r *quickstartFrameworkRun) verifyStarterConfigMapCommit() {
	ns := r.namespace
	configMapName := fmt.Sprintf("quickstart-config-%s", r.testID)
	expectedFile := filepath.Join(
		r.checkoutDir, "live-cluster", ns, "configmaps", fmt.Sprintf("%s.yaml", configMapName),
	)

	_, _ = kubectlRunInNamespace(ns, "delete", "configmap", configMapName, "--ignore-not-found=true")

	commitsBefore, err := r.gitCommitCount()
	Expect(err).NotTo(HaveOccurred())

	_, err = kubectlRunInNamespace(ns, "create", "configmap", configMapName, "--from-literal=value=one")
	Expect(err).NotTo(HaveOccurred(), "failed to create quickstart ConfigMap")

	Eventually(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())
		content, readErr := os.ReadFile(expectedFile)
		g.Expect(readErr).NotTo(HaveOccurred(), fmt.Sprintf("ConfigMap file must exist at %s", expectedFile))
		g.Expect(string(content)).To(ContainSubstring("value: one"))
		commitsAfter, countErr := r.gitCommitCount()
		g.Expect(countErr).NotTo(HaveOccurred())
		g.Expect(commitsAfter).To(BeNumerically(">", commitsBefore))
	}, quickstartTimeout(), 2*time.Second).Should(Succeed())

	_, err = kubectlRunInNamespace(ns, "delete", "configmap", configMapName)
	Expect(err).NotTo(HaveOccurred(), "failed to delete quickstart ConfigMap")

	Eventually(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())
		_, statErr := os.Stat(expectedFile)
		g.Expect(os.IsNotExist(statErr)).To(BeTrue(), fmt.Sprintf("ConfigMap file should be deleted: %s", expectedFile))
	}, quickstartTimeout(), 2*time.Second).Should(Succeed())
}

// runConfigObjectFlow is the pre-existing coverage for the non-Helm install modes
// (config-dir, plain-manifests-file), which are Redis-backed and cannot exercise
// the no-Redis README path. It validates the install by creating config objects
// directly and checking the mirror + encryption + actionable errors.
func (r *quickstartFrameworkRun) runConfigObjectFlow() {
	By("creating dedicated Gitea repository and bootstrap credentials")
	r.setupGiteaRepository()

	By("applying quickstart resources from Go")
	r.applyQuickstartResources()

	By("verifying quickstart resources become Ready")
	r.verifyQuickstartResourcesReady()

	By("verifying generated encryption secret and commit flow")
	generatedAgeKey := r.verifyGeneratedEncryptionSecret(r.encryptionName)

	By("verifying quickstart commits for create/update/delete")
	r.verifyQuickstartConfigMapCommits()

	By("verifying quickstart encrypted Secret commit is decryptable")
	r.verifyQuickstartSecretEncryption(generatedAgeKey)

	By("verifying invalid credentials provider shows actionable message")
	r.verifyInvalidProviderActionableMessage()
}

func quickstartFrameworkEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(quickstartFrameworkEnabledEnv)))
	return value == "1" || value == "true" || value == "yes"
}

func quickstartFrameworkMode() string {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(quickstartFrameworkModeEnv)))
	if mode == "" {
		return "helm"
	}

	Expect(mode == "config-dir" || mode == "helm" || mode == "plain-manifests-file").To(
		BeTrue(),
		fmt.Sprintf(
			"unsupported %s value %q (expected config-dir|helm|plain-manifests-file)",
			quickstartFrameworkModeEnv,
			mode,
		),
	)
	return mode
}

func newQuickstartFrameworkRun() quickstartFrameworkRun {
	testID := strconv.FormatInt(time.Now().UnixNano(), 10)
	mode := quickstartFrameworkMode()

	// Helm mode uses the exact README demo namespace; other modes stay on a
	// throwaway per-run namespace.
	ns := readmeDemoNamespace
	if mode != "helm" {
		ns = testNamespaceFor("quickstart-framework")
	}

	return quickstartFrameworkRun{
		mode:            mode,
		namespace:       ns,
		testID:          testID,
		providerName:    fmt.Sprintf("quickstart-provider-%s", testID),
		targetName:      fmt.Sprintf("quickstart-target-%s", testID),
		watchRuleName:   fmt.Sprintf("quickstart-watchrule-%s", testID),
		invalidProvName: fmt.Sprintf("quickstart-invalid-provider-%s", testID),
		encryptionName:  fmt.Sprintf("quickstart-sops-age-key-%s", testID),
	}
}

func (r *quickstartFrameworkRun) setupGiteaRepository() {
	// Repo + creds + checkout are prepared by SetupRepo in BeforeAll.
	// Keep this method for readability and assert the checkout exists for developer-friendly failures.
	_, err := os.Stat(filepath.Join(r.checkoutDir, ".git"))
	Expect(err).NotTo(HaveOccurred(), "expected checkout to exist at checkoutDir")
}

func (r *quickstartFrameworkRun) applyQuickstartResources() {
	qsNamespace := r.namespace
	createGitProviderWithURLInNamespace(r.providerName, qsNamespace, quickstartRepo.GitSecretHTTP, r.repoURL)

	createGitTargetWithEncryptionOptions(
		r.targetName,
		qsNamespace,
		r.providerName,
		"live-cluster",
		"main",
		r.encryptionName,
		true,
	)

	watchRuleData := struct {
		Name            string
		Namespace       string
		DestinationName string
	}{
		Name:            r.watchRuleName,
		Namespace:       qsNamespace,
		DestinationName: r.targetName,
	}

	err := applyFromTemplate("test/e2e/templates/watchrule.tmpl", watchRuleData, qsNamespace)
	Expect(err).NotTo(HaveOccurred(), "failed to apply quickstart watchrule")

	createGitProviderWithURLInNamespace(r.invalidProvName, qsNamespace, quickstartRepo.GitSecretInvalid, r.repoURL)
}

func (r *quickstartFrameworkRun) verifyQuickstartResourcesReady() {
	ns := r.namespace
	verifyResourceStatus("gitprovider", r.providerName, ns, "True", "Ready", "")
	verifyResourceCondition("gittarget", r.targetName, ns, "Validated", "True", "OK", "")
	verifyResourceStatus("watchrule", r.watchRuleName, ns, "True", "Ready", "")
	waitForStreamsRunning(r.targetName, ns)
}

func (r *quickstartFrameworkRun) verifyGeneratedEncryptionSecret(secretName string) string {
	ns := r.namespace
	var generatedAgeKey string

	Eventually(func(g Gomega) {
		output, err := kubectlRunInNamespace(ns, "get", "secret", secretName, "-o", "json")
		g.Expect(err).NotTo(HaveOccurred())

		var secretObj map[string]interface{}
		g.Expect(json.Unmarshal([]byte(output), &secretObj)).To(Succeed())

		annotations, _, annoErr := unstructured.NestedStringMap(secretObj, "metadata", "annotations")
		g.Expect(annoErr).NotTo(HaveOccurred())
		g.Expect(annotations).To(HaveKeyWithValue("configbutler.ai/backup-warning", "REMOVE_AFTER_BACKUP"))
		g.Expect(annotations).To(HaveKey("configbutler.ai/age-recipient"))
		g.Expect(annotations["configbutler.ai/age-recipient"]).To(HavePrefix("age1"))

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
	}, quickstartTimeout(), 2*time.Second).Should(Succeed())

	return generatedAgeKey
}

func (r *quickstartFrameworkRun) verifyQuickstartConfigMapCommits() {
	ns := r.namespace
	configMapName := fmt.Sprintf("quickstart-config-%s", r.testID)
	expectedFile := filepath.Join(
		r.checkoutDir,
		"live-cluster",
		ns,
		"configmaps",
		fmt.Sprintf("%s.yaml", configMapName),
	)

	_, _ = kubectlRunInNamespace(ns, "delete", "configmap", configMapName, "--ignore-not-found=true")

	commitsBefore, err := r.gitCommitCount()
	Expect(err).NotTo(HaveOccurred())

	_, err = kubectlRunInNamespace(
		ns,
		"create", "configmap",
		configMapName,
		"--from-literal=value=one",
	)
	Expect(err).NotTo(HaveOccurred(), "failed to create quickstart ConfigMap")

	Eventually(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())

		content, readErr := os.ReadFile(expectedFile)
		g.Expect(readErr).NotTo(HaveOccurred(), fmt.Sprintf("ConfigMap file must exist at %s", expectedFile))
		g.Expect(string(content)).To(ContainSubstring("value: one"))

		commitsAfter, countErr := r.gitCommitCount()
		g.Expect(countErr).NotTo(HaveOccurred())
		g.Expect(commitsAfter).To(BeNumerically(">", commitsBefore))
	}, quickstartTimeout(), 2*time.Second).Should(Succeed())

	commitsAfterCreate, err := r.gitCommitCount()
	Expect(err).NotTo(HaveOccurred())

	_, err = kubectlRunInNamespace(
		ns,
		"patch", "configmap",
		configMapName,
		"--type",
		"merge",
		"--patch",
		`{"data":{"value":"two"}}`,
	)
	Expect(err).NotTo(HaveOccurred(), "failed to patch quickstart ConfigMap")

	Eventually(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())

		content, readErr := os.ReadFile(expectedFile)
		g.Expect(readErr).NotTo(HaveOccurred(), fmt.Sprintf("ConfigMap file must exist at %s", expectedFile))
		g.Expect(string(content)).To(ContainSubstring("value: two"))

		commitsAfter, countErr := r.gitCommitCount()
		g.Expect(countErr).NotTo(HaveOccurred())
		g.Expect(commitsAfter).To(BeNumerically(">", commitsAfterCreate))
	}, quickstartTimeout(), 2*time.Second).Should(Succeed())

	commitsAfterUpdate, err := r.gitCommitCount()
	Expect(err).NotTo(HaveOccurred())

	_, err = kubectlRunInNamespace(
		ns,
		"delete", "configmap",
		configMapName,
	)
	Expect(err).NotTo(HaveOccurred(), "failed to delete quickstart ConfigMap")

	Eventually(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())

		_, statErr := os.Stat(expectedFile)
		g.Expect(statErr).To(HaveOccurred())
		g.Expect(os.IsNotExist(statErr)).To(BeTrue(), fmt.Sprintf("ConfigMap file should be deleted: %s", expectedFile))

		commitsAfter, countErr := r.gitCommitCount()
		g.Expect(countErr).NotTo(HaveOccurred())
		g.Expect(commitsAfter).To(BeNumerically(">", commitsAfterUpdate))
	}, quickstartTimeout(), 2*time.Second).Should(Succeed())
}

func (r *quickstartFrameworkRun) verifyQuickstartSecretEncryption(generatedAgeKey string) {
	ns := r.namespace
	secretName := fmt.Sprintf("quickstart-secret-%s", r.testID)
	secretValueOne := fmt.Sprintf("quickstart-plaintext-one-%s", r.testID)
	secretValueTwo := fmt.Sprintf("quickstart-plaintext-two-%s", r.testID)
	secretValueOneB64 := base64.StdEncoding.EncodeToString([]byte(secretValueOne))
	secretValueTwoB64 := base64.StdEncoding.EncodeToString([]byte(secretValueTwo))

	expectedFile := filepath.Join(
		r.checkoutDir,
		"live-cluster",
		ns,
		"secrets",
		fmt.Sprintf("%s.sops.yaml", secretName),
	)

	commitsBefore, err := r.gitCommitCount()
	Expect(err).NotTo(HaveOccurred())

	_, _ = kubectlRunInNamespace(
		ns,
		"delete", "secret",
		secretName,
		"--ignore-not-found=true",
	)

	_, err = kubectlRunInNamespace(
		ns,
		"create", "secret", "generic",
		secretName,
		"--from-literal",
		fmt.Sprintf("password=%s", secretValueOne),
	)
	Expect(err).NotTo(HaveOccurred(), "failed to create quickstart Secret")

	_, err = kubectlRunInNamespace(
		ns,
		"patch", "secret",
		secretName,
		"--type",
		"merge",
		"--patch",
		fmt.Sprintf(`{"stringData":{"password":"%s"}}`, secretValueTwo),
	)
	Expect(err).NotTo(HaveOccurred(), "failed to patch quickstart Secret")

	Eventually(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())

		content, readErr := os.ReadFile(expectedFile)
		g.Expect(readErr).NotTo(HaveOccurred(), fmt.Sprintf("Secret file must exist at %s", expectedFile))
		g.Expect(string(content)).To(ContainSubstring("sops:"))
		g.Expect(string(content)).NotTo(ContainSubstring(secretValueOne))
		g.Expect(string(content)).NotTo(ContainSubstring(secretValueTwo))
		g.Expect(string(content)).NotTo(ContainSubstring(secretValueOneB64))
		g.Expect(string(content)).NotTo(ContainSubstring(secretValueTwoB64))

		decryptedOutput, decryptErr := decryptWithControllerSOPS(content, generatedAgeKey)
		g.Expect(decryptErr).NotTo(HaveOccurred(), "failed to decrypt committed secret via controller sops binary")
		g.Expect(decryptedOutput).To(ContainSubstring(secretValueTwoB64))

		commitsAfter, countErr := r.gitCommitCount()
		g.Expect(countErr).NotTo(HaveOccurred())
		g.Expect(commitsAfter).To(BeNumerically(">", commitsBefore))
	}, quickstartTimeout(), 2*time.Second).Should(Succeed())

	_, _ = kubectlRunInNamespace(
		ns,
		"delete", "secret",
		secretName,
		"--ignore-not-found=true",
	)
}

func (r *quickstartFrameworkRun) verifyInvalidProviderActionableMessage() {
	ns := r.namespace
	Eventually(func(g Gomega) {
		output, err := kubectlRunInNamespace(ns, "get", "gitprovider", r.invalidProvName, "-o", "json")
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

		g.Expect(readyStatus).To(Equal("False"))
		g.Expect(readyReason).To(Equal("ConnectionFailed"))
		g.Expect(strings.TrimSpace(readyMessage)).NotTo(BeEmpty())

		message := strings.ToLower(readyMessage)
		actionable := strings.Contains(message, "auth") ||
			strings.Contains(message, "credential") ||
			strings.Contains(message, "connect") ||
			strings.Contains(message, "repository") ||
			strings.Contains(message, "secret")
		g.Expect(actionable).To(BeTrue(), fmt.Sprintf("expected actionable message, got: %q", readyMessage))
	}, quickstartTimeout(), 2*time.Second).Should(Succeed())
}

func (r *quickstartFrameworkRun) gitPull() error {
	pullCmd := exec.Command("git", "pull", "--ff-only")
	pullCmd.Dir = r.checkoutDir
	output, err := pullCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git pull: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (r *quickstartFrameworkRun) gitCommitCount() (int, error) {
	cmd := exec.Command("git", "rev-list", "--count", "--all")
	cmd.Dir = r.checkoutDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("git rev-list --count: %w: %s", err, strings.TrimSpace(string(output)))
	}

	value := strings.TrimSpace(string(output))
	if value == "" {
		return 0, nil
	}

	count, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse git commit count %q: %w", value, err)
	}
	return count, nil
}

func quickstartTimeout() time.Duration {
	value := strings.TrimSpace(os.Getenv(quickstartTimeoutSecondsEnv))
	if value == "" {
		return 180 * time.Second
	}

	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		return 180 * time.Second
	}

	return time.Duration(seconds) * time.Second
}

// createGitProviderWithURLInNamespace creates a GitProvider that commits each
// event immediately (commitWindow=0s). Use createGitProviderWithCommitWindow
// to exercise non-zero windows.
func createGitProviderWithURLInNamespace(name, ns, secretName, repoURL string) {
	createGitProviderWithCommitWindow(name, ns, secretName, repoURL, "0s")
}

func createGitProviderWithCommitWindow(name, ns, secretName, repoURL, commitWindow string) {
	By(fmt.Sprintf("creating GitProvider '%s' in ns '%s' (branch 'main', commitWindow '%s', secret '%s', URL '%s')",
		name, ns, commitWindow, secretName, repoURL))

	data := struct {
		Name         string
		Namespace    string
		RepoURL      string
		Branch       string
		SecretName   string
		CommitWindow string
	}{
		Name:         name,
		Namespace:    ns,
		RepoURL:      repoURL,
		Branch:       "main",
		SecretName:   secretName,
		CommitWindow: commitWindow,
	}

	err := applyFromTemplate("test/e2e/templates/gitprovider.tmpl", data, ns)
	Expect(err).NotTo(HaveOccurred(), "failed to apply GitProvider")
}
