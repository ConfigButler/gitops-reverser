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
	demoEnabledEnv = "E2E_ENABLE_DEMO"
	demoNamespace  = "vote"
	demoPath       = "adam/rai/8f"
)

type demoRun struct {
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

// demoRepo holds the file-local repo fixtures for the Demo describe block.
// REPO_NAME=demo and TESTNAMESPACE=vote are fixed for demo so the repo is reusable
// for manual inspection after the run.
var demoRepo *RepoArtifacts

var _ = Describe("Demo", Label("demo"), Ordered, func() {
	var run demoRun
	var testNs string

	BeforeAll(func() {
		if !demoEnabled() {
			Skip(fmt.Sprintf(
				"demo is disabled; set %s=true to run",
				demoEnabledEnv,
			))
		}

		By("creating demo test namespace")
		testNs = testNamespaceFor("demo") // returns "vote" when TESTNAMESPACE=vote
		_, _ = kubectlRun("create", "namespace", testNs)

		By("setting up fixed demo Gitea repo")
		demoRepo = SetupRepo(resolveE2EContext(), testNs, "demo")

		By("applying git secrets to demo namespace")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", demoRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to demo namespace")
		applySOPSAgeKeyToNamespace(testNs)

		run = newDemoRun()
		run.sourceNamespace = testNs
	})

	It("prepares a reusable demo repository without cleanup", func() {
		By("asserting the demo repository checkout exists")
		run.setupRepository()

		By("copying Git credentials into the vote namespace")
		run.copyGitSecret()

		By("applying demo GitOps Reverser resources")
		run.applyResources()

		By("verifying the demo resources become Ready")
		run.verifyResourcesReady()

		By("seeding the demo repository with a few representative updates")
		run.seedRepository()

		By("waiting for the initial snapshot to seed the demo repository")
		run.verifyRepositorySeeded()

		By("printing demo artifacts for the presenter")
		run.logArtifacts()
	})
})

func demoEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(demoEnabledEnv)))
	return value == "1" || value == "true" || value == "yes"
}

func newDemoRun() demoRun {
	return demoRun{
		repoName:             demoRepo.RepoName,
		checkoutDir:          demoRepo.CheckoutDir,
		repoURL:              demoRepo.RepoURLHTTP,
		sourceNamespace:      resolveE2ENamespace(),
		gitSecretName:        demoRepo.GitSecretHTTP,
		providerName:         "demo-provider-" + demoRepo.RepoName,
		targetName:           "demo-target-" + demoRepo.RepoName,
		watchRuleName:        "demo-watch-all-" + demoRepo.RepoName,
		clusterWatchRuleName: "demo-cluster-resources-" + demoRepo.RepoName,
		encryptionName:       "demo-sops-age-key",
	}
}

func (r *demoRun) setupRepository() {
	_, err := os.Stat(filepath.Join(r.checkoutDir, ".git"))
	Expect(err).NotTo(HaveOccurred(), "expected checkout to exist")
}

func (r *demoRun) copyGitSecret() {
	output, err := kubectlRunInNamespace(r.sourceNamespace, "get", "secret", r.gitSecretName, "-o", "json")
	Expect(err).NotTo(HaveOccurred(), "failed to fetch source Git Secret")

	var secretObj map[string]interface{}
	Expect(json.Unmarshal([]byte(output), &secretObj)).To(Succeed())

	metadata, found, err := unstructured.NestedMap(secretObj, "metadata")
	Expect(err).NotTo(HaveOccurred(), "failed to read Secret metadata")
	Expect(found).To(BeTrue(), "expected Secret metadata")

	metadata["namespace"] = demoNamespace
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

	_, err = kubectlRunWithStdin(demoNamespace, string(manifest), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply copied Git Secret into vote namespace")
}

func (r *demoRun) applyResources() {
	createGitProviderWithURLInNamespace(r.providerName, demoNamespace, r.gitSecretName, r.repoURL)

	createGitTargetWithEncryptionOptions(
		r.targetName,
		demoNamespace,
		r.providerName,
		demoPath,
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
		Namespace:       demoNamespace,
		DestinationName: r.targetName,
	}

	err := applyFromTemplate("test/e2e/templates/demo/watchrule-all.tmpl", watchRuleData, demoNamespace)
	Expect(err).NotTo(HaveOccurred(), "failed to apply demo WatchRule")

	clusterWatchRuleData := struct {
		Name            string
		Namespace       string
		DestinationName string
	}{
		Name:            r.clusterWatchRuleName,
		Namespace:       demoNamespace,
		DestinationName: r.targetName,
	}

	err = applyFromTemplate("test/e2e/templates/demo/clusterwatchrule-demo.tmpl", clusterWatchRuleData, "")
	Expect(err).NotTo(HaveOccurred(), "failed to apply demo ClusterWatchRule")
}

func (r *demoRun) verifyResourcesReady() {
	verifyResourceStatus("gitprovider", r.providerName, demoNamespace, "True", "Ready", "")
	verifyResourceStatus("gittarget", r.targetName, demoNamespace, "True", "Ready", "")
	verifyResourceStatus("watchrule", r.watchRuleName, demoNamespace, "True", "Ready", "")
	verifyResourceStatus("clusterwatchrule", r.clusterWatchRuleName, "", "True", "Ready", "")
}

func (r *demoRun) seedRepository() {
	value := strconv.FormatInt(time.Now().UnixNano(), 10)
	r.patchClusterResource(
		"namespace",
		demoNamespace,
		fmt.Sprintf(`{"metadata":{"annotations":{"configbutler.ai/demo-prepared-at":"%s"}}}`, value),
	)
}

func (r *demoRun) patchClusterResource(resource, name, patch string) {
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

func (r *demoRun) verifyRepositorySeeded() {
	expectedFiles := []string{
		filepath.Join(r.checkoutDir, demoPath, ".sops.yaml"),
		filepath.Join(r.checkoutDir, demoPath, "v1/namespaces/vote.yaml"),
		filepath.Join(r.checkoutDir, demoPath, "apps/v1/deployments/vote/vote-frontend.yaml"),
		filepath.Join(r.checkoutDir, demoPath, "v1/services/vote/vote-frontend.yaml"),
		filepath.Join(r.checkoutDir, demoPath, "traefik.io/v1alpha1/ingressroutes/vote/frontend-static.yaml"),
		filepath.Join(
			r.checkoutDir,
			demoPath,
			"configbutler.ai/v1alpha1/gitproviders/vote",
			r.providerName+".yaml",
		),
	}
	quizSubmissionPattern := filepath.Join(
		r.checkoutDir,
		demoPath,
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

func (r *demoRun) logArtifacts() {
	_, _ = fmt.Fprintf(
		GinkgoWriter,
		"Demo ready:\n  repo=%s\n  checkout=%s\n  repoURL=%s\n  namespace=%s\n  path=%s\n",
		r.repoName,
		r.checkoutDir,
		r.repoURL,
		demoNamespace,
		demoPath,
	)
}

func (r *demoRun) gitPull() error {
	pullCmd := exec.Command("git", "pull", "--ff-only")
	pullCmd.Dir = r.checkoutDir
	output, err := pullCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git pull: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (r *demoRun) gitCommitCount() (int, error) {
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
