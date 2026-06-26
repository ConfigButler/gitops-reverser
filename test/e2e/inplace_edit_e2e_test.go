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
	"io/fs"
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
// docs/design/manifest/manifestedit-integration-readonly-reconcile.md.
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
		waitForStreamsReady(destName, testNs)

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

var _ = Describe(
	"Manager Manifest Folder Editing",
	Label("manager", "inplace-edit", "manifest-folder"),
	Ordered,
	func() {
		var (
			testNs       string
			repo         *RepoArtifacts
			providerName = "manifest-folder-provider"
			destName     = "manifest-folder-dest"
			ruleName     = "manifest-folder-rule"
			gitPath      = "e2e/manifest-folder"
		)

		const (
			fixtureRoot            = "test/e2e/fixtures/inplace-edit-folder"
			bundleComment          = "# e2e-folder-edit: preserve bundle data comment"
			nestedComment          = "# e2e-folder-edit: preserve nested data comment"
			bundleConfigMapName    = "folder-bundle"
			nestedConfigMapName    = "folder-nested"
			siblingConfigMapName   = "folder-sibling"
			bundleRepoPath         = "apply/bundle.yaml"
			nestedRepoPath         = "apply/nested/sidecar.yaml"
			kustomizationRepoPath  = "kustomization.yaml"
			manifestFolderRepoName = "e2e-manifest-folder"
		)

		BeforeAll(func() {
			By("creating the manifest-folder test namespace")
			testNs = testNamespaceFor("manager-manifest-folder")
			_, _ = kubectlRun("create", "namespace", testNs)

			By("setting up Gitea repo and credentials")
			repo = SetupRepo(
				resolveE2EContext(),
				testNs,
				fmt.Sprintf("%s-%d", manifestFolderRepoName, GinkgoRandomSeed()),
			)

			_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
			Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")

			applySOPSAgeKeyToNamespace(testNs)

			By("creating the GitProvider")
			createGitProviderWithURLInNamespace(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
			verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")
		})

		AfterAll(func() {
			for _, name := range []string{bundleConfigMapName, nestedConfigMapName, siblingConfigMapName} {
				_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", name, "--ignore-not-found=true")
			}
			cleanupWatchRule(ruleName, testNs)
			cleanupGitTarget(destName, testNs)
			_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
			cleanupNamespace(testNs)
		})

		It("edits existing manifests in a real folder without breaking sibling files", func() {
			renderedFixture := renderInPlaceFixtureFolder(fixtureRoot, testNs)
			DeferCleanup(func() { _ = os.RemoveAll(renderedFixture) })

			By("seeding the Git repository with the rendered manifest folder")
			seedRenderedFolderIntoRepo(repo, testNs, renderedFixture, gitPath)

			By("applying the rendered fixture folder with Kustomize")
			_, err := kubectlRunInNamespace(testNs, "apply", "-k", renderedFixture)
			Expect(err).NotTo(HaveOccurred(), "failed to apply rendered fixture kustomization")

			By("creating the GitTarget and ConfigMap WatchRule")
			createGitTarget(destName, testNs, providerName, gitPath, "main")
			err = applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", struct {
				Name            string
				Namespace       string
				DestinationName string
			}{Name: ruleName, Namespace: testNs, DestinationName: destName}, testNs)
			Expect(err).NotTo(HaveOccurred(), "failed to apply ConfigMap WatchRule")
			verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")
			verifyResourceStatus("watchrule", ruleName, testNs, "True", "Ready", "")
			waitForStreamsReady(destName, testNs)

			By("patching ConfigMaps that live in a multi-document file and a nested folder")
			_, err = kubectlRunInNamespace(testNs, "patch", "configmap", bundleConfigMapName,
				"--type=merge", "--patch", `{"data":{"color":"green"}}`)
			Expect(err).NotTo(HaveOccurred(), "failed to patch bundle ConfigMap")

			_, err = kubectlRunInNamespace(testNs, "patch", "configmap", nestedConfigMapName,
				"--type=merge", "--patch", `{"data":{"mode":"loud"}}`)
			Expect(err).NotTo(HaveOccurred(), "failed to patch nested ConfigMap")

			By("verifying the existing files were edited in place and sibling content survived")
			bundleFullPath := filepath.Join(repo.CheckoutDir, gitPath, bundleRepoPath)
			nestedFullPath := filepath.Join(repo.CheckoutDir, gitPath, nestedRepoPath)
			kustomizationFullPath := filepath.Join(repo.CheckoutDir, gitPath, kustomizationRepoPath)
			renderedKustomization := filepath.Join(renderedFixture, kustomizationRepoPath)

			Eventually(func(g Gomega) {
				pullLatestRepoState(g, repo.CheckoutDir)

				bundleBody := readRepoFile(g, bundleFullPath)
				g.Expect(bundleBody).To(ContainSubstring(bundleComment))
				g.Expect(bundleBody).To(ContainSubstring("name: " + bundleConfigMapName))
				g.Expect(bundleBody).To(ContainSubstring("color: green"))
				g.Expect(bundleBody).NotTo(ContainSubstring("color: blue"))
				g.Expect(bundleBody).To(ContainSubstring("name: " + siblingConfigMapName))
				g.Expect(bundleBody).To(ContainSubstring("role: untouched"))
				g.Expect(bundleBody).To(ContainSubstring("note: still-here"))
				g.Expect(bundleBody).NotTo(ContainSubstring("namespace:"))

				nestedBody := readRepoFile(g, nestedFullPath)
				g.Expect(nestedBody).To(ContainSubstring(nestedComment))
				g.Expect(nestedBody).To(ContainSubstring("name: " + nestedConfigMapName))
				g.Expect(nestedBody).To(ContainSubstring("mode: loud"))
				g.Expect(nestedBody).NotTo(ContainSubstring("mode: quiet"))
				g.Expect(nestedBody).NotTo(ContainSubstring("namespace:"))

				kustomizationBody := readRepoFile(g, kustomizationFullPath)
				g.Expect(kustomizationBody).To(Equal(readRepoFile(g, renderedKustomization)))

				for _, name := range []string{bundleConfigMapName, nestedConfigMapName} {
					canonicalPath := filepath.Join(repo.CheckoutDir, gitPath, "v1", "configmaps", testNs, name+".yaml")
					_, statErr := os.Stat(canonicalPath)
					g.Expect(os.IsNotExist(statErr)).
						To(BeTrue(), "must not create canonical duplicate %s", canonicalPath)
				}
			}, 120*time.Second, 3*time.Second).Should(Succeed())

			By("✅ fixture-backed manifest folder was edited in place")
		})
	},
)

// seedCommentIntoRepoFile inserts a YAML comment under the data block of the
// committed manifest and pushes it to main, authenticating the local checkout's
// origin from the GitTarget's Git Secret. It retries once over a remote race by
// rebasing on origin/main, mirroring how a human would resolve a concurrent push.
func seedCommentIntoRepoFile(repo *RepoArtifacts, namespace, relPath, comment string) {
	GinkgoHelper()

	configureRepoOriginWithCredentials(repo, namespace)

	mustGit := func(args ...string) {
		out, gitErr := gitRun(repo.CheckoutDir, args...)
		Expect(gitErr).NotTo(HaveOccurred(), fmt.Sprintf("git %s: %s", strings.Join(args, " "), out))
	}

	if _, err := gitRun(repo.CheckoutDir, "fetch", "origin", "main"); err == nil {
		mustGit("checkout", "-B", "main", "origin/main")
		mustGit("reset", "--hard", "origin/main")
	} else {
		mustGit("checkout", "--orphan", "main")
		_, _ = gitRun(repo.CheckoutDir, "rm", "-rf", ".")
	}

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

func renderInPlaceFixtureFolder(fixtureRoot, namespace string) string {
	GinkgoHelper()

	rendered, err := os.MkdirTemp("", "gitops-reverser-e2e-manifest-folder-*")
	Expect(err).NotTo(HaveOccurred(), "failed to create rendered fixture directory")

	err = filepath.WalkDir(fixtureRoot, func(src string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(fixtureRoot, src)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}

		dst := filepath.Join(rendered, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o750)
		}

		content, readErr := os.ReadFile(src)
		if readErr != nil {
			return readErr
		}
		content = []byte(strings.ReplaceAll(string(content), "__E2E_NAMESPACE__", namespace))
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		return os.WriteFile(dst, content, 0o600)
	})
	Expect(err).NotTo(HaveOccurred(), "failed to render fixture folder")

	return rendered
}

func seedRenderedFolderIntoRepo(repo *RepoArtifacts, namespace, renderedFolder, gitPath string) {
	GinkgoHelper()

	configureRepoOriginWithCredentials(repo, namespace)
	mustGit := func(args ...string) {
		out, gitErr := gitRun(repo.CheckoutDir, args...)
		Expect(gitErr).NotTo(HaveOccurred(), fmt.Sprintf("git %s: %s", strings.Join(args, " "), out))
	}

	if _, err := gitRun(repo.CheckoutDir, "fetch", "origin", "main"); err == nil {
		mustGit("checkout", "-B", "main", "origin/main")
		mustGit("reset", "--hard", "origin/main")
	} else {
		mustGit("checkout", "--orphan", "main")
		_, _ = gitRun(repo.CheckoutDir, "rm", "-rf", ".")
	}

	dest := filepath.Join(repo.CheckoutDir, gitPath)
	Expect(os.RemoveAll(dest)).To(Succeed())
	Expect(copyFixtureDir(renderedFolder, dest)).To(Succeed())

	mustGit("add", gitPath)
	mustGit("commit", "-m", "e2e: seed manifest folder fixture")
	mustGit("push", "origin", "HEAD:main")
}

func copyFixtureDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o750)
		}

		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o600)
	})
}

func readRepoFile(g Gomega, path string) string {
	GinkgoHelper()

	content, err := os.ReadFile(path)
	g.Expect(err).NotTo(HaveOccurred(), "expected repo file %s to exist", path)
	return string(content)
}

func configureRepoOriginWithCredentials(repo *RepoArtifacts, namespace string) {
	GinkgoHelper()

	username, password := inplaceReadGitCredentials(namespace, repo.GitSecretHTTP)
	originOut, err := gitRun(repo.CheckoutDir, "remote", "get-url", "origin")
	Expect(err).NotTo(HaveOccurred(), "failed to read origin URL")
	parsed, err := url.Parse(strings.TrimSpace(originOut))
	Expect(err).NotTo(HaveOccurred(), "failed to parse origin URL")
	parsed.User = url.UserPassword(username, password)

	out, err := gitRun(repo.CheckoutDir, "remote", "set-url", "origin", parsed.String())
	Expect(err).NotTo(HaveOccurred(), "failed to configure authenticated origin: %s", out)
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
