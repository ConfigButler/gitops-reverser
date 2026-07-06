// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	demoCoffeeConfigGitProviderName = "demo-coffeeconfig"
	demoEnabledEnv                  = "E2E_ENABLE_DEMO"
	demoNamespace                   = "vote"
	voterTestNamespace              = "voter-test"
)

var demoCoffeeConfigGitPath = strings.Join([]string{
	"voter-coffee",
	"voter-test",
	"examples.configbutler.ai",
	"coffeeconfigs",
	"testnet-coffee.yaml",
}, "/")

type demoRun struct {
	repoName        string
	checkoutDir     string
	repoURL         string
	sourceNamespace string
	gitSecretName   string
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

		By("ensuring the demo repository has a main branch")
		run.ensureMainBranchSeeded()

		By("copying Git credentials into demo namespaces")
		run.copyGitSecret()
		run.refreshDemoCoffeeConfigResources()
		run.removeLegacyDemoResources()

		By("verifying the CoffeeConfig reverse GitOps resources become Ready")
		run.verifyCoffeeConfigReverseResourcesReady()

		By("touching the CoffeeConfig so demo-test is created when the repo was reset")
		run.touchCoffeeConfig()

		By("waiting for the CoffeeConfig reconcile to seed the demo-test branch")
		run.verifyCoffeeConfigBranchSeeded()

		By("verifying the CoffeeConfig Flux resources become Ready")
		run.verifyCoffeeConfigFluxResourcesReady()

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
		repoName:        demoRepo.RepoName,
		checkoutDir:     demoRepo.CheckoutDir,
		repoURL:         demoRepo.RepoURLHTTP,
		sourceNamespace: resolveE2ENamespace(),
		gitSecretName:   demoRepo.GitSecretHTTP,
	}
}

func (r *demoRun) setupRepository() {
	_, err := os.Stat(filepath.Join(r.checkoutDir, ".git"))
	Expect(err).NotTo(HaveOccurred(), "expected checkout to exist")
}

func (r *demoRun) ensureMainBranchSeeded() {
	if r.repoHasCommits() {
		return
	}

	username, password := r.readGitCredentials()
	authURL := r.authenticatedOriginURL(username, password)

	readmePath := filepath.Join(r.checkoutDir, "README.md")
	content := "# Demo repository\n\nSeeded by `task test-e2e-demo` so promotion branches can start from `main`.\n"

	Expect(os.WriteFile(readmePath, []byte(content), 0o644)).To(Succeed(), "failed to write demo README")
	r.runGit("remote", "set-url", "origin", authURL)
	r.runGit("checkout", "-B", "main")
	r.runGit("add", "README.md")
	r.runGit("commit", "-m", "docs: seed demo repository")
	r.runGit("push", "--set-upstream", "origin", "main")
}

func (r *demoRun) repoHasCommits() bool {
	_, err := gitRun(r.checkoutDir, "rev-parse", "--verify", "HEAD")
	return err == nil
}

func (r *demoRun) readGitCredentials() (string, string) {
	output, err := kubectlRunInNamespace(r.sourceNamespace, "get", "secret", r.gitSecretName, "-o", "json")
	Expect(err).NotTo(HaveOccurred(), "failed to fetch source Git Secret")

	var secret struct {
		Data map[string]string `json:"data"`
	}
	Expect(json.Unmarshal([]byte(output), &secret)).To(Succeed())

	username, err := base64.StdEncoding.DecodeString(secret.Data["username"])
	Expect(err).NotTo(HaveOccurred(), "failed to decode Git username")

	password, err := base64.StdEncoding.DecodeString(secret.Data["password"])
	Expect(err).NotTo(HaveOccurred(), "failed to decode Git password")

	return strings.TrimSpace(string(username)), strings.TrimSpace(string(password))
}

func (r *demoRun) authenticatedOriginURL(username, password string) string {
	output, err := gitRun(r.checkoutDir, "remote", "get-url", "origin")
	Expect(err).NotTo(HaveOccurred(), "failed to read demo repo origin URL")

	parsedURL, err := url.Parse(strings.TrimSpace(output))
	Expect(err).NotTo(HaveOccurred(), "failed to parse demo repo origin URL")

	parsedURL.User = url.UserPassword(username, password)
	return parsedURL.String()
}

func (r *demoRun) runGit(args ...string) {
	output, err := gitRun(r.checkoutDir, args...)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("git %s: %s", strings.Join(args, " "), output))
}

func (r *demoRun) copyGitSecret() {
	output, err := kubectlRunInNamespace(r.sourceNamespace, "get", "secret", r.gitSecretName, "-o", "json")
	Expect(err).NotTo(HaveOccurred(), "failed to fetch source Git Secret")

	var secretObj map[string]interface{}
	Expect(json.Unmarshal([]byte(output), &secretObj)).To(Succeed())

	for _, namespace := range []string{demoNamespace, "voter-production", voterTestNamespace} {
		copied, err := json.Marshal(secretObj)
		Expect(err).NotTo(HaveOccurred(), "failed to clone source Git Secret")

		var copiedSecret map[string]interface{}
		Expect(json.Unmarshal(copied, &copiedSecret)).To(Succeed())

		metadata, found, err := unstructured.NestedMap(copiedSecret, "metadata")
		Expect(err).NotTo(HaveOccurred(), "failed to read Secret metadata")
		Expect(found).To(BeTrue(), "expected Secret metadata")

		metadata["namespace"] = namespace
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

		copiedSecret["metadata"] = metadata
		delete(copiedSecret, "status")

		manifest, err := json.Marshal(copiedSecret)
		Expect(err).NotTo(HaveOccurred(), "failed to marshal copied Git Secret")

		_, err = kubectlRunWithStdin(namespace, string(manifest), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "failed to apply copied Git Secret")
	}
}

func (r *demoRun) refreshDemoCoffeeConfigResources() {
	r.refreshDemoCoffeeConfigResource("gitprovider")
	r.refreshDemoCoffeeConfigResource("gittarget")
}

func (r *demoRun) refreshDemoCoffeeConfigResource(resourceType string) {
	_, err := kubectlRunInNamespace(voterTestNamespace, "get", "gitprovider", demoCoffeeConfigGitProviderName)
	if err != nil {
		return
	}

	reconcileValue := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = kubectlRunInNamespace(
		voterTestNamespace,
		"annotate",
		resourceType,
		demoCoffeeConfigGitProviderName,
		fmt.Sprintf("configbutler.ai/manual-reconcile=%s", reconcileValue),
		"--overwrite",
	)
	Expect(err).NotTo(HaveOccurred(), "failed to refresh voter-test CoffeeConfig "+resourceType)
}

func (r *demoRun) removeLegacyDemoResources() {
	legacyNameSuffix := r.repoName
	_, _ = kubectlRunInNamespace(
		demoNamespace,
		"delete",
		"watchrule",
		"demo-watch-all-"+legacyNameSuffix,
		"--ignore-not-found=true",
	)
	_, _ = kubectlRun(
		"delete",
		"clusterwatchrule",
		"demo-cluster-resources-"+legacyNameSuffix,
		"--ignore-not-found=true",
	)
	_, _ = kubectlRunInNamespace(
		demoNamespace,
		"delete",
		"gittarget",
		"demo-target-"+legacyNameSuffix,
		"--ignore-not-found=true",
	)
	_, _ = kubectlRunInNamespace(
		demoNamespace,
		"delete",
		"gitprovider",
		"demo-provider-"+legacyNameSuffix,
		"--ignore-not-found=true",
	)
}

func (r *demoRun) verifyCoffeeConfigReverseResourcesReady() {
	verifyResourceStatus("gitprovider", demoCoffeeConfigGitProviderName, voterTestNamespace, "True", "Ready", "")
	verifyResourceCondition(
		"gittarget", demoCoffeeConfigGitProviderName, voterTestNamespace, "Validated", "True", "OK", "")
	verifyResourceStatus("watchrule", demoCoffeeConfigGitProviderName, voterTestNamespace, "True", "Ready", "")
}

func (r *demoRun) touchCoffeeConfig() {
	reconcileValue := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := kubectlRunInNamespace(
		voterTestNamespace,
		"annotate",
		"coffeeconfig",
		"testnet-coffee",
		fmt.Sprintf("configbutler.ai/demo-prepared-at=%s", reconcileValue),
		"--overwrite",
	)
	Expect(err).NotTo(HaveOccurred(), "failed to touch voter-test CoffeeConfig")
}

func (r *demoRun) verifyCoffeeConfigFluxResourcesReady() {
	r.verifyFluxResourceReady("gitrepository", demoCoffeeConfigGitProviderName, voterTestNamespace)
	r.verifyFluxResourceReady("kustomization", demoCoffeeConfigGitProviderName, voterTestNamespace)
}

func (r *demoRun) verifyFluxResourceReady(resourceType, name, namespace string) {
	Eventually(func(g Gomega) {
		output, err := kubectlRunInNamespace(
			namespace,
			"get",
			resourceType,
			name,
			"-o",
			"jsonpath={.status.conditions[?(@.type=='Ready')].status}",
		)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(output)).To(Equal("True"))
	}, time.Minute, time.Second).Should(Succeed())
}

func (r *demoRun) verifyCoffeeConfigBranchSeeded() {
	Eventually(func(g Gomega) {
		fetchOutput, fetchErr := gitRun(r.checkoutDir, "fetch", "origin", "demo-test")
		g.Expect(fetchErr).NotTo(HaveOccurred(), fmt.Sprintf("git fetch origin demo-test: %s", fetchOutput))

		content, showErr := gitRun(r.checkoutDir, "show", "origin/demo-test:"+demoCoffeeConfigGitPath)
		g.Expect(showErr).NotTo(HaveOccurred(), fmt.Sprintf("expected CoffeeConfig file %s", demoCoffeeConfigGitPath))
		g.Expect(content).To(ContainSubstring("kind: CoffeeConfig"))
		g.Expect(content).To(ContainSubstring("name: testnet-coffee"))
		g.Expect(content).To(ContainSubstring("namespace: voter-test"))
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
}

func (r *demoRun) logArtifacts() {
	_, _ = fmt.Fprintf(
		GinkgoWriter,
		"Demo ready:\n  repo=%s\n  checkout=%s\n  repoURL=%s\n  namespace=%s\n  path=%s\n",
		r.repoName,
		r.checkoutDir,
		r.repoURL,
		voterTestNamespace,
		demoCoffeeConfigGitPath,
	)
}
