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
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	reverserGit "github.com/ConfigButler/gitops-reverser/internal/git"
)

// signingGitProviderData holds template data for a signing-enabled GitProvider.
type signingGitProviderData struct {
	Name                string
	Namespace           string
	RepoURL             string
	Branch              string
	SecretName          string
	CommitterName       string
	CommitterEmail      string
	MessageTemplate     string
	BatchTemplate       string
	SigningSecretName   string
	GenerateWhenMissing bool
}

// signingRepo holds the file-local repo fixtures for the Commit Signing describe block.
var signingRepo *RepoArtifacts

const (
	signingCommitterName  = "GitOps Reverser"
	signingCommitterEmail = "noreply@configbutler.ai"
)

var _ = Describe("Commit Signing", Label("signing"), Ordered, func() {
	var testNs string

	BeforeAll(func() {
		testNs = testNamespaceFor("signing")

		By("creating signing test namespace")
		_, _ = kubectlRun("create", "namespace", testNs)

		By("setting up Gitea repo and credentials for signing tests")
		signingRepo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-signing-%d", GinkgoRandomSeed()),
		)

		By("applying git secrets to signing test namespace")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", signingRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to signing test namespace")

		applySOPSAgeKeyToNamespace(testNs)

		By("binding committer email to the Gitea admin user for signature verification")
		// Gitea verifies SSH-signed commits by looking up a user whose verified
		// emails include the commit's committer email, then walking that user's
		// registered SSH keys. A secondary email on /user/emails is not treated
		// as verified for this purpose; patching the admin user's primary email
		// via /admin/users is the reliable binding.
		adminUser, _ := giteaAdminCreds()
		_, err = EnsureAdminUserPrimaryEmail(adminUser, signingCommitterEmail)
		Expect(err).NotTo(HaveOccurred(), "failed to bind committer email to Gitea admin user")
	})

	AfterAll(func() {
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(90 * time.Second)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	// ── Test 1: generated signing key — local + Gitea verification ──────────

	It("should produce per-event commits verifiable locally and by Gitea (generated key)",
		Label("smoke"), func() {
			providerName := "signing-per-event"
			signingSecretName := "signing-key-per-event"
			destName := providerName + "-dest"
			watchRuleName := providerName + "-wr"
			cmName := "signing-test-cm-per-event"
			commitPath := "e2e/signing-per-event"

			DeferCleanup(func() {
				_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
				cleanupWatchRule(watchRuleName, testNs)
				cleanupGitTarget(destName, testNs)
				_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
			})

			By("creating a GitProvider with commit signing enabled (generateWhenMissing)")
			data := signingGitProviderData{
				Name:                providerName,
				Namespace:           testNs,
				RepoURL:             signingRepo.RepoURLHTTP,
				Branch:              "main",
				SecretName:          signingRepo.GitSecretHTTP,
				CommitterName:       signingCommitterName,
				CommitterEmail:      signingCommitterEmail,
				MessageTemplate:     "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}",
				BatchTemplate:       "reconcile: sync {{.Count}} resources",
				SigningSecretName:   signingSecretName,
				GenerateWhenMissing: true,
			}
			Expect(applyFromTemplate("test/e2e/templates/gitprovider-signing.tmpl", data, testNs)).
				To(Succeed(), "failed to apply signing GitProvider")

			By("waiting for status.signingPublicKey")
			var signingPublicKey string
			Eventually(func(g Gomega) {
				output, err := kubectlRunInNamespace(testNs, "get", "gitprovider", providerName,
					"-o", "jsonpath={.status.signingPublicKey}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(HavePrefix("ssh-"), "signingPublicKey should be populated")
				signingPublicKey = strings.TrimSpace(output)
			}).Should(Succeed())
			verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")

			By("registering the generated signing public key with Gitea")
			registered, err := RegisterSigningPublicKey(signingPublicKey, "e2e-signing-generated-"+providerName)
			Expect(err).NotTo(HaveOccurred())
			Expect(registered).NotTo(BeNil())
			Expect(registered.ID).To(BeNumerically(">", 0))
			DeferCleanup(func() { _ = DeleteUserPublicKey(registered.ID) })

			By("creating GitTarget and WatchRule")
			createGitTarget(destName, testNs, providerName, commitPath, "main")
			verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

			watchRuleData := struct {
				Name            string
				Namespace       string
				DestinationName string
			}{watchRuleName, testNs, destName}
			Expect(applyFromTemplate("test/e2e/templates/watchrule.tmpl", watchRuleData, testNs)).To(Succeed())
			verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

			By("triggering a per-event commit")
			_, err = kubectlRunInNamespace(testNs, "create", "configmap", cmName, "--from-literal=key=signed-value")
			Expect(err).NotTo(HaveOccurred())

			By("waiting for a signed commit to land in the path")
			var commitHash string
			Eventually(func(g Gomega) {
				_, pullErr := gitRun(signingRepo.CheckoutDir, "pull")
				g.Expect(pullErr).NotTo(HaveOccurred())

				h, hErr := latestCommitHashForPath(signingRepo.CheckoutDir, commitPath)
				g.Expect(hErr).NotTo(HaveOccurred())
				g.Expect(h).NotTo(BeEmpty(), "expected a commit in %s", commitPath)

				rawObj, rawErr := gitRun(signingRepo.CheckoutDir, "cat-file", "commit", h)
				g.Expect(rawErr).NotTo(HaveOccurred())
				g.Expect(rawObj).To(ContainSubstring("-----BEGIN SSH SIGNATURE-----"),
					"commit should carry an SSH signature")
				commitHash = h
			}, "60s", "3s").Should(Succeed())

			By("verifying the commit locally with ssh-keygen and git verify-commit")
			assertLocalSSHVerification(signingRepo.CheckoutDir, commitHash, signingPublicKey, signingCommitterEmail)

			By("verifying the commit through the Gitea commit API")
			assertGiteaVerified(signingRepo.RepoName, commitHash)
		})

	// ── Test 2: BYOK signing — user-provided key, local + Gitea verification ─

	It("should produce per-event commits verifiable locally and by Gitea (BYOK)", Label("byok"), func() {
		providerName := "signing-byok"
		signingSecretName := "signing-key-byok"
		destName := providerName + "-dest"
		watchRuleName := providerName + "-wr"
		cmName := "signing-test-cm-byok"
		commitPath := "e2e/signing-byok"

		DeferCleanup(func() {
			_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
			cleanupWatchRule(watchRuleName, testNs)
			cleanupGitTarget(destName, testNs)
			_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
			_, _ = kubectlRunInNamespace(testNs, "delete", "secret", signingSecretName, "--ignore-not-found=true")
		})

		By("generating a BYOK SSH signing keypair")
		privateKeyPEM, publicKey, err := reverserGit.GenerateSSHSigningKeyPair(nil)
		Expect(err).NotTo(HaveOccurred())
		publicKeyStr := strings.TrimSpace(string(publicKey))
		Expect(publicKeyStr).To(HavePrefix("ssh-"))

		By("creating the signing Secret from the BYOK keypair")
		applySigningSecret(testNs, signingSecretName, privateKeyPEM, publicKey)

		By("registering the BYOK signing public key with Gitea")
		registered, err := RegisterSigningPublicKey(publicKeyStr, "e2e-signing-byok-"+providerName)
		Expect(err).NotTo(HaveOccurred())
		Expect(registered).NotTo(BeNil())
		DeferCleanup(func() { _ = DeleteUserPublicKey(registered.ID) })

		By("creating a GitProvider that consumes the BYOK signing Secret (generateWhenMissing=false)")
		data := signingGitProviderData{
			Name:                providerName,
			Namespace:           testNs,
			RepoURL:             signingRepo.RepoURLHTTP,
			Branch:              "main",
			SecretName:          signingRepo.GitSecretHTTP,
			CommitterName:       signingCommitterName,
			CommitterEmail:      signingCommitterEmail,
			MessageTemplate:     "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}",
			BatchTemplate:       "reconcile: sync {{.Count}} resources",
			SigningSecretName:   signingSecretName,
			GenerateWhenMissing: false,
		}
		Expect(applyFromTemplate("test/e2e/templates/gitprovider-signing.tmpl", data, testNs)).To(Succeed())

		By("waiting for GitProvider to become Ready and to surface the BYOK public key")
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")
		var statusKey string
		Eventually(func(g Gomega) {
			output, err := kubectlRunInNamespace(testNs, "get", "gitprovider", providerName,
				"-o", "jsonpath={.status.signingPublicKey}")
			g.Expect(err).NotTo(HaveOccurred())
			statusKey = strings.TrimSpace(output)
			g.Expect(statusKey).To(HavePrefix("ssh-"))
		}).Should(Succeed())
		// status.signingPublicKey must match the BYOK public key we provisioned.
		Expect(normalizeAuthorizedKey(statusKey)).To(Equal(normalizeAuthorizedKey(publicKeyStr)),
			"GitProvider.status.signingPublicKey should mirror the BYOK public key")

		By("creating GitTarget and WatchRule")
		createGitTarget(destName, testNs, providerName, commitPath, "main")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		watchRuleData := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{watchRuleName, testNs, destName}
		Expect(applyFromTemplate("test/e2e/templates/watchrule.tmpl", watchRuleData, testNs)).To(Succeed())
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

		By("triggering a per-event commit")
		_, err = kubectlRunInNamespace(testNs, "create", "configmap", cmName, "--from-literal=key=byok-value")
		Expect(err).NotTo(HaveOccurred())

		By("waiting for a signed BYOK commit to land")
		var commitHash string
		Eventually(func(g Gomega) {
			_, pullErr := gitRun(signingRepo.CheckoutDir, "pull")
			g.Expect(pullErr).NotTo(HaveOccurred())

			h, hErr := latestCommitHashForPath(signingRepo.CheckoutDir, commitPath)
			g.Expect(hErr).NotTo(HaveOccurred())
			g.Expect(h).NotTo(BeEmpty(), "expected a commit in %s", commitPath)

			rawObj, rawErr := gitRun(signingRepo.CheckoutDir, "cat-file", "commit", h)
			g.Expect(rawErr).NotTo(HaveOccurred())
			g.Expect(rawObj).To(ContainSubstring("-----BEGIN SSH SIGNATURE-----"))
			commitHash = h
		}, "60s", "3s").Should(Succeed())

		By("verifying the BYOK commit locally")
		assertLocalSSHVerification(signingRepo.CheckoutDir, commitHash, publicKeyStr, signingCommitterEmail)

		By("verifying the BYOK commit through the Gitea commit API")
		assertGiteaVerified(signingRepo.RepoName, commitHash)
	})

	// ── Test 3: per-event commit message template and committer identity ────

	It("should use custom committer identity and per-event message template", func() {
		providerName := "signing-committer-template"
		destName := providerName + "-dest"
		watchRuleName := providerName + "-wr"
		cmName := "signing-test-cm-template"
		commitPath := "e2e/signing-committer"

		customName := "E2E Bot"
		customEmail := "e2e-bot@example.com"
		customTemplate := "e2e: {{.Operation}} {{.Resource}}/{{.Name}}"

		DeferCleanup(func() {
			_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
			cleanupWatchRule(watchRuleName, testNs)
			cleanupGitTarget(destName, testNs)
			_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
		})

		By("creating a GitProvider with custom committer and per-event message template")
		data := signingGitProviderData{
			Name:                providerName,
			Namespace:           testNs,
			RepoURL:             signingRepo.RepoURLHTTP,
			Branch:              "main",
			SecretName:          signingRepo.GitSecretHTTP,
			CommitterName:       customName,
			CommitterEmail:      customEmail,
			MessageTemplate:     customTemplate,
			BatchTemplate:       "reconcile: sync {{.Count}} resources",
			SigningSecretName:   "signing-key-committer",
			GenerateWhenMissing: true,
		}
		Expect(applyFromTemplate("test/e2e/templates/gitprovider-signing.tmpl", data, testNs)).To(Succeed())
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")

		createGitTarget(destName, testNs, providerName, commitPath, "main")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		watchRuleData := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{watchRuleName, testNs, destName}
		Expect(applyFromTemplate("test/e2e/templates/watchrule.tmpl", watchRuleData, testNs)).To(Succeed())
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

		_, err := kubectlRunInNamespace(testNs, "create", "configmap", cmName, "--from-literal=key=template-test")
		Expect(err).NotTo(HaveOccurred())

		By("waiting for commit and verifying committer identity and message template")
		Eventually(func(g Gomega) {
			_, pullErr := gitRun(signingRepo.CheckoutDir, "pull")
			g.Expect(pullErr).NotTo(HaveOccurred())

			logLine, logErr := gitRun(signingRepo.CheckoutDir, "log", "-1", "--format=%cn|%ce|%s", "--", commitPath)
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(logLine)).NotTo(BeEmpty(), "expected a commit in %s", commitPath)

			parts := strings.SplitN(strings.TrimSpace(logLine), "|", 3)
			g.Expect(parts).To(HaveLen(3))
			g.Expect(parts[0]).To(Equal(customName), "committer name should match configured value")
			g.Expect(parts[1]).To(Equal(customEmail), "committer email should match configured value")
			g.Expect(parts[2]).To(HavePrefix("e2e:"), "commit subject should use custom template prefix")
			g.Expect(parts[2]).To(ContainSubstring("configmaps"), "commit subject should include resource type")
		}, "60s", "3s").Should(Succeed())
	})

	// ── Test 4: batch/atomic commit uses batch message template ─────────────

	It("should produce a batch commit with the custom batch message template", func() {
		providerName := "signing-batch"
		destName := providerName + "-dest"
		watchRuleName := providerName + "-wr"
		commitPath := "e2e/signing-batch"
		customBatchTemplate := "e2e-batch: synced {{.Count}} resources to {{.GitTarget}}"

		DeferCleanup(func() {
			for i := range 3 {
				_, _ = kubectlRunInNamespace(testNs, "delete", "configmap",
					fmt.Sprintf("batch-cm-%d", i), "--ignore-not-found=true")
			}
			cleanupWatchRule(watchRuleName, testNs)
			cleanupGitTarget(destName, testNs)
			_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
		})

		By("pre-creating ConfigMaps that the reconciler will pick up as a batch")
		for i := range 3 {
			name := fmt.Sprintf("batch-cm-%d", i)
			_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", name, "--ignore-not-found=true")
			_, err := kubectlRunInNamespace(testNs, "create", "configmap", name,
				fmt.Sprintf("--from-literal=index=%d", i))
			Expect(err).NotTo(HaveOccurred())
		}

		data := signingGitProviderData{
			Name:                providerName,
			Namespace:           testNs,
			RepoURL:             signingRepo.RepoURLHTTP,
			Branch:              "main",
			SecretName:          signingRepo.GitSecretHTTP,
			CommitterName:       signingCommitterName,
			CommitterEmail:      signingCommitterEmail,
			MessageTemplate:     "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}",
			BatchTemplate:       customBatchTemplate,
			SigningSecretName:   "signing-key-batch",
			GenerateWhenMissing: true,
		}
		Expect(applyFromTemplate("test/e2e/templates/gitprovider-signing.tmpl", data, testNs)).To(Succeed())
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")

		createGitTarget(destName, testNs, providerName, commitPath, "main")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		watchRuleData := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{watchRuleName, testNs, destName}
		Expect(applyFromTemplate("test/e2e/templates/watchrule.tmpl", watchRuleData, testNs)).To(Succeed())
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

		By("waiting for the batch commit and verifying its message uses the batch template")
		Eventually(func(g Gomega) {
			_, pullErr := gitRun(signingRepo.CheckoutDir, "pull")
			g.Expect(pullErr).NotTo(HaveOccurred())

			logOutput, logErr := gitRun(signingRepo.CheckoutDir, "log", "--format=%s", "--", commitPath)
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(logOutput).To(ContainSubstring("e2e-batch:"),
				"expected a batch commit with the custom batch template in path %s", commitPath)
		}, "90s", "3s").Should(Succeed())
	})
})

// ── helpers specific to the signing suite ────────────────────────────────────

// extractSSHSigBlock extracts the -----BEGIN/END SSH SIGNATURE----- block from a
// raw git commit object (as produced by `git cat-file commit <hash>`).
// Git indents continuation lines of the gpgsig header with a single space,
// which is stripped so the block is valid PEM for ssh-keygen.
func extractSSHSigBlock(commitRaw string) string {
	const begin = "-----BEGIN SSH SIGNATURE-----"
	const end = "-----END SSH SIGNATURE-----"

	startIdx := strings.Index(commitRaw, begin)
	if startIdx < 0 {
		return ""
	}
	endIdx := strings.Index(commitRaw[startIdx:], end)
	if endIdx < 0 {
		return ""
	}

	block := commitRaw[startIdx : startIdx+endIdx+len(end)]
	var out strings.Builder
	for _, line := range strings.Split(block, "\n") {
		out.WriteString(strings.TrimPrefix(line, " "))
		out.WriteByte('\n')
	}
	return strings.TrimRight(out.String(), "\n")
}

// removeGpgsigHeader removes the gpgsig header and its continuation lines from a
// raw git commit object, producing the payload that was signed.
func removeGpgsigHeader(commitRaw string) string {
	var out strings.Builder
	skip := false
	for _, segment := range strings.SplitAfter(commitRaw, "\n") {
		line := strings.TrimSuffix(segment, "\n")
		hasTrailingNewline := strings.HasSuffix(segment, "\n")

		if strings.HasPrefix(line, "gpgsig ") {
			skip = true
			continue
		}
		if skip && strings.HasPrefix(line, " ") {
			continue
		}
		skip = false
		out.WriteString(line)
		if hasTrailingNewline {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

// sshKeygenVerify runs `ssh-keygen -Y verify` with the commit payload on stdin
// and returns the combined output.
func sshKeygenVerify(allowedSignersFile, identity, sigFile, payloadFile string) (string, error) {
	payload, err := os.ReadFile(payloadFile)
	if err != nil {
		return "", fmt.Errorf("read payload file: %w", err)
	}

	cmd := exec.Command("ssh-keygen",
		"-Y", "verify",
		"-f", allowedSignersFile,
		"-I", identity,
		"-n", "git",
		"-s", sigFile,
	)
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func gitVerifyCommit(repoDir, allowedSignersFile, commitHash string) (string, error) {
	cmd := exec.Command(
		"git",
		"-c", "gpg.format=ssh",
		"-c", fmt.Sprintf("gpg.ssh.allowedSignersFile=%s", allowedSignersFile),
		"verify-commit",
		commitHash,
	)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
