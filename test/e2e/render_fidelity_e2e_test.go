// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	renderFidelityFixtureRoot = "test/e2e/fixtures/render-fidelity"
	renderFidelityGitPath     = "e2e/render-fidelity"
	renderFidelityTimeout     = 120 * time.Second
)

// This is the real external-render-context proof for RenderMatchesLive. Flux postBuild resolves
// ${REGION} in the live Deployment, while gitops-reverser's local Kustomize render must retain the
// source token. The target therefore stalls and must not write either the expansion or a later,
// unrelated live change back to Git. It intentionally does not claim remote-Git repair works: the
// revision detector needed to start that fresh epoch is not implemented yet.
var _ = Describe("Manager Render Fidelity", Label("manager", "render-fidelity", "flux"), Ordered, func() {
	var run renderFidelityRun

	BeforeAll(func() {
		testNs := testNamespaceFor("manager-render-fidelity")
		_, _ = kubectlRun("create", "namespace", testNs)

		repo := SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-render-fidelity-%d", GinkgoRandomSeed()),
		)
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply Git credentials")
		applySOPSAgeKeyToNamespace(testNs)

		run = newRenderFidelityRun(testNs, repo)
		run.checkout.assertCheckoutReady()
	})

	AfterAll(func() {
		run.cleanup()
	})

	It("refuses a real Flux postBuild expansion and preserves the Git source form", func() {
		By("seeding the dedicated Kustomize source fixture")
		fixture := renderInPlaceFixtureFolder(renderFidelityFixtureRoot, run.testNs)
		DeferCleanup(func() { _ = os.RemoveAll(fixture) })
		seedRenderedFolderIntoRepo(run.repo, run.testNs, fixture, renderFidelityGitPath)

		seedHead := remoteBranchHead(Default, run.repo.CheckoutDir)
		Expect(seedHead).NotTo(BeEmpty(), "the source fixture must be committed before Flux reads it")

		By("letting Flux resolve the postBuild variable in the live Deployment")
		run.applyFluxGitRepository()
		run.applyFluxKustomization()
		run.reconcileFluxKustomization()
		run.waitForFluxPostBuildValue()

		By("watching that path with gitops-reverser")
		createReadyGitProvider(run.providerName, run.testNs, run.repo.GitSecretHTTP, run.repo.RepoURLHTTP)
		createValidatedGitTarget(run.targetName, run.testNs, run.providerName, renderFidelityGitPath)
		run.applyDeploymentWatchRule()

		By("reporting the render-vs-live divergence as a stalled target")
		verifyResourceCondition(
			"gittarget", run.targetName, run.testNs,
			"RenderMatchesLive", "False", "RenderDoesNotMatchLive", "${REGION}", "150s",
		)
		verifyResourceCondition(
			"gittarget", run.targetName, run.testNs,
			"Ready", "False", "RenderDoesNotMatchLive", "${REGION}", "150s",
		)
		verifyResourceCondition(
			"gittarget", run.targetName, run.testNs,
			"Stalled", "True", "RenderDoesNotMatchLive", "${REGION}", "150s",
		)
		waitForStreamsRunning(run.targetName, run.testNs)

		By("preserving the unresolved token and creating no reverse-GitOps commit")
		run.consistentlyExpectSourceForm(seedHead)

		By("rejecting a later live edit while the gate remains closed")
		_, err := kubectlRunInNamespace(
			run.testNs,
			"patch",
			"deployment",
			run.deploymentName,
			"--type=merge",
			"--patch={\"spec\":{\"replicas\":2}}",
		)
		Expect(err).NotTo(HaveOccurred(), "failed to patch the live Deployment")
		run.consistentlyExpectSourceForm(seedHead)
		verifyResourceCondition(
			"gittarget", run.targetName, run.testNs,
			"RenderMatchesLive", "False", "RenderDoesNotMatchLive", "${REGION}",
		)

		By("Flux postBuild divergence stalled the target without changing its source tree")
	})
})

type renderFidelityRun struct {
	testNs string
	repo   *RepoArtifacts

	checkout gitCheckout

	deploymentName string
	providerName   string
	targetName     string
	watchRuleName  string

	fluxSecretName        string
	fluxGitRepositoryName string
	fluxKustomizationName string
}

func newRenderFidelityRun(testNs string, repo *RepoArtifacts) renderFidelityRun {
	id := strconv.FormatInt(time.Now().UnixNano(), 10)
	return renderFidelityRun{
		testNs:                testNs,
		repo:                  repo,
		checkout:              newGitCheckout(repo, testNs),
		deploymentName:        "render-fidelity-postbuild",
		providerName:          fmt.Sprintf("render-fidelity-provider-%s", id),
		targetName:            fmt.Sprintf("render-fidelity-target-%s", id),
		watchRuleName:         fmt.Sprintf("render-fidelity-watchrule-%s", id),
		fluxSecretName:        fmt.Sprintf("render-fidelity-auth-%s", id),
		fluxGitRepositoryName: fmt.Sprintf("render-fidelity-repo-%s", id),
		fluxKustomizationName: fmt.Sprintf("render-fidelity-kustomization-%s", id),
	}
}

func (r renderFidelityRun) applyFluxGitRepository() {
	username, password := r.checkout.readGitCredentialSecretDataBase64()
	Expect(applyFromTemplate("test/e2e/templates/bi-directional/flux-gitrepository-http.tmpl", struct {
		Namespace  string
		SecretName string
		Name       string
		RepoURL    string
		Branch     string
		Interval   string
		Username   string
		Password   string
	}{
		Namespace:  "flux-system",
		SecretName: r.fluxSecretName,
		Name:       r.fluxGitRepositoryName,
		RepoURL:    r.repo.RepoURLHTTP,
		Branch:     "main",
		Interval:   "30m",
		Username:   username,
		Password:   password,
	}, "flux-system")).To(Succeed(), "failed to apply Flux GitRepository")
}

func (r renderFidelityRun) applyFluxKustomization() {
	manifest := fmt.Sprintf(`apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %s
  namespace: flux-system
spec:
  interval: 30m
  timeout: 2m
  path: ./%s
  prune: true
  sourceRef:
    kind: GitRepository
    name: %s
  postBuild:
    substitute:
      REGION: us-east
`, r.fluxKustomizationName, renderFidelityGitPath, r.fluxGitRepositoryName)
	_, err := kubectlRunWithStdin("flux-system", manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply Flux Kustomization")
}

func (r renderFidelityRun) reconcileFluxKustomization() {
	args := []string{
		"reconcile", "kustomization", r.fluxKustomizationName,
		"-n", "flux-system", "--with-source", "--timeout", "90s",
	}
	if context := strings.TrimSpace(kubectlContext()); context != "" {
		args = append([]string{"--context", context}, args...)
	}
	command := exec.Command("flux", args...)
	output, err := command.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "flux %s: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
}

func (r renderFidelityRun) waitForFluxPostBuildValue() {
	Eventually(func(g Gomega) {
		value, err := kubectlRunInNamespace(
			r.testNs,
			"get",
			"deployment",
			r.deploymentName,
			"-o=jsonpath={.spec.template.spec.containers[0].env[0].value}",
		)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(value).To(Equal("us-east"))
	}, renderFidelityTimeout, resourceConditionPollInterval).Should(Succeed())
}

func (r renderFidelityRun) applyDeploymentWatchRule() {
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: %s
  namespace: %s
spec:
  targetRef:
    kind: GitTarget
    name: %s
  rules:
    - apiGroups: ["apps"]
      apiVersions: ["v1"]
      resources: ["deployments"]
`, r.watchRuleName, r.testNs, r.targetName)
	_, err := kubectlRunWithStdin(r.testNs, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply Deployment WatchRule")
}

func (r renderFidelityRun) consistentlyExpectSourceForm(seedHead string) {
	Consistently(func(g Gomega) {
		g.Expect(remoteBranchHead(g, r.repo.CheckoutDir)).To(Equal(seedHead), recentCommitDiagnostics(
			r.repo.CheckoutDir, renderFidelityGitPath,
		))
		pullLatestRepoState(g, r.repo.CheckoutDir)
		content := readRepoFile(g, filepath.Join(r.repo.CheckoutDir, renderFidelityGitPath, "deployment.yaml"))
		g.Expect(content).To(ContainSubstring("value: ${REGION}"))
		g.Expect(content).To(ContainSubstring("replicas: 1"))
		g.Expect(content).NotTo(ContainSubstring("replicas: 2"))
	}, 20*time.Second, 2*time.Second).Should(Succeed())
}

func (r renderFidelityRun) cleanup() {
	cleanupWatchRule(r.watchRuleName, r.testNs)
	cleanupGitTarget(r.targetName, r.testNs)
	cleanupNamespacedResource(r.testNs, "gitprovider", r.providerName)
	cleanupNamespacedResource("flux-system", "kustomization", r.fluxKustomizationName)
	cleanupNamespacedResource("flux-system", "gitrepository", r.fluxGitRepositoryName)
	cleanupNamespacedResource("flux-system", "secret", r.fluxSecretName)
	cleanupNamespace(r.testNs)
}
