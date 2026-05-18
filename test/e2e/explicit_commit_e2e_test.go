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

// The ExplicitCommit suite exercises the "save" signal: an ExplicitCommit
// object finalizes the open commit window for a GitTarget immediately, instead
// of waiting for the rolling silence timer. The GitProvider is configured with
// a deliberately long commitWindow so that, without the ExplicitCommit, the
// edit would not be committed for minutes — observing the commit promptly is
// what proves the explicit-commit path works.
var _ = Describe("Explicit Commit", Label("explicit-commit", "audit-redis", "smoke"), Ordered, func() {
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
		By("creating explicit-commit test namespace and applying git secrets")
		testNs = testNamespaceFor("explicit-commit")
		_, _ = kubectlRun("create", "namespace", testNs)
		repo := ensureAuditRedisRepo()
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to namespace")
		applySOPSAgeKeyToNamespace(testNs)

		seed := GinkgoRandomSeed()
		gitProvName = fmt.Sprintf("explicit-commit-gitprovider-%d", seed)
		gitTargetName = fmt.Sprintf("explicit-commit-gittarget-%d", seed)
		watchRuleName = fmt.Sprintf("explicit-commit-watchrule-%d", seed)

		By(fmt.Sprintf("creating GitProvider with commitWindow=%s", commitWindow))
		createGitProviderWithCommitWindow(gitProvName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP, commitWindow)
		verifyResourceStatus("gitprovider", gitProvName, testNs, "True", "Ready", "")

		createGitTarget(gitTargetName, testNs, gitProvName, "e2e/explicit-commit-test", "main")
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
		basePath := "e2e/explicit-commit-test"
		seed := GinkgoRandomSeed()
		cmName := fmt.Sprintf("explicit-commit-cm-%d", seed)
		explicitCommitName := fmt.Sprintf("explicit-commit-save-%d", seed)
		message := fmt.Sprintf("save: explicit commit from e2e seed %d", seed)

		By("editing a ConfigMap to open a commit window")
		applyConfigMap(testNs, cmName)

		By("waiting for the ConfigMap audit event to reach the Redis stream")
		// Confirming the producer side guarantees the edit is observable by the
		// consumer before the ExplicitCommit's own (later) audit event.
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

		By("creating an ExplicitCommit to finalize the open window now")
		applyExplicitCommit(testNs, explicitCommitName, gitTargetName, message)

		By("waiting for the ExplicitCommit to reach the Committed phase")
		var reportedSHA string
		Eventually(func(g Gomega) {
			phase := explicitCommitField(g, testNs, explicitCommitName, "{.status.phase}")
			g.Expect(phase).To(Equal("Committed"),
				"ExplicitCommit should finalize the window and report Committed")

			reportedSHA = explicitCommitField(g, testNs, explicitCommitName, "{.status.sha}")
			g.Expect(reportedSHA).NotTo(BeEmpty(), "status.sha should be populated")

			branch := explicitCommitField(g, testNs, explicitCommitName, "{.status.branch}")
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

		By("cleaning up the test ConfigMap and ExplicitCommit")
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
		_, _ = kubectlRunInNamespace(testNs, "delete", "explicitcommit", explicitCommitName, "--ignore-not-found=true")
	})
})

// applyExplicitCommit creates an ExplicitCommit object that targets the given
// GitTarget with an optional commit message.
func applyExplicitCommit(namespace, name, gitTargetName, message string) {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha1
kind: ExplicitCommit
metadata:
  name: %s
  namespace: %s
spec:
  gitTargetRef:
    name: %s
  message: %q
`, name, namespace, gitTargetName, message)
	_, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to apply ExplicitCommit %s/%s", namespace, name))
}

// explicitCommitField reads a jsonpath field off an ExplicitCommit object.
func explicitCommitField(g Gomega, namespace, name, jsonPath string) string {
	out, err := kubectlRunInNamespace(namespace, "get", "explicitcommit", name,
		"-o", "jsonpath="+jsonPath)
	g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to read %s of ExplicitCommit %s", jsonPath, name))
	return strings.TrimSpace(out)
}
