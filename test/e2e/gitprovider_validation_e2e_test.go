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
			"True", "Ready", "Repository connectivity validated",
		)

		By("showing final controller logs")
		showControllerLogs("after status verification")

		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", gitProviderName, "--ignore-not-found=true")
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
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", gitProviderName, "--ignore-not-found=true")
	})

	It("should handle GitTarget with nonexistent branch pattern", func() {
		gitProviderName := "gitprovider-branch-test"

		// GitProvider should be Ready=True (validates connectivity, not branch existence)
		createGitProviderWithURLInNamespace(
			gitProviderName,
			testNs,
			validationRepo.GitSecretHTTP,
			validationRepo.RepoURLHTTP,
		)
		verifyResourceStatus(
			"gitprovider", gitProviderName, testNs, "True", "Ready", "Repository connectivity validated",
		)

		// GitTarget with branch not matching any pattern should fail
		destName := "dest-invalid-branch"
		createGitTarget(destName, testNs, gitProviderName, "test/invalid", "different-branch")

		By("verifying GitTarget fails branch validation")
		verifyResourceStatus("gittarget", destName, testNs, "False", "ValidationFailed", "BranchNotAllowed")

		cleanupGitTarget(destName, testNs)
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", gitProviderName, "--ignore-not-found=true")
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
			"gitprovider", gitProviderName, testNs, "True", "Ready", "Repository connectivity validated",
		)

		By("✅ Final SSH test logs")
		showControllerLogs("SSH test completion")

		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", gitProviderName, "--ignore-not-found=true")
	})
})
