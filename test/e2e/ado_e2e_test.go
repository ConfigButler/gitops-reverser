// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The Azure DevOps corner: the one e2e category that talks to a real hosted Git provider instead of
// the in-cluster Gitea.
//
// It exists because ADO is the provider that broke. It rejects any Git fetch whose capability list
// omits multi_ack with HTTP 400 "TF401041: Clients must support multi-ack", which go-git only
// implements from v6. `internal/git/ado_multiack_test.go` reproduces that rule locally and gates CI;
// `internal/git/ado_live_test.go` checks the library against the real remote. This spec is the layer
// above both: it proves the *operator* mirrors a live cluster into an ADO repository, which is what a
// user actually cares about, and it is the recipe docs/azure-devops-getting-started.md is written from.
//
// Opt-in, because it needs a credential nobody's CI has:
//
//	export E2E_ADO_REPO_URL='https://dev.azure.com/<org>/<project>/_git/<repo>'
//	export E2E_ADO_PAT='<personal access token>'   # scope: Code (read & write)
//	task test-e2e-ado
//
// It writes to a path of its own inside the repository and does not clean the repository up. Point it
// at a scratch repo.
const (
	adoRepoURLEnv  = "E2E_ADO_REPO_URL"
	adoPATEnv      = "E2E_ADO_PAT"
	adoUsernameEnv = "E2E_ADO_USERNAME"
)

// skipUnlessADOConfigured aborts the calling spec unless a real ADO repository is supplied. The Ginkgo
// label alone is not enough: no credential, no test.
func skipUnlessADOConfigured() string {
	GinkgoHelper()

	url := strings.TrimSpace(os.Getenv(adoRepoURLEnv))
	if url == "" || strings.TrimSpace(os.Getenv(adoPATEnv)) == "" {
		Skip(fmt.Sprintf(
			"Azure DevOps corner is disabled; set %s and %s, then run `task test-e2e-ado`",
			adoRepoURLEnv, adoPATEnv,
		))
	}

	return url
}

var _ = Describe("Azure DevOps", Label("ado"), Ordered, func() {
	const (
		providerName = "ado-provider"
		targetName   = "ado-target"
		ruleName     = "ado-rule"
		secretName   = "ado-creds"
	)

	var (
		testNs   string
		repoURL  string
		repoPath string
	)

	BeforeAll(func() {
		repoURL = skipUnlessADOConfigured()

		testNs = testNamespaceFor("ado")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent

		// A path of our own, so a shared scratch repository can carry several runs.
		repoPath = fmt.Sprintf("e2e/ado-%d", GinkgoRandomSeed())

		By("creating the ADO credentials Secret")
		// ADO sends a PAT as HTTP basic auth with the token as the password and ignores the
		// username, so only `password` is set. This is the Secret shape the getting-started guide
		// documents, and creating it any other way is the most common way to get ADO wrong.
		_, err := kubectlRunInNamespace(testNs, "create", "secret", "generic", secretName,
			"--from-literal=password="+os.Getenv(adoPATEnv))
		Expect(err).NotTo(HaveOccurred(), "failed to create the ADO credentials Secret")

		applySOPSAgeKeyToNamespace(testNs)

		By("creating a GitProvider pointing at the ADO repository")
		createGitProviderWithURLInNamespace(providerName, testNs, secretName, repoURL)

		// Reaching Ready here already proves more than it looks: the connectivity check reads the
		// ref advertisement, which is the one ADO operation that never needed multi_ack.
		verifyResourceStatus(
			"gitprovider", providerName, testNs,
			"True", "Succeeded", "Repository connectivity validated",
		)

		By("creating a GitTarget for a path of this run's own")
		createGitTarget(targetName, testNs, providerName, repoPath, "main")
		verifyResourceCondition("gittarget", targetName, testNs, "Validated", "True", "Succeeded", "")

		By("watching ConfigMaps in the test namespace")
		applyIsolationWatchRule(ruleName, testNs, targetName, `"configmaps"`)
		verifyResourceStatus("watchrule", ruleName, testNs, "True", "Succeeded", "")

		By("waiting for the stream to be live before creating anything to mirror")
		waitForStreamsRunning(targetName, testNs)
	})

	AfterAll(func() {
		if testNs != "" {
			cleanupNamespace(testNs)
		}
	})

	It("mirrors a live ConfigMap into the Azure DevOps repository", func() {
		const cmName = "ado-demo"

		By("creating a ConfigMap in the cluster")
		_, err := kubectlRunInNamespace(testNs, "create", "configmap", cmName,
			"--from-literal=greeting=hello-from-azure-devops")
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the operator to commit it to ADO, then reading the repository back")
		// Cloning with canonical git rather than our own library is deliberate: the assertion must
		// not depend on the code under test.
		wanted := filepath.Join(repoPath, testNs, "configmaps", cmName+".yaml")
		Eventually(func(g Gomega) {
			content := adoReadFile(g, repoURL, wanted)
			g.Expect(content).To(ContainSubstring("hello-from-azure-devops"),
				"the committed manifest must carry the ConfigMap's data")
			g.Expect(content).To(ContainSubstring("kind: ConfigMap"))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})
})

// adoReadFile clones the ADO repository with canonical git and returns one file's contents. The clone
// is shallow and single-branch: this is a read-back assertion, not a history check.
func adoReadFile(g Gomega, repoURL, relPath string) string {
	GinkgoHelper()

	dir, err := os.MkdirTemp("", "ado-readback-*")
	g.Expect(err).NotTo(HaveOccurred())
	defer func() { _ = os.RemoveAll(dir) }()

	// The PAT travels in a config header rather than in argv or the URL, so it stays out of process
	// listings and out of any error text that quotes the command.
	cfg := filepath.Join(dir, "gitconfig")
	g.Expect(os.WriteFile(cfg, []byte(adoExtraHeader(repoURL)), 0600)).To(Succeed())

	checkout := filepath.Join(dir, "checkout")
	cmd := exec.Command("git", "clone", "--depth=1", "--single-branch", repoURL, checkout)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+cfg,
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(), "git clone of the ADO repository failed: %s", out)

	content, err := os.ReadFile(filepath.Join(checkout, relPath))
	g.Expect(err).NotTo(HaveOccurred(), "expected %s in the ADO repository", relPath)

	return string(content)
}

// adoExtraHeader renders a gitconfig that authenticates to this repository's origin only, so the
// credential is never offered to another host on a redirect.
func adoExtraHeader(repoURL string) string {
	origin := repoURL
	if idx := strings.Index(repoURL, "/_git/"); idx > 0 {
		origin = repoURL[:idx]
	}

	basic := adoBasicCredential()

	return fmt.Sprintf("[http %q]\n\textraHeader = Authorization: Basic %s\n", origin, basic)
}

// adoBasicCredential base64-encodes the PAT as HTTP basic auth: an empty username with the token as
// the password, which is the form ADO documents.
func adoBasicCredential() string {
	return base64.StdEncoding.EncodeToString(
		[]byte(os.Getenv(adoUsernameEnv) + ":" + os.Getenv(adoPATEnv)))
}
