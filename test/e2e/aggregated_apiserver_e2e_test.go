// SPDX-License-Identifier: Apache-2.0

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

// Not Serial: the wardle APIService is installed once at cluster setup and only
// read here; everything this spec mutates is name-isolated. See
// docs/spec/e2e-serial-registry.md.
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
		createReadyGitProvider(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		createValidatedGitTarget(targetName, testNs, providerName, aggregatedPath)
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

		By("waiting for the flunder stream to be live before asserting per-event commits")
		waitForStreamsRunning(targetName, testNs)
	}

	AfterAll(func() {
		cleanupPipeline(testNs, providerName, targetName, watchRuleName)
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("should install and serve flunders through the aggregation layer", func() {
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

	It("should accept an aggregated flunder WatchRule and mark it ready", func() {
		ensureFlunderWatchRuleReady()
	})

	It("should create separate git commits for Flunder create, update, and delete", func() {
		ensureFlunderWatchRuleReady()

		flunderName := fmt.Sprintf("aggregated-commit-flunder-%d", GinkgoRandomSeed())
		repoPath := path.Join(
			aggregatedPath,
			fmt.Sprintf("%s/wardle.example.com/flunders/%s.yaml", testNs, flunderName),
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

			// The flunder path's only writers are the create/update/delete this spec performed, so
			// its full history is exactly those three commits, newest first.
			subjects := commitSubjectsForPath(g, repo.CheckoutDir, repoPath, 3)
			g.Expect(subjects).To(HaveLen(3),
				"flunder path history should be exactly create+update+delete, got %v", subjects)
			g.Expect(subjects[0]).To(ContainSubstring("[DELETE]"))
			g.Expect(subjects[1]).To(ContainSubstring("[UPDATE]"))
			g.Expect(subjects[2]).To(ContainSubstring("[CREATE]"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})
})
