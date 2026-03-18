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
	talkFrameworkEnabledEnv = "E2E_ENABLE_TALK_FRAMEWORK"
	talkDemoNamespace       = "vote"
	talkDemoPath            = "demo/vote-stack"
)

type talkDemoRun struct {
	repoName             string
	checkoutDir          string
	repoURL              string
	sourceNamespace      string
	gitSecretName        string
	providerName         string
	targetName           string
	watchRuleName        string
	clusterWatchRuleName string
	encryptionName       string
}

var _ = Describe("Talk Demo", Label("talk-demo"), Ordered, func() {
	var run talkDemoRun
	var testNs string

	BeforeAll(func() {
		if !talkFrameworkEnabled() {
			Skip(fmt.Sprintf(
				"talk demo is disabled; set %s=true to run",
				talkFrameworkEnabledEnv,
			))
		}

		run = newTalkDemoRun()

		By("creating test namespace and applying git secrets")
		testNs = testNamespaceFor("talk-demo")
		run.sourceNamespace = testNs
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists
		secretsYaml := strings.TrimSpace(os.Getenv("E2E_SECRETS_YAML"))
		Expect(secretsYaml).NotTo(BeEmpty(), "E2E_SECRETS_YAML must be set by BeforeSuite")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", secretsYaml)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)
	})

	It("prepares a reusable demo repository without cleanup", func() {
		By("asserting the talk demo repository checkout exists")
		run.setupGiteaRepository()

		By("copying Git credentials into the vote namespace")
		run.copyGitSecretToTalkNamespace()

		By("applying talk demo GitOps Reverser resources")
		run.applyTalkResources()

		By("verifying the talk demo resources become Ready")
		run.verifyTalkResourcesReady()

		By("seeding the demo repository with a few representative updates")
		run.seedTalkRepository()

		By("waiting for the initial snapshot to seed the demo repository")
		run.verifyTalkRepositorySeeded()

		By("printing demo artifacts for the presenter")
		run.logArtifacts()
	})
})

func talkFrameworkEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(talkFrameworkEnabledEnv)))
	return value == "1" || value == "true" || value == "yes"
}

func newTalkDemoRun() talkDemoRun {
	repoName := strings.TrimSpace(os.Getenv("E2E_REPO_NAME"))
	checkoutDir := strings.TrimSpace(os.Getenv("E2E_CHECKOUT_DIR"))
	sourceNamespace := resolveE2ENamespace()
	gitSecretName := e2eGitSecretHTTP()

	Expect(repoName).NotTo(
		BeEmpty(),
		"E2E_REPO_NAME must be set by the suite (make e2e-gitea-run-setup)",
	)
	Expect(checkoutDir).NotTo(
		BeEmpty(),
		"E2E_CHECKOUT_DIR must be set by the suite (make e2e-gitea-run-setup)",
	)

	repoURL := fmt.Sprintf(
		"http://gitea-http.gitea-e2e.svc.cluster.local:13000/testorg/%s.git",
		repoName,
	)

	return talkDemoRun{
		repoName:    repoName,
		checkoutDir: checkoutDir,
		repoURL:     repoURL,

		sourceNamespace:      sourceNamespace,
		gitSecretName:        gitSecretName,
		providerName:         "talk-demo-provider",
		targetName:           "talk-demo-target",
		watchRuleName:        "talk-demo-watch-all",
		clusterWatchRuleName: "talk-demo-cluster-resources",
		encryptionName:       "talk-demo-sops-age-key",
	}
}

func (r *talkDemoRun) setupGiteaRepository() {
	_, err := os.Stat(filepath.Join(r.checkoutDir, ".git"))
	Expect(err).NotTo(HaveOccurred(), "expected checkout to exist at E2E_CHECKOUT_DIR")
}

func (r *talkDemoRun) copyGitSecretToTalkNamespace() {
	output, err := kubectlRunInNamespace(r.sourceNamespace, "get", "secret", r.gitSecretName, "-o", "json")
	Expect(err).NotTo(HaveOccurred(), "failed to fetch source Git Secret")

	var secretObj map[string]interface{}
	Expect(json.Unmarshal([]byte(output), &secretObj)).To(Succeed())

	metadata, found, err := unstructured.NestedMap(secretObj, "metadata")
	Expect(err).NotTo(HaveOccurred(), "failed to read Secret metadata")
	Expect(found).To(BeTrue(), "expected Secret metadata")

	metadata["namespace"] = talkDemoNamespace
	delete(metadata, "creationTimestamp")
	delete(metadata, "generateName")
	delete(metadata, "managedFields")
	delete(metadata, "resourceVersion")
	delete(metadata, "selfLink")
	delete(metadata, "uid")

	if annotations, ok := metadata["annotations"].(map[string]interface{}); ok {
		delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
		if len(annotations) == 0 {
			delete(metadata, "annotations")
		}
	}

	secretObj["metadata"] = metadata
	delete(secretObj, "status")

	manifest, err := json.Marshal(secretObj)
	Expect(err).NotTo(HaveOccurred(), "failed to marshal copied Git Secret")

	_, err = kubectlRunWithStdin(talkDemoNamespace, string(manifest), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply copied Git Secret into vote namespace")
}

func (r *talkDemoRun) applyTalkResources() {
	createGitProviderWithURLInNamespace(r.providerName, talkDemoNamespace, r.gitSecretName, r.repoURL)

	createGitTargetWithEncryptionOptions(
		r.targetName,
		talkDemoNamespace,
		r.providerName,
		talkDemoPath,
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
		Namespace:       talkDemoNamespace,
		DestinationName: r.targetName,
	}

	err := applyFromTemplate("test/e2e/templates/talk-demo/watchrule-all.tmpl", watchRuleData, talkDemoNamespace)
	Expect(err).NotTo(HaveOccurred(), "failed to apply talk demo WatchRule")

	clusterWatchRuleData := struct {
		Name            string
		Namespace       string
		DestinationName string
	}{
		Name:            r.clusterWatchRuleName,
		Namespace:       talkDemoNamespace,
		DestinationName: r.targetName,
	}

	err = applyFromTemplate("test/e2e/templates/talk-demo/clusterwatchrule-talk.tmpl", clusterWatchRuleData, "")
	Expect(err).NotTo(HaveOccurred(), "failed to apply talk demo ClusterWatchRule")
}

func (r *talkDemoRun) verifyTalkResourcesReady() {
	verifyResourceStatus("gitprovider", r.providerName, talkDemoNamespace, "True", "Ready", "")
	verifyResourceStatus("gittarget", r.targetName, talkDemoNamespace, "True", "Ready", "")
	verifyResourceStatus("watchrule", r.watchRuleName, talkDemoNamespace, "True", "Ready", "")
	verifyResourceStatus("clusterwatchrule", r.clusterWatchRuleName, "", "True", "Ready", "")
}

func (r *talkDemoRun) seedTalkRepository() {
	value := strconv.FormatInt(time.Now().UnixNano(), 10)

	r.patchClusterResource(
		"namespace",
		talkDemoNamespace,
		fmt.Sprintf(`{"metadata":{"annotations":{"configbutler.ai/talk-demo-prepared-at":"%s"}}}`, value),
	)

	r.patchNamespacedResource(
		"gitprovider",
		r.providerName,
		fmt.Sprintf(`{"metadata":{"annotations":{"configbutler.ai/talk-demo-prepared-at":"%s"}}}`, value),
	)
	r.patchNamespacedResource(
		"gittarget",
		r.targetName,
		fmt.Sprintf(`{"metadata":{"annotations":{"configbutler.ai/talk-demo-prepared-at":"%s"}}}`, value),
	)
	r.patchNamespacedResource(
		"watchrule",
		r.watchRuleName,
		fmt.Sprintf(`{"metadata":{"annotations":{"configbutler.ai/talk-demo-prepared-at":"%s"}}}`, value),
	)
	r.patchClusterResource(
		"clusterwatchrule",
		r.clusterWatchRuleName,
		fmt.Sprintf(`{"metadata":{"annotations":{"configbutler.ai/talk-demo-prepared-at":"%s"}}}`, value),
	)

	r.patchNamespacedResource(
		"deployment",
		"vote-frontend",
		fmt.Sprintf(`{"metadata":{"annotations":{"configbutler.ai/talk-demo-prepared-at":"%s"}}}`, value),
	)
	r.patchNamespacedResource(
		"service",
		"vote-frontend",
		fmt.Sprintf(`{"metadata":{"annotations":{"configbutler.ai/talk-demo-prepared-at":"%s"}}}`, value),
	)
	r.patchNamespacedResource(
		"ingressroute",
		"frontend-static",
		fmt.Sprintf(`{"metadata":{"annotations":{"configbutler.ai/talk-demo-prepared-at":"%s"}}}`, value),
	)
	r.patchNamespacedResource(
		"quizsession",
		"kubecon-2026",
		fmt.Sprintf(`{"metadata":{"annotations":{"configbutler.ai/talk-demo-prepared-at":"%s"}}}`, value),
	)
}

func (r *talkDemoRun) patchClusterResource(resource, name, patch string) {
	_, err := kubectlRun(
		"patch",
		resource,
		name,
		"--type",
		"merge",
		"--patch",
		patch,
	)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to patch cluster resource %s/%s", resource, name))
}

func (r *talkDemoRun) patchNamespacedResource(resource, name, patch string) {
	_, err := kubectlRunInNamespace(
		talkDemoNamespace,
		"patch",
		resource,
		name,
		"--type",
		"merge",
		"--patch",
		patch,
	)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to patch namespaced resource %s/%s", resource, name))
}

func (r *talkDemoRun) verifyTalkRepositorySeeded() {
	expectedFiles := []string{
		filepath.Join(r.checkoutDir, talkDemoPath, ".sops.yaml"),
		filepath.Join(r.checkoutDir, talkDemoPath, "v1/namespaces/vote.yaml"),
		filepath.Join(r.checkoutDir, talkDemoPath, "apps/v1/deployments/vote/vote-frontend.yaml"),
		filepath.Join(r.checkoutDir, talkDemoPath, "v1/services/vote/vote-frontend.yaml"),
		filepath.Join(r.checkoutDir, talkDemoPath, "traefik.io/v1alpha1/ingressroutes/vote/frontend-static.yaml"),
		filepath.Join(
			r.checkoutDir,
			talkDemoPath,
			"configbutler.ai/v1alpha1/gitproviders/vote/talk-demo-provider.yaml",
		),
	}
	quizSubmissionPattern := filepath.Join(
		r.checkoutDir,
		talkDemoPath,
		"examples.configbutler.ai/v1alpha1/quizsubmissions/vote/*.yaml",
	)

	Eventually(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())

		commitCount, err := r.gitCommitCount()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(commitCount).To(BeNumerically(">", 0), "expected the demo repo to contain commits")

		for _, expectedFile := range expectedFiles {
			content, readErr := os.ReadFile(expectedFile)
			g.Expect(readErr).NotTo(HaveOccurred(), fmt.Sprintf("expected seeded file %s", expectedFile))
			g.Expect(content).NotTo(BeEmpty(), fmt.Sprintf("expected non-empty file %s", expectedFile))
		}
		quizSubmissionFiles, globErr := filepath.Glob(quizSubmissionPattern)
		g.Expect(globErr).NotTo(HaveOccurred())
		g.Expect(quizSubmissionFiles).NotTo(BeEmpty(), "expected at least one quizsubmission file in the demo repo")
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
}

func (r *talkDemoRun) logArtifacts() {
	_, _ = fmt.Fprintf(
		GinkgoWriter,
		"Talk demo ready:\n  repo=%s\n  checkout=%s\n  repoURL=%s\n  namespace=%s\n  path=%s\n",
		r.repoName,
		r.checkoutDir,
		r.repoURL,
		talkDemoNamespace,
		talkDemoPath,
	)
}

func (r *talkDemoRun) gitPull() error {
	pullCmd := exec.Command("git", "pull", "--ff-only")
	pullCmd.Dir = r.checkoutDir
	output, err := pullCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git pull: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (r *talkDemoRun) gitCommitCount() (int, error) {
	cmd := exec.Command("git", "rev-list", "--count", "--all")
	cmd.Dir = r.checkoutDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("git rev-list --count: %w: %s", err, strings.TrimSpace(string(output)))
	}

	var count int
	_, scanErr := fmt.Sscanf(strings.TrimSpace(string(output)), "%d", &count)
	if scanErr != nil {
		return 0, fmt.Errorf("parse git commit count %q: %w", strings.TrimSpace(string(output)), scanErr)
	}
	return count, nil
}
