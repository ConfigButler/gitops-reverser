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
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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

var _ = Describe("Commit Signing", Ordered, func() {
	var testNs string

	BeforeAll(func() {
		testNs = testNamespaceFor("signing")

		By("creating signing test namespace")
		_, _ = kubectlRun("create", "namespace", testNs)

		By("applying git secrets to signing test namespace")
		secretsYaml := strings.TrimSpace(os.Getenv("E2E_SECRETS_YAML"))
		Expect(secretsYaml).NotTo(BeEmpty(), "E2E_SECRETS_YAML must be set by BeforeSuite")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", secretsYaml)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to signing test namespace")
	})

	AfterAll(func() {
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(90 * time.Second)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	// ── Test 1: per-event commit is signed and verifiable by ssh-keygen ──────────

	It("should produce per-event commits signed and verifiable by ssh-keygen", func() {
		providerName := "signing-per-event"
		signingSecretName := "signing-key-per-event"
		destName := providerName + "-dest"
		watchRuleName := providerName + "-wr"
		cmName := "signing-test-cm-per-event"
		commitPath := "e2e/signing-per-event"

		By("creating a GitProvider with commit signing enabled (generateWhenMissing)")
		data := signingGitProviderData{
			Name:                providerName,
			Namespace:           testNs,
			RepoURL:             getRepoURLHTTP(),
			Branch:              "main",
			SecretName:          gitSecretHTTP,
			CommitterName:       "GitOps Reverser",
			CommitterEmail:      "noreply@configbutler.ai",
			MessageTemplate:     "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}",
			BatchTemplate:       "reconcile: sync {{.Count}} resources",
			SigningSecretName:   signingSecretName,
			GenerateWhenMissing: true,
		}
		err := applyFromTemplate("test/e2e/templates/gitprovider-signing.tmpl", data, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to apply signing GitProvider")

		By("waiting for GitProvider to be Ready and SigningPublicKey to appear in status")
		var signingPublicKey string
		Eventually(func(g Gomega) {
			output, err := kubectlRunInNamespace(testNs, "get", "gitprovider", providerName,
				"-o", "jsonpath={.status.signingPublicKey}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(HavePrefix("ssh-"), "signingPublicKey should be populated")
			signingPublicKey = strings.TrimSpace(output)
		}).Should(Succeed())
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")

		By("creating GitTarget and WatchRule")
		createGitTarget(destName, testNs, providerName, commitPath, "main")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		watchRuleData := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{watchRuleName, testNs, destName}
		err = applyFromTemplate("test/e2e/templates/watchrule.tmpl", watchRuleData, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to apply WatchRule")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

		By("creating a ConfigMap to trigger a per-event commit")
		_, err = kubectlRunInNamespace(testNs, "create", "configmap", cmName, "--from-literal=key=signed-value")
		Expect(err).NotTo(HaveOccurred())

		By("waiting for a signed commit to land in the path")
		Eventually(func(g Gomega) {
			_, pullErr := gitRun(checkoutDir, "pull")
			g.Expect(pullErr).NotTo(HaveOccurred())

			commitRaw, catErr := gitRun(checkoutDir, "log", "-1", "--format=%B", "--", commitPath)
			g.Expect(catErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(commitRaw)).NotTo(BeEmpty(), "expected a commit in %s", commitPath)

			// Confirm signature is present in the commit object.
			hash, hashErr := gitRun(checkoutDir, "log", "-1", "--format=%H", "--", commitPath)
			g.Expect(hashErr).NotTo(HaveOccurred())
			rawObj, rawErr := gitRun(checkoutDir, "cat-file", "commit", strings.TrimSpace(hash))
			g.Expect(rawErr).NotTo(HaveOccurred())
			g.Expect(rawObj).To(ContainSubstring("-----BEGIN SSH SIGNATURE-----"),
				"commit should carry an SSH signature")
		}, "60s", "3s").Should(Succeed())

		By("verifying the commit signature with ssh-keygen -Y verify")
		commitHash, err := gitRun(checkoutDir, "log", "-1", "--format=%H", "--", commitPath)
		Expect(err).NotTo(HaveOccurred())
		commitHash = strings.TrimSpace(commitHash)

		commitRaw, err := gitRun(checkoutDir, "cat-file", "commit", commitHash)
		Expect(err).NotTo(HaveOccurred())

		sigBlock := extractSSHSigBlock(commitRaw)
		Expect(sigBlock).To(ContainSubstring("BEGIN SSH SIGNATURE"), "must find SSH signature block")

		tmpDir, err := os.MkdirTemp("", "e2e-signing-*")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = os.RemoveAll(tmpDir) }()

		allowedSigners := fmt.Sprintf("noreply@configbutler.ai namespaces=\"git\" %s\n", signingPublicKey)
		allowedSignersFile := filepath.Join(tmpDir, "allowed-signers")
		Expect(os.WriteFile(allowedSignersFile, []byte(allowedSigners), 0o600)).To(Succeed())

		sigFile := filepath.Join(tmpDir, "commit.sig")
		Expect(os.WriteFile(sigFile, []byte(sigBlock), 0o600)).To(Succeed())

		payload := removeGpgsigHeader(commitRaw)
		payloadFile := filepath.Join(tmpDir, "commit.payload")
		Expect(os.WriteFile(payloadFile, []byte(payload), 0o600)).To(Succeed())

		verifyOut, verifyErr := sshKeygenVerify(allowedSignersFile, "noreply@configbutler.ai", sigFile, payloadFile)
		Expect(verifyErr).NotTo(HaveOccurred(),
			"ssh-keygen -Y verify should succeed.\nOutput: %s", verifyOut)
		Expect(verifyOut).To(ContainSubstring("Good"), "ssh-keygen should report a good signature")

		By("cleaning up")
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(destName, testNs)
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
	})

	// ── Test 2: per-event commit message template and committer identity ──────────

	It("should use custom committer identity and per-event message template", func() {
		providerName := "signing-committer-template"
		destName := providerName + "-dest"
		watchRuleName := providerName + "-wr"
		cmName := "signing-test-cm-template"
		commitPath := "e2e/signing-committer"

		customName := "E2E Bot"
		customEmail := "e2e-bot@example.com"
		// Template renders to e.g. "e2e: CREATE configmaps/signing-test-cm-template"
		customTemplate := "e2e: {{.Operation}} {{.Resource}}/{{.Name}}"

		By("creating a GitProvider with custom committer and per-event message template")
		data := signingGitProviderData{
			Name:                providerName,
			Namespace:           testNs,
			RepoURL:             getRepoURLHTTP(),
			Branch:              "main",
			SecretName:          gitSecretHTTP,
			CommitterName:       customName,
			CommitterEmail:      customEmail,
			MessageTemplate:     customTemplate,
			BatchTemplate:       "reconcile: sync {{.Count}} resources",
			SigningSecretName:   "signing-key-committer",
			GenerateWhenMissing: true,
		}
		err := applyFromTemplate("test/e2e/templates/gitprovider-signing.tmpl", data, testNs)
		Expect(err).NotTo(HaveOccurred())
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")

		By("creating GitTarget and WatchRule")
		createGitTarget(destName, testNs, providerName, commitPath, "main")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		watchRuleData := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{watchRuleName, testNs, destName}
		err = applyFromTemplate("test/e2e/templates/watchrule.tmpl", watchRuleData, testNs)
		Expect(err).NotTo(HaveOccurred())
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

		By("creating a ConfigMap to trigger a per-event commit")
		_, err = kubectlRunInNamespace(testNs, "create", "configmap", cmName, "--from-literal=key=template-test")
		Expect(err).NotTo(HaveOccurred())

		By("waiting for commit and verifying committer identity and message template")
		Eventually(func(g Gomega) {
			_, pullErr := gitRun(checkoutDir, "pull")
			g.Expect(pullErr).NotTo(HaveOccurred())

			// %cn=committer name, %ce=committer email, %s=subject
			logLine, logErr := gitRun(checkoutDir, "log", "-1", "--format=%cn|%ce|%s", "--", commitPath)
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(logLine)).NotTo(BeEmpty(), "expected a commit in %s", commitPath)

			parts := strings.SplitN(strings.TrimSpace(logLine), "|", 3)
			g.Expect(parts).To(HaveLen(3))
			g.Expect(parts[0]).To(Equal(customName), "committer name should match configured value")
			g.Expect(parts[1]).To(Equal(customEmail), "committer email should match configured value")
			// Template: "e2e: {{.Operation}} {{.Resource}}/{{.Name}}"
			g.Expect(parts[2]).To(HavePrefix("e2e:"), "commit subject should use custom template prefix")
			g.Expect(parts[2]).To(ContainSubstring("configmaps"), "commit subject should include resource type")
		}, "60s", "3s").Should(Succeed())

		By("cleaning up")
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(destName, testNs)
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
	})

	// ── Test 3: batch/atomic commit uses batch message template ──────────────────

	It("should produce a batch commit with the custom batch message template", func() {
		providerName := "signing-batch"
		destName := providerName + "-dest"
		watchRuleName := providerName + "-wr"
		commitPath := "e2e/signing-batch"
		customBatchTemplate := "e2e-batch: synced {{.Count}} resources to {{.GitTarget}}"

		// Create resources BEFORE the WatchRule is set up so that the initial reconcile
		// finds them all at once and emits them as a single atomic batch commit.
		By("pre-creating ConfigMaps that the reconciler will pick up as a batch")
		for i := range 3 {
			name := fmt.Sprintf("batch-cm-%d", i)
			_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", name, "--ignore-not-found=true")
			_, err := kubectlRunInNamespace(testNs, "create", "configmap", name,
				fmt.Sprintf("--from-literal=index=%d", i))
			Expect(err).NotTo(HaveOccurred())
		}

		By("creating a GitProvider with a custom batch message template")
		data := signingGitProviderData{
			Name:                providerName,
			Namespace:           testNs,
			RepoURL:             getRepoURLHTTP(),
			Branch:              "main",
			SecretName:          gitSecretHTTP,
			CommitterName:       "GitOps Reverser",
			CommitterEmail:      "noreply@configbutler.ai",
			MessageTemplate:     "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}",
			BatchTemplate:       customBatchTemplate,
			SigningSecretName:   "signing-key-batch",
			GenerateWhenMissing: true,
		}
		err := applyFromTemplate("test/e2e/templates/gitprovider-signing.tmpl", data, testNs)
		Expect(err).NotTo(HaveOccurred())
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")

		By("creating GitTarget and WatchRule — initial reconcile produces a batch commit")
		createGitTarget(destName, testNs, providerName, commitPath, "main")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		watchRuleData := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{watchRuleName, testNs, destName}
		err = applyFromTemplate("test/e2e/templates/watchrule.tmpl", watchRuleData, testNs)
		Expect(err).NotTo(HaveOccurred())
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

		By("waiting for the batch commit and verifying its message uses the batch template")
		Eventually(func(g Gomega) {
			_, pullErr := gitRun(checkoutDir, "pull")
			g.Expect(pullErr).NotTo(HaveOccurred())

			// Walk the full log for the path to find any commit matching the batch template prefix.
			logOutput, logErr := gitRun(checkoutDir, "log", "--format=%s", "--", commitPath)
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(logOutput).To(ContainSubstring("e2e-batch:"),
				"expected a batch commit with the custom batch template in path %s", commitPath)
		}, "90s", "3s").Should(Succeed())

		By("cleaning up")
		for i := range 3 {
			_, _ = kubectlRunInNamespace(testNs, "delete", "configmap",
				fmt.Sprintf("batch-cm-%d", i), "--ignore-not-found=true")
		}
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(destName, testNs)
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
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
	for _, line := range strings.Split(commitRaw, "\n") {
		if strings.HasPrefix(line, "gpgsig ") {
			skip = true
			continue
		}
		if skip && strings.HasPrefix(line, " ") {
			continue
		}
		skip = false
		out.WriteString(line)
		out.WriteByte('\n')
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
