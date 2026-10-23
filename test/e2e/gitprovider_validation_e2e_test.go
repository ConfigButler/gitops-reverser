// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Manager GitProvider Validation", Label("manager"), Ordered, func() {
	var (
		testNs         string
		validationRepo *RepoArtifacts
	)

	BeforeAll(func() {
		By("creating GitProvider validation test namespace")
		testNs = testNamespaceFor("manager-gitprovider")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		By("setting up Gitea repo and credentials for GitProvider validation tests")
		validationRepo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-manager-gitprovider-%d", GinkgoRandomSeed()),
		)

		By("applying git secrets to test namespace")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", validationRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)
	})

	AfterAll(func() {
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("should validate GitProvider with real Gitea repository", func() {
		gitProviderName := "gitprovider-e2e-test"

		By("showing initial controller logs")
		showControllerLogs("before creating GitProvider")

		createGitProviderWithURLInNamespace(
			gitProviderName,
			testNs,
			validationRepo.GitSecretHTTP,
			validationRepo.RepoURLHTTP,
		)

		By("showing controller logs after GitProvider creation")
		showControllerLogs("after creating GitProvider")

		verifyResourceStatus(
			"gitprovider", gitProviderName, testNs,
			"True", "Succeeded", "Repository connectivity validated",
		)

		By("showing final controller logs")
		showControllerLogs("after status verification")

		deleteGitProvider(testNs, gitProviderName)
	})

	It("should handle GitProvider with invalid credentials", func() {
		gitProviderName := "gitprovider-invalid-test"
		createGitProviderWithURLInNamespace(
			gitProviderName,
			testNs,
			validationRepo.GitSecretInvalid,
			validationRepo.RepoURLHTTP,
		)
		verifyResourceStatus("gitprovider", gitProviderName, testNs, "False", "ConnectionFailed", "")
		deleteGitProvider(testNs, gitProviderName)
	})

	It("should refuse a GitBranch whose branch the GitProvider does not allow", func() {
		gitProviderName := "gitprovider-branch-test"

		// GitProvider should be Ready=True (validates connectivity, not branch existence)
		createGitProviderWithURLInNamespace(
			gitProviderName,
			testNs,
			validationRepo.GitSecretHTTP,
			validationRepo.RepoURLHTTP,
		)
		verifyResourceStatus(
			"gitprovider", gitProviderName, testNs, "True", "Succeeded", "Repository connectivity validated",
		)

		// A GitBranch for a branch matching no pattern is refused, and its GitTarget waits for it.
		destName := "dest-invalid-branch"
		createGitTarget(destName, testNs, gitProviderName, "test/invalid", "different-branch")

		By("verifying the GitBranch fails branch validation")
		verifyResourceStatus("gitbranch", gitBranchName(gitProviderName, "different-branch"), testNs,
			"False", "BranchNotAllowed", `requested branch "different-branch" is not allowed`)
		verifyResourceStatus("gittarget", destName, testNs, "False", "GitBranchNotReady", "spec.allowedGitBranches")

		cleanupGitTarget(destName, testNs)
		deleteGitProvider(testNs, gitProviderName)
	})

	It("should validate GitProvider with SSH authentication", func() {
		gitProviderName := "gitprovider-ssh-test"

		By("🔐 Starting SSH authentication test")
		showControllerLogs("before SSH test")

		By("📋 Checking SSH secret exists")
		secretOutput, err := kubectlRunInNamespace(testNs, "get", "secret", validationRepo.GitSecretSSH, "-o", "yaml")
		if err != nil {
			fmt.Printf("❌ SSH secret not found: %v\n", err)
		} else {
			previewLen := min(300, len(secretOutput))
			fmt.Printf(
				"✅ SSH secret exists - showing first %d chars:\n%s...\n",
				previewLen,
				secretOutput[:previewLen],
			)
		}

		createGitProviderWithURLInNamespace(
			gitProviderName,
			testNs,
			validationRepo.GitSecretSSH,
			validationRepo.RepoURLSSH,
		)

		By("🔍 Controller logs after SSH GitProvider creation")
		showControllerLogs("after SSH GitProvider creation")

		verifyResourceStatus(
			"gitprovider", gitProviderName, testNs, "True", "Succeeded", "Repository connectivity validated",
		)

		By("✅ Final SSH test logs")
		showControllerLogs("SSH test completion")

		deleteGitProvider(testNs, gitProviderName)
	})
})
