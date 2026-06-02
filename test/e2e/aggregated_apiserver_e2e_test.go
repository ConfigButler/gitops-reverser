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
	"path"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Not Serial: the wardle APIService is installed once at cluster setup (Flux)
// and only read here; everything this spec mutates is name-isolated. See
// docs/design/e2e-serial-registry.md.
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

		By("setting up the Prometheus client for the shallow-drop metric check")
		setupPrometheusClient()
		verifyPrometheusAvailable()

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

	It("should never drop aggregated audit events as shallow", func() {
		// A "shallow" audit event is one whose request/response body never
		// arrived, so it cannot drive a high-quality Git write and is dropped,
		// incrementing gitopsreverser_audit_shallow_dropped_total. In a correctly
		// configured environment every audit event is paired with its body, so
		// that counter must stay at zero — for this scenario and for every other
		// test sharing the cluster. Asserting it here is the externally
		// observable equivalent of confirming the proxy recovers full bodies.
		ensureFlunderWatchRuleReady()

		flunderName := fmt.Sprintf("aggregated-shallow-flunder-%d", GinkgoRandomSeed())
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
  reference: shallow-check-reference
`, flunderName, testNs)

		DeferCleanup(func() {
			if skipCleanupBecauseResourcesArePreserved(fmt.Sprintf("Flunder %s/%s", testNs, flunderName), testNs) {
				return
			}
			_, _ = kubectlRunInNamespace(testNs, "delete", "flunder", flunderName, "--ignore-not-found=true")
		})

		By("creating a Flunder through the aggregated API to exercise the audit body-join path")
		_, err := kubectlRunWithStdin(testNs, flunderManifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the create commit, confirming the audit body was recovered (not shallow)")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			content, readErr := os.ReadFile(repoFile)
			g.Expect(readErr).NotTo(HaveOccurred(), "expected Flunder file to exist after create")
			g.Expect(string(content)).To(ContainSubstring("reference: shallow-check-reference"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("verifying no audit events were dropped as shallow, cluster-wide")
		// Zero is the only acceptable value here. We never want a shallow drop:
		// not for this Flunder, and not as a side effect of any other test
		// sharing this cluster. Any non-zero count means a request/response body
		// failed to join its official event, so this assertion stays absolute
		// (not a baseline delta) on purpose.
		Consistently(func(g Gomega) {
			dropped, queryErr := queryPrometheus("sum(gitopsreverser_audit_shallow_dropped_total) or vector(0)")
			g.Expect(queryErr).NotTo(HaveOccurred())
			g.Expect(dropped).To(BeZero(),
				"gitopsreverser_audit_shallow_dropped_total must stay at zero in a healthy e2e environment")
		}, 10*time.Second, 2*time.Second).Should(Succeed())
	})

	It("should accept an aggregated flunder WatchRule and mark it ready", func() {
		ensureFlunderWatchRuleReady()
	})

	It("should attribute Flunder commits to the impersonated user", func() {
		ensureFlunderWatchRuleReady()

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

		DeferCleanup(func() {
			if skipCleanupBecauseResourcesArePreserved(fmt.Sprintf("Flunder %s/%s", testNs, flunderName), testNs) {
				return
			}
			_, _ = kubectlRunInNamespace(testNs, "delete", "flunder", flunderName, "--ignore-not-found=true")
		})

		By("creating a Flunder through the aggregated API as an impersonated user")
		_, err := kubectlRunWithStdin(testNs, flunderManifest, "--as=jane@acme.com", "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())

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
