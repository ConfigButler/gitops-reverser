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
	"github.com/ConfigButler/gitops-reverser/internal/giteaclient"
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
	EventTemplate       string
	ReconcileTemplate   string
	SigningSecretName   string
	GenerateWhenMissing bool
}

// signingRepo holds the file-local repo fixtures for the Commit Signing describe block.
var signingRepo *RepoArtifacts

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
		Expect(signingRepo.User).NotTo(BeNil(), "expected SetupRepo to populate a dedicated Gitea user")
	})

	AfterAll(func() {
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(90 * time.Second)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	// ── Test 1: generated signing key — local + Gitea verification ──────────

	It("should produce per-event commits verifiable locally and by Gitea (generated key)",
		func() {
			gitea := giteaTestInstance()
			providerName := "signing-per-event"
			signingSecretName := "signing-key-per-event"
			destName := providerName + "-dest"
			watchRuleName := providerName + "-wr"
			cmName := "signing-test-cm-per-event"
			commitPath := "e2e/signing-per-event"

			DeferCleanup(func() {
				if skipCleanupBecauseResourcesArePreserved(
					fmt.Sprintf("commit signing resources for GitProvider %s/%s", testNs, providerName),
					testNs,
				) {
					return
				}
				_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
				cleanupWatchRule(watchRuleName, testNs)
				cleanupGitTarget(destName, testNs)
				cleanupNamespacedResource(testNs, "gitprovider", providerName)
			})

			By("creating a GitProvider with commit signing enabled (generateWhenMissing)")
			committerName, committerEmail := signingRepoCommitter()
			data := signingGitProviderData{
				Name:                providerName,
				Namespace:           testNs,
				RepoURL:             signingRepo.RepoURLHTTP,
				Branch:              "main",
				SecretName:          signingRepo.GitSecretHTTP,
				CommitterName:       committerName,
				CommitterEmail:      committerEmail,
				EventTemplate:       "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}",
				ReconcileTemplate:   "reconciled {{.Count}} {{.Resource}}",
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
			registered, err := gitea.RegisterSigningPublicKey(signingRepo.User, signingPublicKey,
				"e2e-signing-generated-"+providerName)
			Expect(err).NotTo(HaveOccurred())
			Expect(registered).NotTo(BeNil())
			Expect(registered.ID).To(BeNumerically(">", 0))

			By("verifying the generated signing key in Gitea")
			privateKeyPEM, err := signingPrivateKeyFromSecret(testNs, signingSecretName)
			Expect(err).NotTo(HaveOccurred())
			Expect(verifySigningPublicKeyInGitea(
				signingRepo.User,
				signingPublicKey,
				registered.Fingerprint,
				privateKeyPEM,
			)).To(Succeed())

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
			}, "90s", "3s").Should(Succeed())

			By("verifying the commit locally with ssh-keygen and git verify-commit")
			assertLocalSSHVerification(signingRepo.CheckoutDir, commitHash, signingPublicKey, committerEmail)

			By("verifying the commit through the Gitea commit API")
			assertGiteaVerified(signingRepo.RepoName, commitHash, committerEmail)
		})

	// ── Test 2: BYOK signing — user-provided key, local + Gitea verification ─

	It("should produce per-event commits verifiable locally and by Gitea (BYOK)", Label("byok"), func() {
		gitea := giteaTestInstance()
		providerName := "signing-byok"
		signingSecretName := "signing-key-byok"
		destName := providerName + "-dest"
		watchRuleName := providerName + "-wr"
		cmName := "signing-test-cm-byok"
		commitPath := "e2e/signing-byok"

		DeferCleanup(func() {
			if skipCleanupBecauseResourcesArePreserved(
				fmt.Sprintf("commit signing resources for GitProvider %s/%s", testNs, providerName),
				testNs,
			) {
				return
			}
			_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
			cleanupWatchRule(watchRuleName, testNs)
			cleanupGitTarget(destName, testNs)
			cleanupNamespacedResource(testNs, "gitprovider", providerName)
			cleanupNamespacedResource(testNs, "secret", signingSecretName)
		})

		By("generating a BYOK SSH signing keypair")
		privateKeyPEM, publicKey, err := reverserGit.GenerateSSHSigningKeyPair(nil)
		Expect(err).NotTo(HaveOccurred())
		publicKeyStr := strings.TrimSpace(string(publicKey))
		Expect(publicKeyStr).To(HavePrefix("ssh-"))

		By("creating the signing Secret from the BYOK keypair")
		applySigningSecret(testNs, signingSecretName, privateKeyPEM, publicKey)

		By("registering the BYOK signing public key with Gitea")
		registered, err := gitea.RegisterSigningPublicKey(
			signingRepo.User,
			publicKeyStr,
			"e2e-signing-byok-"+providerName,
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(registered).NotTo(BeNil())

		By("verifying the BYOK signing key in Gitea")
		Expect(verifySigningPublicKeyInGitea(
			signingRepo.User,
			publicKeyStr,
			registered.Fingerprint,
			privateKeyPEM,
		)).To(Succeed())

		By("creating a GitProvider that consumes the BYOK signing Secret (generateWhenMissing=false)")
		committerName, committerEmail := signingRepoCommitter()
		data := signingGitProviderData{
			Name:                providerName,
			Namespace:           testNs,
			RepoURL:             signingRepo.RepoURLHTTP,
			Branch:              "main",
			SecretName:          signingRepo.GitSecretHTTP,
			CommitterName:       committerName,
			CommitterEmail:      committerEmail,
			EventTemplate:       "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}",
			ReconcileTemplate:   "reconciled {{.Count}} {{.Resource}}",
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
		Expect(giteaclient.NormalizeAuthorizedKey(statusKey)).To(
			Equal(giteaclient.NormalizeAuthorizedKey(publicKeyStr)),
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
		}, "90s", "3s").Should(Succeed())

		By("verifying the BYOK commit locally")
		assertLocalSSHVerification(signingRepo.CheckoutDir, commitHash, publicKeyStr, committerEmail)

		By("verifying the BYOK commit through the Gitea commit API")
		assertGiteaVerified(signingRepo.RepoName, commitHash, committerEmail)
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
			if skipCleanupBecauseResourcesArePreserved(
				fmt.Sprintf("commit signing resources for GitProvider %s/%s", testNs, providerName),
				testNs,
			) {
				return
			}
			_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
			cleanupWatchRule(watchRuleName, testNs)
			cleanupGitTarget(destName, testNs)
			cleanupNamespacedResource(testNs, "gitprovider", providerName)
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
			EventTemplate:       customTemplate,
			ReconcileTemplate:   "reconciled {{.Count}} {{.Resource}}",
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
		// Scope the assertion to the ConfigMap's OWN file, and look for the per-event ("e2e:") commit
		// anywhere in its history — not just the single latest commit on the whole path. The WatchRule
		// also watches secrets, so when the target is created the namespace's pre-existing secrets get a
		// one-time INITIAL BACKFILL ("reconciled N secrets") whose commit can legitimately land after the
		// per-event ConfigMap commit. That backfill touches a different file, so scoping to the ConfigMap
		// file (and matching the per-event commit explicitly) asserts the per-event behaviour without
		// being fragile to that acceptable setup-time ordering.
		cmFile := fmt.Sprintf("%s/v1/configmaps/%s/%s.yaml", commitPath, testNs, cmName)
		Eventually(func(g Gomega) {
			_, pullErr := gitRun(signingRepo.CheckoutDir, "pull")
			g.Expect(pullErr).NotTo(HaveOccurred())

			logOut, logErr := gitRun(signingRepo.CheckoutDir, "log", "--format=%cn|%ce|%s", "--", cmFile)
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(logOut)).NotTo(BeEmpty(), "expected a commit for the ConfigMap %s", cmFile)

			var found bool
			for _, line := range strings.Split(strings.TrimSpace(logOut), "\n") {
				parts := strings.SplitN(strings.TrimSpace(line), "|", 3)
				if len(parts) != 3 || !strings.HasPrefix(parts[2], "e2e:") {
					continue // tolerate a later heal/reconcile commit on the same file
				}
				g.Expect(parts[0]).To(Equal(customName), "committer name should match configured value")
				g.Expect(parts[1]).To(Equal(customEmail), "committer email should match configured value")
				g.Expect(parts[2]).To(ContainSubstring("configmaps"), "commit subject should include resource type")
				found = true
			}
			g.Expect(found).To(BeTrue(),
				"a per-event 'e2e:' commit for the ConfigMap must exist with the custom committer identity")
			// 90s: signing does the slowest per-event work (commit + SSH sign +
			// push); under Ginkgo parallelism the shared controller can push this
			// past 60s on a contended run.
		}, "90s", "3s").Should(Succeed())
	})

	// ── Test 4: batch/atomic commit uses batch message template ─────────────

	It("should produce a reconcile commit with the custom reconcile message template", func() {
		providerName := "signing-reconcile"
		destName := providerName + "-dest"
		watchRuleName := providerName + "-wr"
		commitPath := "e2e/signing-reconcile"
		// The reconcile template names the synced type ({{.APIVersion}}/{{.Resource}}) and pins the
		// {{.Revision}} so the per-type splice commits are self-describing — the §9 "name the synced
		// type" improvement in docs/design/stream/signing-snapshot-tail-replay-failure-investigation.md.
		customReconcileTemplate := "e2e-reconcile: synced {{.Count}} {{.APIVersion}}/{{.Resource}}" +
			"@{{.Revision}} to {{.GitTarget}}"

		DeferCleanup(func() {
			if skipCleanupBecauseResourcesArePreserved(
				fmt.Sprintf("commit signing resources for GitProvider %s/%s", testNs, providerName),
				testNs,
			) {
				return
			}
			for i := range 3 {
				_, _ = kubectlRunInNamespace(testNs, "delete", "configmap",
					fmt.Sprintf("batch-cm-%d", i), "--ignore-not-found=true")
			}
			cleanupWatchRule(watchRuleName, testNs)
			cleanupGitTarget(destName, testNs)
			cleanupNamespacedResource(testNs, "gitprovider", providerName)
		})

		By("pre-creating ConfigMaps that the reconciler will pick up as a batch")
		for i := range 3 {
			name := fmt.Sprintf("batch-cm-%d", i)
			_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", name, "--ignore-not-found=true")
			_, err := kubectlRunInNamespace(testNs, "create", "configmap", name,
				fmt.Sprintf("--from-literal=index=%d", i))
			Expect(err).NotTo(HaveOccurred())
		}

		committerName, committerEmail := signingRepoCommitter()
		data := signingGitProviderData{
			Name:                providerName,
			Namespace:           testNs,
			RepoURL:             signingRepo.RepoURLHTTP,
			Branch:              "main",
			SecretName:          signingRepo.GitSecretHTTP,
			CommitterName:       committerName,
			CommitterEmail:      committerEmail,
			EventTemplate:       "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}",
			ReconcileTemplate:   customReconcileTemplate,
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

		By("recreating the GitTarget now that the WatchRule is active to force a fresh reconcile batch")
		cleanupGitTarget(destName, testNs)
		createGitTarget(destName, testNs, providerName, commitPath, "main")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		By("waiting for the batch commit and verifying its message uses the batch template")
		Eventually(func(g Gomega) {
			_, pullErr := gitRun(signingRepo.CheckoutDir, "pull")
			g.Expect(pullErr).NotTo(HaveOccurred())

			latestHash, latestErr := latestCommitHashForPath(signingRepo.CheckoutDir, commitPath)
			g.Expect(latestErr).NotTo(HaveOccurred())
			g.Expect(latestHash).NotTo(BeEmpty(), "expected a commit in %s", commitPath)

			subject, subjectErr := gitRun(signingRepo.CheckoutDir, "show", "-s", "--format=%s", latestHash)
			g.Expect(subjectErr).NotTo(HaveOccurred())
			subject = strings.TrimSpace(subject)
			g.Expect(subject).To(HavePrefix("e2e-reconcile:"),
				"expected latest commit in %s to use the configured template", commitPath)
			g.Expect(subject).NotTo(HavePrefix("["),
				"expected latest commit in %s not to use the per-event template", commitPath)

			logOutput, logErr := gitRun(signingRepo.CheckoutDir, "log", "--format=%s", "--", commitPath)
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(logOutput).NotTo(ContainSubstring("["),
				"expected reconcile path %s not to contain per-event template subjects", commitPath)
			// The per-type splice names its type, so the configmaps batch is committed as a
			// self-describing "synced N v1/configmaps" reconcile rather than an anonymous count.
			g.Expect(logOutput).To(ContainSubstring("v1/configmaps"),
				"expected reconcile subjects in %s to name the synced type (§9 improvement)", commitPath)
		}, "30s", "3s").Should(Succeed())
	})

	// ── Test 5: late-joining target suppresses already-reconciled tail entries ──
	//
	// User-path guard for the per-(GitTarget, GVR) coverage watermark
	// (signing-snapshot-tail-replay-failure-investigation.md §8.1). Two GitTargets track the same
	// v1/configmaps into different paths off one commitWindow:0s signing provider. Target A starts
	// the shared ConfigMap tail; target B joins later. The strict red-first proof is the unit test
	// (internal/watch TestAuditTailFanout_*); a full e2e cannot classify the overlap band without an
	// observable Hc or a tail-pause hook, so this only asserts the robust user-visible guarantees:
	// the pre-existing seed set is reconcile-only for B (no per-event subject), content converges
	// under B regardless of which path delivered it, and live events created after B is Synced still
	// flow as per-event commits. It deliberately does NOT wait for A to commit the overlap band
	// before B exists — that consumes the entries before B joins and erases the very bug path (§8.1).
	It("should not replay already-reconciled configmaps as per-event commits to a late-joining target", func() {
		providerName := "signing-overlap"
		watchRuleNameA := providerName + "-wr-a"
		watchRuleNameB := providerName + "-wr-b"
		destNameA := providerName + "-a"
		destNameB := providerName + "-b"
		commitPathA := "e2e/signing-overlap/a"
		commitPathB := "e2e/signing-overlap/b"

		const seedCount = 40
		seedNames := make([]string, seedCount)
		for i := range seedNames {
			seedNames[i] = fmt.Sprintf("seed-cm-%04d", i)
		}
		overlapNames := make([]string, 20)
		for i := range overlapNames {
			overlapNames[i] = fmt.Sprintf("overlap-b-cm-%02d", i)
		}
		liveNames := []string{"live-b-cm-0", "live-b-cm-1", "live-b-cm-2"}

		DeferCleanup(func() {
			if skipCleanupBecauseResourcesArePreserved(
				fmt.Sprintf("commit signing overlap resources for GitProvider %s/%s", testNs, providerName),
				testNs,
			) {
				return
			}
			_, _ = kubectlRunInNamespace(testNs, "delete", "configmap",
				"-l", "app.kubernetes.io/part-of=signing-overlap", "--ignore-not-found=true")
			cleanupWatchRule(watchRuleNameA, testNs)
			cleanupWatchRule(watchRuleNameB, testNs)
			cleanupGitTarget(destNameA, testNs)
			cleanupGitTarget(destNameB, testNs)
			cleanupNamespacedResource(testNs, "gitprovider", providerName)
		})

		By("pre-creating the seed ConfigMaps before any target exists")
		_, err := kubectlRunWithStdin(testNs, signingOverlapConfigMapsManifest(testNs, seedNames),
			"apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "failed to create seed configmaps")

		By("creating the signing GitProvider (commitWindow 0s) with the reconcile/per-event templates")
		committerName, committerEmail := signingRepoCommitter()
		Expect(applyFromTemplate("test/e2e/templates/gitprovider-signing.tmpl", signingGitProviderData{
			Name:           providerName,
			Namespace:      testNs,
			RepoURL:        signingRepo.RepoURLHTTP,
			Branch:         "main",
			SecretName:     signingRepo.GitSecretHTTP,
			CommitterName:  committerName,
			CommitterEmail: committerEmail,
			EventTemplate:  "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}",
			ReconcileTemplate: "e2e-reconcile: synced {{.Count}} {{.APIVersion}}/{{.Resource}}" +
				"@{{.Revision}} to {{.GitTarget}}",
			SigningSecretName:   "signing-key-overlap",
			GenerateWhenMissing: true,
		}, testNs)).To(Succeed())
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")

		By("creating target A and its WatchRule, then waiting for A to reconcile the seed band")
		createGitTarget(destNameA, testNs, providerName, commitPathA, "main")
		verifyResourceStatus("gittarget", destNameA, testNs, "True", "Ready", "")
		Expect(applyFromTemplate("test/e2e/templates/watchrule.tmpl", struct {
			Name, Namespace, DestinationName string
		}{watchRuleNameA, testNs, destNameA}, testNs)).To(Succeed())
		verifyResourceStatus("watchrule", watchRuleNameA, testNs, "True", "Ready", "")
		waitForGitTargetSynced(destNameA, testNs)

		By("proving the shared ConfigMap tail is live for A via a probe per-event commit")
		_, err = kubectlRunInNamespace(testNs, "create", "configmap", "probe-a", "--from-literal=k=v")
		Expect(err).NotTo(HaveOccurred())
		_, _ = kubectlRunInNamespace(testNs, "label", "configmap", "probe-a",
			"app.kubernetes.io/part-of=signing-overlap", "--overwrite")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, signingRepo.CheckoutDir)
			logOut, logErr := gitRun(signingRepo.CheckoutDir, "log", "--format=%s", "--", commitPathA)
			g.Expect(logErr).NotTo(HaveOccurred())
			g.Expect(logOut).To(ContainSubstring("[CREATE] v1/configmaps/probe-a"),
				"target A's per-event tail must be live before B joins\n%s",
				recentCommitDiagnostics(signingRepo.CheckoutDir, commitPathA))
		}).Should(Succeed())

		By("creating the overlap band and IMMEDIATELY target B + its WatchRule (widen the join window)")
		_, err = kubectlRunWithStdin(testNs, signingOverlapConfigMapsManifest(testNs, overlapNames),
			"apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "failed to create overlap-b configmaps")
		createGitTarget(destNameB, testNs, providerName, commitPathB, "main")
		Expect(applyFromTemplate("test/e2e/templates/watchrule.tmpl", struct {
			Name, Namespace, DestinationName string
		}{watchRuleNameB, testNs, destNameB}, testNs)).To(Succeed())
		verifyResourceStatus("gittarget", destNameB, testNs, "True", "Ready", "")
		verifyResourceStatus("watchrule", watchRuleNameB, testNs, "True", "Ready", "")
		waitForGitTargetSynced(destNameB, testNs)

		By("asserting seed + overlap content converges under path B with no seed leaked as a per-event commit")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, signingRepo.CheckoutDir)
			files, lsErr := gitRun(signingRepo.CheckoutDir, "ls-files", "--", commitPathB)
			g.Expect(lsErr).NotTo(HaveOccurred())
			commitDiagnostics := recentCommitDiagnostics(signingRepo.CheckoutDir, commitPathB)
			for _, n := range seedNames {
				g.Expect(files).To(ContainSubstring("/"+n+".yaml"),
					"seed %s must be present under path B\n%s",
					n, commitDiagnostics)
			}
			for _, n := range overlapNames {
				g.Expect(files).To(ContainSubstring("/"+n+".yaml"),
					"overlap %s must be present under path B\n%s",
					n, commitDiagnostics)
			}
			logOut, logErr := gitRun(signingRepo.CheckoutDir, "log", "--format=%s", "--", commitPathB)
			g.Expect(logErr).NotTo(HaveOccurred())
			// A seed object name can only reach a commit SUBJECT through the per-event template
			// ("[CREATE] .../seed-cm-…"); the reconcile template names the TYPE, never an object. So
			// any "seed-cm-" in the subject log is a re-delivered historical entry — the bug this fix
			// closes.
			g.Expect(logOut).NotTo(ContainSubstring("seed-cm-"),
				"a pre-existing seed object must never appear as a per-event commit under path B\n%s",
				commitDiagnostics)
		}).Should(Succeed())

		By("creating the live-b band after B is Synced and asserting it flows as per-event commits")
		_, err = kubectlRunWithStdin(testNs, signingOverlapConfigMapsManifest(testNs, liveNames),
			"apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "failed to create live-b configmaps")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, signingRepo.CheckoutDir)
			logOut, logErr := gitRun(signingRepo.CheckoutDir, "log", "--format=%s", "--", commitPathB)
			g.Expect(logErr).NotTo(HaveOccurred())
			for _, n := range liveNames {
				g.Expect(logOut).To(ContainSubstring("[CREATE] v1/configmaps/"+n),
					"a post-Synced create must reach path B as a live per-event commit\n%s",
					recentCommitDiagnostics(signingRepo.CheckoutDir, commitPathB))
			}
		}).Should(Succeed())
	})
})

// signingOverlapConfigMapsManifest builds a multi-document ConfigMap manifest for the overlap guard,
// each labelled app.kubernetes.io/part-of=signing-overlap for one-shot delete-by-label cleanup.
func signingOverlapConfigMapsManifest(namespace string, names []string) string {
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b,
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: %s\n"+
				"  labels:\n    app.kubernetes.io/part-of: signing-overlap\ndata:\n  k: v\n---\n",
			n, namespace)
	}
	return b.String()
}

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

func signingRepoCommitter() (string, string) {
	Expect(signingRepo).NotTo(BeNil(), "expected signing repo fixtures to be initialised")
	Expect(signingRepo.User).NotTo(BeNil(), "expected signing repo fixtures to include a Gitea user")
	Expect(strings.TrimSpace(signingRepo.User.Login)).NotTo(BeEmpty(), "expected signing repo user login")
	Expect(strings.TrimSpace(signingRepo.User.Email)).NotTo(BeEmpty(), "expected signing repo user email")
	return signingRepo.User.Login, signingRepo.User.Email
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
