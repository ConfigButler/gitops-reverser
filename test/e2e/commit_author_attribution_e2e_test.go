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
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Commit Author Attribution proves the end-to-end identity chain: a write made
// by an impersonated user whose audit event carries OIDC name/email claims in
// user.extra ends up as the Git commit author. The kube-apiserver emits the
// real audit event, the controller's audit pipeline consumes it, and the
// resulting commit must be attributed to that identity. The author-mapping
// *logic* is unit-tested (internal/queue redis_audit_consumer_test.go
// TestResolveUserInfo); this spec is the only place that proves the full
// apiserver → audit webhook → consumer → git-author wiring carries the keys
// through.
//
// Not Serial: the dedicated GitProvider uses a 0s commit window, so every
// watched event is committed immediately as its own commit (no batching). Each
// assertion reads the author scoped to its own unique file path, so concurrent
// audit traffic from other specs lands in separate commits and cannot change
// the author of this spec's commit.
var _ = Describe("Commit Author Attribution", Label("manager"), Ordered, func() {
	var (
		testNs        string
		repo          *RepoArtifacts
		gitProvName   string
		gitTargetName string
		watchRuleName string
	)

	const basePath = "e2e/commit-author-test"

	BeforeAll(func() {
		By("creating commit-author test namespace")
		testNs = testNamespaceFor("commit-author")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		By("setting up the Gitea repo and credentials")
		repo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-commit-author-%d", GinkgoRandomSeed()),
		)
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		By("setting up GitProvider (0s commit window), GitTarget and WatchRule")
		seed := GinkgoRandomSeed()
		gitProvName = fmt.Sprintf("commit-author-gitprovider-%d", seed)
		gitTargetName = fmt.Sprintf("commit-author-gittarget-%d", seed)
		watchRuleName = fmt.Sprintf("commit-author-watchrule-%d", seed)

		// createGitProviderWithURLInNamespace uses a 0s commit window, so each
		// event commits immediately and never coalesces with another's.
		createGitProviderWithURLInNamespace(gitProvName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		verifyResourceStatus("gitprovider", gitProvName, testNs, "True", "Ready", "")

		createGitTarget(gitTargetName, testNs, gitProvName, basePath, "main")
		verifyResourceStatus("gittarget", gitTargetName, testNs, "True", "Ready", "")

		data := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{
			Name:            watchRuleName,
			Namespace:       testNs,
			DestinationName: gitTargetName,
		}
		err = applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", data, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to apply WatchRule")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

		// Authorship only flows through the per-event audit tail. A ConfigMap created
		// while the configmaps type is still building its first checkpoint would land in
		// the unattributed baseline splice instead (fallback author), so wait until the
		// claimed type is Synced before producing the impersonated writes.
		waitForGitTargetMaterializationSettled(gitTargetName, testNs, 1)
	})

	AfterAll(func() {
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(gitTargetName, testNs)
		cleanupNamespacedResource(testNs, "gitprovider", gitProvName)
		cleanupNamespace(testNs)
	})

	// assertCommitAuthorFromOIDCClaims creates a ConfigMap while impersonating a
	// user that carries the given OIDC display-name/email claims in user.extra,
	// then asserts the commit for that ConfigMap's unique path is authored by
	// "<display name> <email>".
	assertCommitAuthorFromOIDCClaims := func(cmName, asUser, displayName, email string) {
		GinkgoHelper()

		By(fmt.Sprintf("creating ConfigMap %q while impersonating %q with OIDC name/email extras", cmName, asUser))
		// Impersonation makes the real kube-apiserver emit an audit event whose
		// impersonatedUser.Extra carries these keys, mimicking the claims a
		// structured authentication config maps into user.extra for an OIDC
		// login. system:masters keeps the impersonated create authorized
		// without provisioning per-user RBAC.
		err := createConfigMapAsImpersonatedUser(
			testNs,
			cmName,
			asUser,
			[]string{"system:masters"},
			map[string][]string{
				"configbutler.ai/claims/display-name": {displayName},
				"configbutler.ai/claims/email":        {email},
			},
		)
		Expect(err).NotTo(HaveOccurred(), "failed to create impersonated ConfigMap")

		repoPath := path.Join(basePath, fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, cmName))
		expectedFile := filepath.Join(repo.CheckoutDir, repoPath)

		By("waiting for the commit and asserting its author is the OIDC identity")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)

			info, statErr := os.Stat(expectedFile)
			g.Expect(statErr).NotTo(HaveOccurred(),
				fmt.Sprintf("ConfigMap file should exist at %s", expectedFile))
			g.Expect(info.Size()).To(BeNumerically(">", 0))

			// Scope the log to this ConfigMap's unique path so the author read
			// back is unambiguously the commit produced by this impersonated
			// create, independent of any concurrent audit traffic.
			out, logErr := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%an <%ae>", "--", repoPath)
			g.Expect(logErr).NotTo(HaveOccurred(), fmt.Sprintf("git log author failed: %s", out))
			g.Expect(strings.TrimSpace(out)).To(
				Equal(fmt.Sprintf("%s <%s>", displayName, email)),
				"commit author should be the OIDC display name and email from user.extra")
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up the test ConfigMap")
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
	}

	It("attributes a commit to the OIDC display name and email from user.extra", func() {
		assertCommitAuthorFromOIDCClaims(
			fmt.Sprintf("commit-author-oidc-cm-%d", GinkgoRandomSeed()),
			"oidc-simon",
			"Simon Koudijs",
			"something@configbutler.ai",
		)
	})

	It("attributes a second commit to a different OIDC identity (claims are data-driven)", func() {
		assertCommitAuthorFromOIDCClaims(
			fmt.Sprintf("commit-author-oidc-cm2-%d", GinkgoRandomSeed()),
			"oidc-ada",
			"Ada Lovelace",
			"ada@configbutler.ai",
		)
	})
})
