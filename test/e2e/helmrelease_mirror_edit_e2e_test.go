// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Higher-level KRM objects are first-class documents. This spec proves the
// mirror/edit pipeline treats a Flux HelmRelease — a control-plane custom
// resource, the canonical "install an app" object — exactly like a core
// resource: it is mirrored to Git on create, and a live field edit (a chart
// version bump, launch use case 2) round-trips into the mirrored file in place,
// preserving hand-authored formatting. The generic-CRD case is already pinned by
// crd_lifecycle_e2e_test.go; this pins a real, named higher-level type.
// See docs/design/support-boundary/finished/higher-level-krm-documents.md.
var _ = Describe("Manager Higher-Level KRM (HelmRelease)",
	Label("manager", "higher-level-krm"), Ordered, func() {
		var (
			testNs       string
			repo         *RepoArtifacts
			providerName = "f7-helmrelease-provider"
			destName     = "f7-helmrelease-dest"
			ruleName     = "f7-helmrelease-rule"
			hrName       = "podinfo"
			gitPath      = "e2e/f7-helmrelease"
		)

		const (
			initialVersion   = "6.5.0"
			bumpedVersion    = "6.6.0"
			preservedComment = "# gitops-reverser-e2e: preserve-this-helmrelease-comment"
			repoName         = "e2e-f7-helmrelease"
		)

		// helmReleaseRepoPath is the canonical mirror path for a grouped custom
		// resource: <gitPath>/<namespace>/<group>/<plural>/<name>.yaml.
		helmReleaseRepoPath := func() string {
			return filepath.Join(gitPath, testNs, "helm.toolkit.fluxcd.io", "helmreleases", hrName+".yaml")
		}

		BeforeAll(func() {
			By("creating the f7-helmrelease test namespace")
			testNs = testNamespaceFor("manager-f7-helmrelease")
			_, _ = kubectlRun("create", "namespace", testNs)

			By("setting up Gitea repo and credentials")
			repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("%s-%d", repoName, GinkgoRandomSeed()))

			_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
			Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")

			applySOPSAgeKeyToNamespace(testNs)

			By("creating the GitProvider")
			createGitProviderWithURLInNamespace(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
			verifyResourceStatus("gitprovider", providerName, testNs, "True", "Succeeded", "")
		})

		AfterAll(func() {
			_, _ = kubectlRunInNamespace(testNs, "delete", "helmrelease", hrName, "--ignore-not-found=true")
			cleanupWatchRule(ruleName, testNs)
			cleanupGitTarget(destName, testNs)
			_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
			cleanupNamespace(testNs)
		})

		It("mirrors a HelmRelease and round-trips a chart-version bump in place", func() {
			By("creating the GitTarget and a HelmRelease WatchRule")
			createGitTarget(destName, testNs, providerName, gitPath, "main")
			err := applyFromTemplate("test/e2e/templates/manager/watchrule-helmrelease.tmpl", struct {
				Name            string
				Namespace       string
				DestinationName string
			}{Name: ruleName, Namespace: testNs, DestinationName: destName}, testNs)
			Expect(err).NotTo(HaveOccurred(), "failed to apply HelmRelease WatchRule")
			verifyResourceCondition("gittarget", destName, testNs, "Validated", "True", "Succeeded", "")
			verifyResourceStatus("watchrule", ruleName, testNs, "True", "Succeeded", "")
			waitForStreamsRunning(destName, testNs)

			By(fmt.Sprintf("creating the HelmRelease with chart version %s", initialVersion))
			err = applyFromTemplate("test/e2e/templates/manager/helmrelease.tmpl", struct {
				Name      string
				Namespace string
				Version   string
			}{Name: hrName, Namespace: testNs, Version: initialVersion}, testNs)
			Expect(err).NotTo(HaveOccurred(), "failed to apply HelmRelease")

			By("waiting for the operator to mirror the HelmRelease to its canonical path")
			fullPath := filepath.Join(repo.CheckoutDir, helmReleaseRepoPath())
			Eventually(func(g Gomega) {
				pullLatestRepoState(g, repo.CheckoutDir)
				content, readErr := os.ReadFile(fullPath)
				g.Expect(readErr).NotTo(HaveOccurred(), "HelmRelease file must exist at %s", fullPath)
				body := string(content)
				g.Expect(body).To(ContainSubstring("kind: HelmRelease"))
				g.Expect(body).To(ContainSubstring(initialVersion), "the initial chart version must be mirrored")
				g.Expect(body).NotTo(ContainSubstring("status:"), "status must never be committed")
			}, 90*time.Second, 3*time.Second).Should(Succeed())

			By("seeding a hand-authored comment into the committed file")
			seedHeadCommentIntoRepoFile(repo, testNs, helmReleaseRepoPath(), preservedComment)
			Eventually(func(g Gomega) {
				pullLatestRepoState(g, repo.CheckoutDir)
				content, readErr := os.ReadFile(fullPath)
				g.Expect(readErr).NotTo(HaveOccurred())
				g.Expect(string(content)).To(ContainSubstring(preservedComment))
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By(fmt.Sprintf("bumping the live chart version to %s to trigger an in-place edit", bumpedVersion))
			_, err = kubectlRunInNamespace(testNs, "patch", "helmrelease", hrName,
				"--type=merge", "--patch",
				fmt.Sprintf(`{"spec":{"chart":{"spec":{"version":"%s"}}}}`, bumpedVersion))
			Expect(err).NotTo(HaveOccurred(), "HelmRelease patch should succeed")

			By("verifying the edit landed in place: new version written, comment preserved, old version gone")
			Eventually(func(g Gomega) {
				pullLatestRepoState(g, repo.CheckoutDir)
				content, readErr := os.ReadFile(fullPath)
				g.Expect(readErr).NotTo(HaveOccurred())
				body := string(content)
				g.Expect(body).To(ContainSubstring(bumpedVersion), "the bumped chart version must be written")
				g.Expect(body).To(ContainSubstring(preservedComment),
					"the hand-authored comment must survive the in-place edit")
				g.Expect(body).NotTo(ContainSubstring(initialVersion))
			}, 90*time.Second, 3*time.Second).Should(Succeed())

			By("✅ HelmRelease mirrored and edited in place like any KRM document")
		})
	})

// seedHeadCommentIntoRepoFile prepends a hand-authored comment as the document
// head comment of the mirrored file and pushes it to main, authenticating the
// local checkout's origin from the GitTarget's Git Secret. It retries once over
// a remote race by rebasing on origin/main. A document head comment is the most
// layout-agnostic anchor: it survives an in-place field edit because the editor
// only rewrites changed nodes.
func seedHeadCommentIntoRepoFile(repo *RepoArtifacts, namespace, relPath, comment string) {
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
	seeded := comment + "\n" + string(content)
	Expect(os.WriteFile(full, []byte(seeded), 0o600)).To(Succeed())

	mustGit("add", relPath)
	mustGit("commit", "-m", "e2e: seed hand-authored comment on HelmRelease")
	if _, pushErr := gitRun(repo.CheckoutDir, "push", "origin", "HEAD:main"); pushErr != nil {
		mustGit("fetch", "origin", "main")
		mustGit("rebase", "origin/main")
		mustGit("push", "origin", "HEAD:main")
	}
}
