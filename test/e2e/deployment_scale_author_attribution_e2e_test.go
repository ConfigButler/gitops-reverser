// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Deployment scale author attribution proves the one job the surviving /scale
// special-case still does: name *who* scaled a Deployment. A `kubectl scale`
// never touches the Deployment object directly — it writes the autoscaling/v1
// Scale subresource (deployments/scale), and the apiserver mirrors the accepted
// replica count onto the parent's spec.replicas and bumps the parent's
// resourceVersion. Object state therefore arrives through the *parent* Deployment
// watch (see deployment_scale_subresource_e2e_test.go), while the audit event
// lands under deployments/scale. The attribution handler forwards that one
// subresource and keys the fact under the parent GVR at the scale response RV, so
// the resolver joins it to the parent's MODIFIED watch event and the resulting
// commit is authored by the human who ran the scale.
//
// kubectl's CLI cannot set user.extra, so this drives a *real* `kubectl scale`
// impersonating a bare username (no OIDC display-name/email claims). The author is
// therefore the Kubernetes username itself, with a safe constructed email
// (username@noreply.cluster.local) — the OIDC-claims enrichment is covered
// separately by commit_author_attribution_e2e_test.go.
var _ = Describe("Deployment scale author attribution", Label("manager", "subresource"), func() {
	It("attributes a kubectl scale commit to the human who ran it", func() {
		if committerOnlyModeEnabled() {
			Skip("watch-first committer-only mode has no audit facts for author attribution")
		}

		testNs := testNamespaceFor("scale-author")
		seed := GinkgoRandomSeed()
		repoName := fmt.Sprintf("e2e-scale-author-%d", seed)
		providerName := "scale-author-provider"
		targetName := "scale-author-target"
		watchRuleName := "scale-author-watchrule"
		targetPath := "e2e/scale-author"
		deploymentName := "scale-author-target-deploy"

		// The human who runs `kubectl scale`. With no OIDC claims in user.extra the
		// commit author is this username verbatim, and authorEmail falls back to
		// ConstructSafeEmail(username, "cluster.local").
		const scaleUser = "grace-hopper"
		expectedAuthor := fmt.Sprintf("%s <%s@noreply.cluster.local>", scaleUser, scaleUser)

		By("creating the scale-author test namespace")
		_, _ = kubectlRun("create", "namespace", testNs)

		By("setting up a dedicated Gitea repo and credentials")
		repo := SetupRepo(resolveE2EContext(), testNs, repoName)
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		defer cleanupNamespace(testNs)

		By("creating GitProvider (immediate commit window), GitTarget and a Deployment WatchRule")
		// A 0s commit window commits every watched event immediately, so the scale's
		// MODIFIED event becomes its own commit and never coalesces with another's.
		createReadyGitProvider(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		createValidatedGitTarget(targetName, testNs, providerName, targetPath)
		applyDeploymentWatchRule(testNs, watchRuleName, targetName)
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

		// Authorship only flows through the per-event audit tail. A scale issued
		// before the live stream is running would land in the unattributed baseline
		// splice, so gate on StreamsRunning before producing the attributed write.
		waitForStreamsRunning(targetName, testNs)

		By("creating a Deployment with replicas=1 and waiting for it to land in git")
		applyScaleTestDeployment(testNs, deploymentName, 1)
		osDeploymentFile := filepath.Join(
			repo.CheckoutDir,
			targetPath,
			fmt.Sprintf("apps/v1/deployments/%s/%s.yaml", testNs, deploymentName),
		)
		Eventually(func(g Gomega) {
			g.Expect(committedDeploymentReplicas(g, repo.CheckoutDir, osDeploymentFile)).To(Equal(int64(1)))
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		By(fmt.Sprintf("scaling the Deployment to replicas=3 via `kubectl scale` impersonating %q", scaleUser))
		// A literal `kubectl scale deployment ... --replicas=3 --as=<human>`: this
		// writes the deployments/scale subresource under the hood. system:masters
		// keeps the impersonated scale authorized without per-user RBAC.
		_, err = kubectlRunInNamespace(
			testNs, "scale", "deployment", deploymentName, "--replicas=3",
			"--as="+scaleUser, "--as-group=system:masters",
		)
		Expect(err).NotTo(HaveOccurred(), "impersonated kubectl scale should succeed")

		repoPath := path.Join(targetPath, fmt.Sprintf("apps/v1/deployments/%s/%s.yaml", testNs, deploymentName))

		By("verifying the parent Deployment manifest is updated AND the commit is authored by the scaler")
		Eventually(func(g Gomega) {
			// The replica change proves the scale flowed through the parent Deployment
			// watch — the value never comes from the scale subresource body.
			g.Expect(committedDeploymentReplicas(g, repo.CheckoutDir, osDeploymentFile)).To(Equal(int64(3)))

			// Scope the author read to this Deployment's unique path so it is
			// unambiguously the commit produced by this scale, independent of any
			// concurrent audit traffic from other specs.
			out, logErr := gitRun(repo.CheckoutDir, "log", "-1", "--pretty=%an <%ae>", "--", repoPath)
			g.Expect(logErr).NotTo(HaveOccurred(), fmt.Sprintf("git log author failed: %s", out))
			g.Expect(strings.TrimSpace(out)).To(
				Equal(expectedAuthor),
				"the scale commit must be authored by the impersonated human who ran kubectl scale")
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(targetName, testNs)
	})
})
