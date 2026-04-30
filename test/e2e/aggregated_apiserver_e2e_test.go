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
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe("Aggregated API server", Label("aggregated-api"), Ordered, func() {
	var (
		testNs         string
		repo           *RepoArtifacts
		providerName   string
		targetName     string
		watchRuleName  string
		aggregatedPath string
	)

	BeforeAll(func() {
		testNs = testNamespaceFor("aggregated-api")
		providerName = "aggregated-api-provider"
		targetName = "aggregated-api-target"
		watchRuleName = "aggregated-api-watchrule"
		aggregatedPath = "e2e/aggregated-api"

		_, _ = kubectlRun("create", "namespace", testNs)

		repo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-aggregated-api-%d", GinkgoRandomSeed()),
		)

		By("applying git secrets to aggregated-api test namespace")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to aggregated-api namespace")
		applySOPSAgeKeyToNamespace(testNs)

		By("creating GitProvider and GitTarget for aggregated-api scenarios")
		createGitProviderWithURLInNamespace(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")

		createGitTarget(targetName, testNs, providerName, aggregatedPath, "main")
		verifyResourceStatus("gittarget", targetName, testNs, "True", "Ready", "")
	})

	ensureFlunderWatchRuleReady := func() {
		By("applying a WatchRule for wardle flunders")
		err := applyFromTemplate(
			"test/e2e/templates/aggregated-api/watchrule-flunder.tmpl",
			struct {
				Name          string
				Namespace     string
				GitTargetName string
			}{
				Name:          watchRuleName,
				Namespace:     testNs,
				GitTargetName: targetName,
			},
			testNs,
		)
		Expect(err).NotTo(HaveOccurred(), "failed to apply aggregated-api WatchRule")

		By("verifying the WatchRule reaches Ready=True")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
	}

	AfterAll(func() {
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(targetName, testNs)
		cleanupNamespacedResource(testNs, "gitprovider", providerName)
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("should install and serve flunders through the aggregation layer", Label("smoke"), func() {
		By("waiting for the wardle APIService to report available")
		Eventually(func(g Gomega) {
			output, err := kubectlRun(
				"get",
				"apiservice",
				"v1alpha1.wardle.example.com",
				"-o",
				"jsonpath={.status.conditions[?(@.type=='Available')].status}",
			)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(Equal("True"))
		}, 180*time.Second, 2*time.Second).Should(Succeed())

		By("verifying wardle resources are discoverable")
		Eventually(func(g Gomega) {
			output, err := kubectlRun("api-resources", "--api-group=wardle.example.com")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("flunders"))
			g.Expect(output).To(ContainSubstring("fischers"))
		}, 30*time.Second, time.Second).Should(Succeed())

		flunderName := fmt.Sprintf("install-smoke-%d", GinkgoRandomSeed())
		flunderManifest := fmt.Sprintf(`apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: %s
  namespace: %s
spec:
  reference: smoke-reference
`, flunderName, testNs)

		By("creating a Flunder via the aggregated API")
		_, err := kubectlRunWithStdin(testNs, flunderManifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())

		DeferCleanup(func() {
			if skipCleanupBecauseResourcesArePreserved(fmt.Sprintf("Flunder %s/%s", testNs, flunderName), testNs) {
				return
			}
			_, _ = kubectlRunInNamespace(testNs, "delete", "flunder", flunderName, "--ignore-not-found=true")
		})

		By("reading the Flunder back through kubectl discovery")
		Eventually(func(g Gomega) {
			output, err := kubectlRunInNamespace(testNs, "get", "flunder", flunderName, "-o", "json")
			g.Expect(err).NotTo(HaveOccurred())

			var obj unstructured.Unstructured
			g.Expect(json.Unmarshal([]byte(stripKubectlWarnings(output)), &obj.Object)).To(Succeed())

			g.Expect(obj.GetAPIVersion()).To(Equal("wardle.example.com/v1alpha1"))
			g.Expect(obj.GetKind()).To(Equal("Flunder"))
			g.Expect(obj.GetName()).To(Equal(flunderName))
			g.Expect(obj.GetNamespace()).To(Equal(testNs))

			reference, found, nestedErr := unstructured.NestedString(obj.Object, "spec", "reference")
			g.Expect(nestedErr).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(reference).To(Equal("smoke-reference"))
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	It("should recover Flunder audit request and response bodies through the proxy", func() {
		streamName := defaultAuditRedisStream
		configMapName := fmt.Sprintf("aggregated-audit-compare-cm-%d", GinkgoRandomSeed())
		flunderName := fmt.Sprintf("aggregated-audit-compare-flunder-%d", GinkgoRandomSeed())

		By("connecting to Valkey through the e2e port-forward")
		client := newE2EValkeyClient()
		defer func() {
			_ = client.Close()
		}()

		Eventually(func(g Gomega) {
			g.Expect(client.Ping(context.Background()).Err()).NotTo(HaveOccurred())
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("recording the latest audit stream entry before creating comparison resources")
		baselineID, err := latestAuditStreamID(context.Background(), client, streamName)
		Expect(err).NotTo(HaveOccurred())

		configMapManifest := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  compare: configmap
`, configMapName, testNs)

		flunderManifest := fmt.Sprintf(`apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: %s
  namespace: %s
spec:
  reference: compare-reference
`, flunderName, testNs)

		By("creating a ConfigMap and Flunder via the same kubectl apply path")
		_, err = kubectlRunWithStdin(testNs, configMapManifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		_, err = kubectlRunWithStdin(testNs, flunderManifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())

		DeferCleanup(func() {
			if skipCleanupBecauseResourcesArePreserved(
				fmt.Sprintf("ConfigMap %s/%s", testNs, configMapName),
				testNs,
			) {
				return
			}
			_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", configMapName, "--ignore-not-found=true")
		})
		DeferCleanup(func() {
			if skipCleanupBecauseResourcesArePreserved(fmt.Sprintf("Flunder %s/%s", testNs, flunderName), testNs) {
				return
			}
			_, _ = kubectlRunInNamespace(testNs, "delete", "flunder", flunderName, "--ignore-not-found=true")
		})

		By("capturing the corresponding raw audit payloads from the Valkey stream")
		Eventually(func(g Gomega) {
			configMapAudit, findErr := findAuditPayloadSince(
				context.Background(),
				client,
				streamName,
				baselineID,
				300,
				func(payload map[string]interface{}) bool {
					return auditPayloadMatches(payload, "", "configmaps", testNs, configMapName, "create")
				},
			)
			g.Expect(findErr).NotTo(HaveOccurred())

			flunderAudit, findErr := findAuditPayloadSince(
				context.Background(),
				client,
				streamName,
				baselineID,
				300,
				func(payload map[string]interface{}) bool {
					return auditPayloadMatches(payload, "wardle.example.com", "flunders", testNs, flunderName, "create")
				},
			)
			g.Expect(findErr).NotTo(HaveOccurred())

			g.Expect(auditObjectRefName(configMapAudit.Payload)).To(Equal(configMapName))
			g.Expect(auditPayloadHasObject(configMapAudit.Payload, "requestObject")).To(BeTrue())
			g.Expect(auditPayloadHasObject(configMapAudit.Payload, "responseObject")).To(BeTrue())
			g.Expect(auditObjectRefName(flunderAudit.Payload)).To(Equal(flunderName))
			g.Expect(auditPayloadHasObject(flunderAudit.Payload, "requestObject")).To(BeTrue())
			g.Expect(auditPayloadHasObject(flunderAudit.Payload, "responseObject")).To(BeTrue())

			_, _ = fmt.Fprintf(
				GinkgoWriter,
				"ConfigMap create audit payload for comparison:\n%s\n",
				prettyAuditPayload(configMapAudit.Payload),
			)
			_, _ = fmt.Fprintf(
				GinkgoWriter,
				"Flunder create audit payload for comparison:\n%s\n",
				prettyAuditPayload(flunderAudit.Payload),
			)
		}, 30*time.Second, 2*time.Second).Should(Succeed())
	})

	It("should accept an aggregated flunder WatchRule and mark it ready", func() {
		ensureFlunderWatchRuleReady()
	})

	It("should attribute Flunder commits to the impersonated user", func() {
		ensureFlunderWatchRuleReady()

		streamName := defaultAuditRedisStream
		flunderName := fmt.Sprintf("aggregated-impersonation-flunder-%d", GinkgoRandomSeed())
		repoPath := path.Join(
			aggregatedPath,
			fmt.Sprintf("wardle.example.com/v1alpha1/flunders/%s/%s.yaml", testNs, flunderName),
		)
		repoFile := filepath.Join(repo.CheckoutDir, repoPath)
		flunderManifest := fmt.Sprintf(`apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: %s
  namespace: %s
spec:
  reference: impersonated-reference
`, flunderName, testNs)

		By("connecting to Valkey through the e2e port-forward")
		client := newE2EValkeyClient()
		defer func() {
			_ = client.Close()
		}()

		Eventually(func(g Gomega) {
			g.Expect(client.Ping(context.Background()).Err()).NotTo(HaveOccurred())
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("recording the latest audit stream entry before the impersonated create")
		baselineID, err := latestAuditStreamID(context.Background(), client, streamName)
		Expect(err).NotTo(HaveOccurred())

		DeferCleanup(func() {
			if skipCleanupBecauseResourcesArePreserved(fmt.Sprintf("Flunder %s/%s", testNs, flunderName), testNs) {
				return
			}
			_, _ = kubectlRunInNamespace(testNs, "delete", "flunder", flunderName, "--ignore-not-found=true")
		})

		By("creating a Flunder through the aggregated API as an impersonated user")
		_, err = kubectlRunWithStdin(testNs, flunderManifest, "--as=jane@acme.com", "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())

		By("observing the impersonated user in the audit payload")
		Eventually(func(g Gomega) {
			flunderAudit, findErr := findAuditPayloadSince(
				context.Background(),
				client,
				streamName,
				baselineID,
				300,
				func(payload map[string]interface{}) bool {
					return auditPayloadMatches(payload, "wardle.example.com", "flunders", testNs, flunderName, "create")
				},
			)
			g.Expect(findErr).NotTo(HaveOccurred())
			g.Expect(auditObjectRefName(flunderAudit.Payload)).To(Equal(flunderName))
			g.Expect(auditEffectiveUsername(flunderAudit.Payload)).To(Equal("jane@acme.com"))
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("verifying the resulting git commit uses the impersonated user as author")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			content, readErr := os.ReadFile(repoFile)
			g.Expect(readErr).NotTo(HaveOccurred(), "expected Flunder file to exist after impersonated create")
			g.Expect(string(content)).To(ContainSubstring("reference: impersonated-reference"))

			hash, hashErr := latestCommitHashForPath(repo.CheckoutDir, repoPath)
			g.Expect(hashErr).NotTo(HaveOccurred())
			g.Expect(hash).NotTo(BeEmpty())

			author, authorErr := gitRun(repo.CheckoutDir, "show", "-s", "--format=%an <%ae>", hash)
			g.Expect(authorErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(author)).To(Equal("jane@acme.com <jane@acme.com>"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})

	It("should create separate git commits for Flunder create, update, and delete", func() {
		ensureFlunderWatchRuleReady()

		flunderName := fmt.Sprintf("aggregated-commit-flunder-%d", GinkgoRandomSeed())
		repoPath := path.Join(
			aggregatedPath,
			fmt.Sprintf("wardle.example.com/v1alpha1/flunders/%s/%s.yaml", testNs, flunderName),
		)
		repoFile := filepath.Join(repo.CheckoutDir, repoPath)

		renderFlunderManifest := func(reference string) string {
			return fmt.Sprintf(`apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: %s
  namespace: %s
spec:
  reference: %s
`, flunderName, testNs, reference)
		}

		readLatestCommitForPath := func(g Gomega, expectedOperation string) string {
			GinkgoHelper()
			hash, err := latestCommitHashForPath(repo.CheckoutDir, repoPath)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(hash).NotTo(BeEmpty(), "expected a git commit for %s", repoPath)

			subject, err := gitRun(repo.CheckoutDir, "show", "-s", "--format=%s", hash)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(subject).To(ContainSubstring(expectedOperation))
			g.Expect(subject).To(ContainSubstring("flunders"))

			return strings.TrimSpace(hash)
		}

		DeferCleanup(func() {
			if skipCleanupBecauseResourcesArePreserved(fmt.Sprintf("Flunder %s/%s", testNs, flunderName), testNs) {
				return
			}
			_, _ = kubectlRunInNamespace(testNs, "delete", "flunder", flunderName, "--ignore-not-found=true")
		})

		By("creating a Flunder through the aggregated API")
		_, err := kubectlRunWithStdin(testNs, renderFlunderManifest("commit-create-reference"), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())

		var createCommitHash string
		By("waiting for the create commit to land in git")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			content, err := os.ReadFile(repoFile)
			g.Expect(err).NotTo(HaveOccurred(), "expected Flunder file to exist after create")
			g.Expect(string(content)).To(ContainSubstring("reference: commit-create-reference"))

			createCommitHash = readLatestCommitForPath(g, "[CREATE]")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("updating the Flunder through the aggregated API")
		_, err = kubectlRunWithStdin(testNs, renderFlunderManifest("commit-update-reference"), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())

		var updateCommitHash string
		By("waiting for the update commit to land in git")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			content, err := os.ReadFile(repoFile)
			g.Expect(err).NotTo(HaveOccurred(), "expected Flunder file to exist after update")
			g.Expect(string(content)).To(ContainSubstring("reference: commit-update-reference"))
			g.Expect(string(content)).NotTo(ContainSubstring("reference: commit-create-reference"))

			updateCommitHash = readLatestCommitForPath(g, "[UPDATE]")
			g.Expect(updateCommitHash).NotTo(Equal(createCommitHash))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("deleting the Flunder through the aggregated API")
		_, err = kubectlRunInNamespace(testNs, "delete", "flunder", flunderName)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the delete commit to land in git")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			_, statErr := os.Stat(repoFile)
			g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "expected Flunder file to be removed after delete")

			deleteCommitHash := readLatestCommitForPath(g, "[DELETE]")
			g.Expect(deleteCommitHash).NotTo(Equal(updateCommitHash))

			subjectsOutput, err := gitRun(repo.CheckoutDir, "log", "--format=%s", "-n", "3", "--", repoPath)
			g.Expect(err).NotTo(HaveOccurred())
			subjects := nonEmptyTrimmedLines(subjectsOutput)
			subjectsJoined := strings.Join(subjects, "\n")
			g.Expect(subjectsJoined).To(ContainSubstring("[DELETE]"))
			g.Expect(subjectsJoined).To(ContainSubstring("[UPDATE]"))
			g.Expect(subjectsJoined).To(ContainSubstring("[CREATE]"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})
})

func auditEffectiveUsername(payload map[string]interface{}) string {
	if impersonated := nestedAuditString(payload, "impersonatedUser", "username"); impersonated != "" {
		return impersonated
	}

	return nestedAuditString(payload, "user", "username")
}

func nestedAuditString(payload map[string]interface{}, fields ...string) string {
	current := any(payload)
	for _, field := range fields {
		nextMap, ok := current.(map[string]interface{})
		if !ok {
			return ""
		}
		current, ok = nextMap[field]
		if !ok {
			return ""
		}
	}

	value, ok := current.(string)
	if !ok {
		return ""
	}

	return strings.TrimSpace(value)
}
