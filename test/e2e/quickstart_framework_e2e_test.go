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

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

const (
	quickstartFrameworkEnabledEnv = "E2E_ENABLE_QUICKSTART_FRAMEWORK"
	quickstartFrameworkModeEnv    = "E2E_QUICKSTART_MODE"
	quickstartSetupNamespaceEnv   = "QUICKSTART_NAMESPACE"
	quickstartTimeoutSecondsEnv   = "QUICKSTART_TIMEOUT_SECONDS"
)

type quickstartFrameworkRun struct {
	mode            string
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
	})

	It("sets up quickstart flow via Go framework", func() {
		By("creating dedicated Gitea repository and bootstrap credentials")
		run.setupGiteaRepository()

		By("applying quickstart resources from Go")
		run.applyQuickstartResources()

		By("verifying quickstart resources become Ready")
		run.verifyQuickstartResourcesReady()

		By("verifying generated encryption secret and commit flow")
		generatedAgeKey := run.verifyGeneratedEncryptionSecret()

		By("verifying quickstart commits for create/update/delete")
		run.verifyQuickstartConfigMapCommits()

		By("verifying quickstart encrypted Secret commit is decryptable")
		run.verifyQuickstartSecretEncryption(generatedAgeKey)

		By("verifying invalid credentials provider shows actionable message")
		run.verifyInvalidProviderActionableMessage()
	})

})

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
	mode := quickstartFrameworkMode()
	testID := strconv.FormatInt(time.Now().UnixNano(), 10)
	repoName := fmt.Sprintf("quickstart-framework-%s-%s", mode, testID)
	checkoutDir := filepath.Join("/tmp/gitops-reverser", repoName)

	return quickstartFrameworkRun{
		mode:            mode,
		testID:          testID,
		repoName:        repoName,
		checkoutDir:     checkoutDir,
		repoURL:         fmt.Sprintf("http://gitea-http.gitea-e2e.svc.cluster.local:13000/testorg/%s.git", repoName),
		providerName:    fmt.Sprintf("quickstart-provider-%s", testID),
		targetName:      fmt.Sprintf("quickstart-target-%s", testID),
		watchRuleName:   fmt.Sprintf("quickstart-watchrule-%s", testID),
		invalidProvName: fmt.Sprintf("quickstart-invalid-provider-%s", testID),
		encryptionName:  fmt.Sprintf("quickstart-sops-age-key-%s", testID),
	}
}

func (r *quickstartFrameworkRun) setupGiteaRepository() {
	cmd := exec.Command("bash", "test/e2e/scripts/setup-gitea.sh", r.repoName, r.checkoutDir)
	projectDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred(), "failed to resolve project directory for quickstart")

	cmd.Dir = projectDir
	cmd.Env = append(
		os.Environ(),
		"GO111MODULE=on",
		fmt.Sprintf("SUT_NAMESPACE=%s", quickstartSetupNamespace()),
	)
	output, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to bootstrap gitea repository for quickstart: %s", output))
}

func (r *quickstartFrameworkRun) applyQuickstartResources() {
	qsNamespace := quickstartSetupNamespace()
	createGitProviderWithURLInNamespace(r.providerName, qsNamespace, "main", "git-creds", r.repoURL)

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

	createGitProviderWithURLInNamespace(r.invalidProvName, qsNamespace, "main", "git-creds-invalid", r.repoURL)
}

func (r *quickstartFrameworkRun) verifyQuickstartResourcesReady() {
	ns := quickstartSetupNamespace()
	verifyResourceStatus("gitprovider", r.providerName, ns, "True", "Ready", "")
	verifyResourceStatus("gittarget", r.targetName, ns, "True", "Ready", "")
	verifyResourceStatus("watchrule", r.watchRuleName, ns, "True", "Ready", "")
}

func (r *quickstartFrameworkRun) verifyGeneratedEncryptionSecret() string {
	ns := quickstartSetupNamespace()
	var generatedAgeKey string

	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "secret", r.encryptionName, "-n", ns, "-o", "json")
		output, err := utils.Run(cmd)
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
	ns := quickstartSetupNamespace()
	configMapName := fmt.Sprintf("quickstart-config-%s", r.testID)
	expectedFile := filepath.Join(
		r.checkoutDir,
		"live-cluster",
		"v1",
		"configmaps",
		ns,
		fmt.Sprintf("%s.yaml", configMapName),
	)

	_, _ = utils.Run(exec.Command(
		"kubectl",
		"delete",
		"configmap",
		configMapName,
		"-n",
		ns,
		"--ignore-not-found=true",
	))

	commitsBefore, err := r.gitCommitCount()
	Expect(err).NotTo(HaveOccurred())

	_, err = utils.Run(exec.Command(
		"kubectl",
		"create",
		"configmap",
		configMapName,
		"--from-literal=value=one",
		"-n",
		ns,
	))
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

	_, err = utils.Run(exec.Command(
		"kubectl",
		"patch",
		"configmap",
		configMapName,
		"-n",
		ns,
		"--type",
		"merge",
		"--patch",
		`{"data":{"value":"two"}}`,
	))
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

	_, err = utils.Run(exec.Command(
		"kubectl",
		"delete",
		"configmap",
		configMapName,
		"-n",
		ns,
	))
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
	ns := quickstartSetupNamespace()
	secretName := fmt.Sprintf("quickstart-secret-%s", r.testID)
	secretValueOne := fmt.Sprintf("quickstart-plaintext-one-%s", r.testID)
	secretValueTwo := fmt.Sprintf("quickstart-plaintext-two-%s", r.testID)
	secretValueOneB64 := base64.StdEncoding.EncodeToString([]byte(secretValueOne))
	secretValueTwoB64 := base64.StdEncoding.EncodeToString([]byte(secretValueTwo))

	expectedFile := filepath.Join(
		r.checkoutDir,
		"live-cluster",
		"v1",
		"secrets",
		ns,
		fmt.Sprintf("%s.sops.yaml", secretName),
	)

	commitsBefore, err := r.gitCommitCount()
	Expect(err).NotTo(HaveOccurred())

	_, _ = utils.Run(exec.Command(
		"kubectl",
		"delete",
		"secret",
		secretName,
		"-n",
		ns,
		"--ignore-not-found=true",
	))

	_, err = utils.Run(exec.Command(
		"kubectl",
		"create",
		"secret",
		"generic",
		secretName,
		"--from-literal",
		fmt.Sprintf("password=%s", secretValueOne),
		"-n",
		ns,
	))
	Expect(err).NotTo(HaveOccurred(), "failed to create quickstart Secret")

	_, err = utils.Run(exec.Command(
		"kubectl",
		"patch",
		"secret",
		secretName,
		"-n",
		ns,
		"--type",
		"merge",
		"--patch",
		fmt.Sprintf(`{"stringData":{"password":"%s"}}`, secretValueTwo),
	))
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

	_, _ = utils.Run(exec.Command(
		"kubectl",
		"delete",
		"secret",
		secretName,
		"-n",
		ns,
		"--ignore-not-found=true",
	))
}

func (r *quickstartFrameworkRun) verifyInvalidProviderActionableMessage() {
	ns := quickstartSetupNamespace()
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "gitprovider", r.invalidProvName, "-n", ns, "-o", "json")
		output, err := utils.Run(cmd)
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

func createGitProviderWithURLInNamespace(name, ns, branch, secretName, repoURL string) {
	By(fmt.Sprintf("creating GitProvider '%s' in ns '%s' with branch '%s', secret '%s' and URL '%s'",
		name, ns, branch, secretName, repoURL))

	data := struct {
		Name       string
		Namespace  string
		RepoURL    string
		Branch     string
		SecretName string
	}{
		Name:       name,
		Namespace:  ns,
		RepoURL:    repoURL,
		Branch:     branch,
		SecretName: secretName,
	}

	err := applyFromTemplate("test/e2e/templates/gitprovider.tmpl", data, ns)
	Expect(err).NotTo(HaveOccurred(), "failed to apply GitProvider")
}

func quickstartSetupNamespace() string {
	if value := strings.TrimSpace(os.Getenv(quickstartSetupNamespaceEnv)); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("SUT_NAMESPACE")); value != "" {
		return value
	}
	return "sut"
}
