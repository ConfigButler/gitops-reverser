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
)

// This spec proves the manifestedit in-place editing path end to end: when a
// document in Git carries hand-authored formatting (here a YAML comment) that the
// operator did not produce, a later cluster update is applied as a minimal
// in-place edit that preserves the comment, rather than a wholesale rewrite. See
// docs/future/manifestedit-integration-readonly-reconcile.md.
var _ = Describe("Manager In-Place Manifest Editing", Label("manager", "inplace-edit"), Ordered, func() {
	var (
		testNs   string
		repo     *RepoArtifacts
		destName = "inplace-edit-dest"
		ruleName = "inplace-edit-rule"
		cmName   = "inplace-demo"
		gitPath  = "e2e/inplace-edit"
	)

	const preservedComment = "# gitops-reverser-e2e: preserve-this-comment"

	configMapRepoPath := func() string {
		return filepath.Join(gitPath, fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, cmName))
	}

	BeforeAll(func() {
		By("creating the in-place-edit test namespace")
		testNs = testNamespaceFor("manager-inplace-edit")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent

		By("setting up Gitea repo and credentials")
		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("e2e-inplace-edit-%d", GinkgoRandomSeed()))

		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")

		// createGitTarget references the shared sops-age-key secret for its
		// EncryptionConfigured gate; without it the GitTarget never reaches Ready.
		applySOPSAgeKeyToNamespace(testNs)

		By("creating the GitProvider")
		createGitProviderWithURLInNamespace("inplace-edit-provider", testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		verifyResourceStatus("gitprovider", "inplace-edit-provider", testNs, "True", "Ready", "")
	})

	AfterAll(func() {
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
		cleanupWatchRule(ruleName, testNs)
		cleanupGitTarget(destName, testNs)
		cleanupNamespace(testNs)
	})

	It("preserves a hand-authored comment when a watched ConfigMap is updated", func() {
		By("creating the GitTarget and a ConfigMap WatchRule")
		createGitTarget(destName, testNs, "inplace-edit-provider", gitPath, "main")
		err := applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", struct {
			Name            string
			Namespace       string
			DestinationName string
		}{Name: ruleName, Namespace: testNs, DestinationName: destName}, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to apply ConfigMap WatchRule")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")
		verifyResourceStatus("watchrule", ruleName, testNs, "True", "Ready", "")

		// Let any in-flight reconciles from prior specs settle before our event.
		time.Sleep(5 * time.Second)

		By("creating the ConfigMap with color=blue")
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
		_, err = kubectlRunInNamespace(testNs, "create", "configmap", cmName, "--from-literal=color=blue")
		Expect(err).NotTo(HaveOccurred(), "ConfigMap creation should succeed")

		By("waiting for the operator to commit the canonical ConfigMap file")
		fullPath := filepath.Join(repo.CheckoutDir, configMapRepoPath())
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			content, readErr := os.ReadFile(fullPath)
			g.Expect(readErr).NotTo(HaveOccurred(), "ConfigMap file must exist at %s", fullPath)
			g.Expect(string(content)).To(ContainSubstring("color: blue"))
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		By("seeding a hand-authored comment into the committed file (semantically identical)")
		// Adding only a comment keeps the document semantically equal to the
		// cluster object, so the operator leaves it untouched until a real change.
		seedCommentIntoRepoFile(repo, testNs, configMapRepoPath(), preservedComment)

		By("confirming the comment is on the remote and survives until a change")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			content, readErr := os.ReadFile(fullPath)
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(string(content)).To(ContainSubstring(preservedComment))
		}, 60*time.Second, 2*time.Second).Should(Succeed())
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			content, readErr := os.ReadFile(fullPath)
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(string(content)).To(ContainSubstring(preservedComment),
				"a comment-only change is semantically equal, so the operator must not rewrite the file")
		}, 15*time.Second, 3*time.Second).Should(Succeed())

		By("updating the ConfigMap to color=green to trigger an in-place edit")
		_, err = kubectlRunInNamespace(testNs, "patch", "configmap", cmName,
			"--type=merge", "--patch", `{"data":{"color":"green"}}`)
		Expect(err).NotTo(HaveOccurred(), "ConfigMap patch should succeed")

		By("verifying the update was applied in place: comment preserved AND value updated")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			content, readErr := os.ReadFile(fullPath)
			g.Expect(readErr).NotTo(HaveOccurred())
			body := string(content)
			g.Expect(body).To(ContainSubstring("color: green"), "the changed value must be written")
			g.Expect(body).To(ContainSubstring(preservedComment),
				"the hand-authored comment must survive the in-place edit")
			g.Expect(body).NotTo(ContainSubstring("color: blue"))
		}, 90*time.Second, 3*time.Second).Should(Succeed())

		By("✅ in-place edit preserved hand-authored formatting through a live update")
	})
})

// seedCommentIntoRepoFile inserts a YAML comment under the data block of the
// committed manifest and pushes it to main, authenticating the local checkout's
// origin from the GitTarget's Git Secret. It retries once over a remote race by
// rebasing on origin/main, mirroring how a human would resolve a concurrent push.
func seedCommentIntoRepoFile(repo *RepoArtifacts, namespace, relPath, comment string) {
	GinkgoHelper()

	username, password := inplaceReadGitCredentials(namespace, repo.GitSecretHTTP)
	originOut, err := gitRun(repo.CheckoutDir, "remote", "get-url", "origin")
	Expect(err).NotTo(HaveOccurred(), "failed to read origin URL")
	parsed, err := url.Parse(strings.TrimSpace(originOut))
	Expect(err).NotTo(HaveOccurred(), "failed to parse origin URL")
	parsed.User = url.UserPassword(username, password)

	mustGit := func(args ...string) {
		out, gitErr := gitRun(repo.CheckoutDir, args...)
		Expect(gitErr).NotTo(HaveOccurred(), fmt.Sprintf("git %s: %s", strings.Join(args, " "), out))
	}

	mustGit("remote", "set-url", "origin", parsed.String())
	mustGit("fetch", "origin", "main")
	mustGit("checkout", "-B", "main", "origin/main")
	mustGit("reset", "--hard", "origin/main")

	full := filepath.Join(repo.CheckoutDir, relPath)
	content, readErr := os.ReadFile(full)
	Expect(readErr).NotTo(HaveOccurred(), "committed file must exist before seeding a comment")
	// Attach the comment as the head comment of the data block's first key.
	seeded := strings.Replace(string(content), "data:\n", "data:\n  "+comment+"\n", 1)
	Expect(seeded).NotTo(Equal(string(content)), "expected to insert the comment under data:")
	Expect(os.WriteFile(full, []byte(seeded), 0o600)).To(Succeed())

	mustGit("add", relPath)
	mustGit("commit", "-m", "e2e: seed hand-authored comment")
	if out, pushErr := gitRun(repo.CheckoutDir, "push", "origin", "HEAD:main"); pushErr != nil {
		// Lost a race with the operator's own push: rebase on the new tip and retry.
		_ = out
		mustGit("fetch", "origin", "main")
		mustGit("rebase", "origin/main")
		mustGit("push", "origin", "HEAD:main")
	}
}

// inplaceReadGitCredentials reads username/password from a GitTarget Git Secret.
func inplaceReadGitCredentials(namespace, secretName string) (string, string) {
	GinkgoHelper()
	output, err := kubectlRunInNamespace(namespace, "get", "secret", secretName, "-o", "json")
	Expect(err).NotTo(HaveOccurred(), "failed to fetch Git Secret")

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
