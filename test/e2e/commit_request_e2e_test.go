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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The CommitRequest suite exercises the "save" signal: a CommitRequest
// object finalizes the open commit window for a GitTarget immediately, instead
// of waiting for the rolling silence timer. The GitProvider is configured with
// a deliberately long commitWindow so that, without the CommitRequest, the
// edit would not be committed for minutes — observing the commit promptly is
// what proves the commit-request path works.
var _ = Describe("Commit Request", Label("commit-request", "audit-redis", "smoke"), Ordered, func() {
	var (
		testNs        string
		gitProvName   string
		gitTargetName string
		watchRuleName string
	)

	// commitWindow is long enough that the silence timer cannot be what
	// produces the commit within the assertion timeout below.
	const commitWindow = "300s"

	BeforeAll(func() {
		By("creating commit-request test namespace and applying git secrets")
		testNs = testNamespaceFor("commit-request")
		_, _ = kubectlRun("create", "namespace", testNs)
		repo := ensureAuditRedisRepo()
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to namespace")
		applySOPSAgeKeyToNamespace(testNs)

		seed := GinkgoRandomSeed()
		gitProvName = fmt.Sprintf("commit-request-gitprovider-%d", seed)
		gitTargetName = fmt.Sprintf("commit-request-gittarget-%d", seed)
		watchRuleName = fmt.Sprintf("commit-request-watchrule-%d", seed)

		By(fmt.Sprintf("creating GitProvider with commitWindow=%s", commitWindow))
		createGitProviderWithCommitWindow(gitProvName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP, commitWindow)
		verifyResourceStatus("gitprovider", gitProvName, testNs, "True", "Ready", "")

		createGitTarget(gitTargetName, testNs, gitProvName, "e2e/commit-request-test", "main")
		verifyResourceStatus("gittarget", gitTargetName, testNs, "True", "Ready", "")

		watchRuleData := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{
			Name:            watchRuleName,
			Namespace:       testNs,
			DestinationName: gitTargetName,
		}
		err = applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", watchRuleData, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to apply WatchRule")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
	})

	AfterAll(func() {
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(gitTargetName, testNs)
		cleanupNamespacedResource(testNs, "gitprovider", gitProvName)
		cleanupNamespace(testNs)
	})

	It("finalizes the open commit window on demand and reports the resulting SHA", func() {
		repo := auditRedisRepo
		basePath := "e2e/commit-request-test"
		seed := GinkgoRandomSeed()
		cmName := fmt.Sprintf("commit-request-cm-%d", seed)
		commitRequestName := fmt.Sprintf("commit-request-save-%d", seed)
		message := fmt.Sprintf("save: commit request from e2e seed %d", seed)

		By("editing a ConfigMap to open a commit window")
		applyConfigMap(testNs, cmName)

		By("waiting for the ConfigMap audit event to reach the Redis stream")
		// Confirming the producer side guarantees the edit is observable by the
		// consumer before the CommitRequest's own (later) audit event.
		valkeyClient := newE2EValkeyClient()
		defer func() { _ = valkeyClient.Close() }()
		Eventually(func(g Gomega) {
			entries, readErr := valkeyClient.XRange(context.Background(), defaultAuditRedisStream, "-", "+").Result()
			g.Expect(readErr).NotTo(HaveOccurred())
			found := false
			for _, entry := range entries {
				resource, _ := entry.Values["resource"].(string)
				name, _ := entry.Values["name"].(string)
				if resource == "configmaps" && name == cmName {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "ConfigMap audit event should appear in the stream")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("confirming the edit has NOT been committed yet (long commitWindow)")
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			expected := filepath.Join(repo.CheckoutDir, basePath, "v1", "configmaps", testNs, cmName+".yaml")
			_, statErr := os.Stat(expected)
			g.Expect(statErr).To(HaveOccurred(), "edit should still be pending inside the open commit window")
		}, 10*time.Second, 2*time.Second).Should(Succeed())

		By("creating a CommitRequest to finalize the open window now")
		applyCommitRequest(testNs, commitRequestName, gitTargetName, message)

		By("waiting for the CommitRequest to reach the Committed phase")
		var reportedSHA string
		Eventually(func(g Gomega) {
			phase := commitRequestField(g, testNs, commitRequestName, "{.status.phase}")
			g.Expect(phase).To(Equal("Committed"),
				"CommitRequest should finalize the window and report Committed")

			reportedSHA = commitRequestField(g, testNs, commitRequestName, "{.status.sha}")
			g.Expect(reportedSHA).NotTo(BeEmpty(), "status.sha should be populated")

			branch := commitRequestField(g, testNs, commitRequestName, "{.status.branch}")
			g.Expect(branch).To(Equal("main"))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("verifying the commit landed in Git with the explicit message and the edited file")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			expectedFile := filepath.Join(repo.CheckoutDir, basePath, "v1", "configmaps", testNs, cmName+".yaml")
			info, statErr := os.Stat(expectedFile)
			g.Expect(statErr).NotTo(HaveOccurred(), fmt.Sprintf("ConfigMap file should exist at %s", expectedFile))
			g.Expect(info.Size()).To(BeNumerically(">", 0))

			subject, logErr := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%B")
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(subject)).To(Equal(message),
				"the explicit spec.message should be used verbatim as the commit message")

			headSHA, shaErr := gitRun(repo.CheckoutDir, "rev-parse", "HEAD")
			g.Expect(shaErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(headSHA)).To(Equal(reportedSHA),
				"status.sha should match the SHA of the commit on the branch")
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up the test ConfigMap and CommitRequest")
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
		_, _ = kubectlRunInNamespace(testNs, "delete", "commitrequest", commitRequestName, "--ignore-not-found=true")
	})

	// generateName creates surface the bug described in
	// docs/tasks/generated-name-support.md: the audit objectRef.name is empty
	// for collection POSTs, so the consumer must resolve the generated name
	// from responseObject. Without the fix the CommitRequest stays stuck in
	// WaitingForAuditEvent forever.
	It("finalizes a CommitRequest created with metadata.generateName", func() {
		repo := auditRedisRepo
		basePath := "e2e/commit-request-test"
		seed := GinkgoRandomSeed()
		cmName := fmt.Sprintf("commit-request-gen-cm-%d", seed)
		commitRequestPrefix := fmt.Sprintf("commit-request-gen-%d-", seed)
		message := fmt.Sprintf("save: generateName commit request from e2e seed %d", seed)

		By("editing a ConfigMap to open a commit window")
		applyConfigMap(testNs, cmName)

		By("creating a CommitRequest with metadata.generateName")
		generatedName := applyCommitRequestWithGenerateName(testNs, commitRequestPrefix, gitTargetName, message)

		By("waiting for the generated-name CommitRequest to reach Committed")
		var reportedSHA string
		Eventually(func(g Gomega) {
			phase := commitRequestField(g, testNs, generatedName, "{.status.phase}")
			g.Expect(phase).To(Equal("Committed"),
				"a CommitRequest created via generateName must reach Committed")

			reportedSHA = commitRequestField(g, testNs, generatedName, "{.status.sha}")
			g.Expect(reportedSHA).NotTo(BeEmpty(), "status.sha should be populated")
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("verifying the commit landed in Git with the explicit message")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			expectedFile := filepath.Join(repo.CheckoutDir, basePath, "v1", "configmaps", testNs, cmName+".yaml")
			info, statErr := os.Stat(expectedFile)
			g.Expect(statErr).NotTo(HaveOccurred(), fmt.Sprintf("ConfigMap file should exist at %s", expectedFile))
			g.Expect(info.Size()).To(BeNumerically(">", 0))

			subject, logErr := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%B")
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(subject)).To(Equal(message),
				"the explicit spec.message should be used verbatim")
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up the generateName ConfigMap and CommitRequest")
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
		_, _ = kubectlRunInNamespace(testNs, "delete", "commitrequest", generatedName, "--ignore-not-found=true")
	})
})

// applyCommitRequestWithGenerateName creates a CommitRequest using
// metadata.generateName and returns the server-allocated name.
func applyCommitRequestWithGenerateName(namespace, prefix, gitTargetName, message string) string {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha1
kind: CommitRequest
metadata:
  generateName: %s
  namespace: %s
spec:
  gitTargetRef:
    name: %s
  message: %q
`, prefix, namespace, gitTargetName, message)
	out, err := kubectlRunWithStdin(namespace, manifest,
		"create", "-f", "-", "-o", "jsonpath={.metadata.name}")
	Expect(err).NotTo(HaveOccurred(),
		fmt.Sprintf("failed to create CommitRequest with generateName=%s", prefix))
	name := strings.TrimSpace(out)
	Expect(name).NotTo(BeEmpty(), "kubectl create must return the server-allocated name")
	Expect(name).To(HavePrefix(prefix), "the allocated name must start with the requested prefix")
	return name
}

// applyCommitRequest creates a CommitRequest object that targets the given
// GitTarget with an optional commit message.
func applyCommitRequest(namespace, name, gitTargetName, message string) {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha1
kind: CommitRequest
metadata:
  name: %s
  namespace: %s
spec:
  gitTargetRef:
    name: %s
  message: %q
`, name, namespace, gitTargetName, message)
	_, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to apply CommitRequest %s/%s", namespace, name))
}

// commitRequestField reads a jsonpath field off a CommitRequest object.
func commitRequestField(g Gomega, namespace, name, jsonPath string) string {
	out, err := kubectlRunInNamespace(namespace, "get", "commitrequest", name,
		"-o", "jsonpath="+jsonPath)
	g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to read %s of CommitRequest %s", jsonPath, name))
	return strings.TrimSpace(out)
}
