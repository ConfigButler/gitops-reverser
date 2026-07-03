// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

const (
	playgroundNamespace     = "tilt-playground"
	playgroundRepoName      = "playground"
	playgroundProviderName  = "playground-provider"
	playgroundTargetName    = "playground-target"
	playgroundWatchRuleName = "playground-watchrule"
	playgroundPath          = "live-cluster"
)

type playgroundRun struct {
	namespace     string
	repoName      string
	checkoutDir   string
	repoURL       string
	providerName  string
	targetName    string
	watchRuleName string
	path          string
}

// playgroundRepo holds the reusable repo fixtures for the playground flow.
var playgroundRepo *RepoArtifacts

var _ = Describe("playground", Label("playground"), Ordered, Serial, func() {
	var run playgroundRun

	BeforeAll(func() {
		run = newPlaygroundRun()

		By(fmt.Sprintf("creating or reusing playground namespace %q", run.namespace))
		_, _ = kubectlRun("create", "namespace", run.namespace)

		By(fmt.Sprintf("setting up reusable Gitea repo %q for the playground", run.repoName))
		playgroundRepo = SetupRepo(resolveE2EContext(), run.namespace, run.repoName)

		By("applying Git credentials to the playground namespace")
		_, err := kubectlRunInNamespace(run.namespace, "apply", "-f", playgroundRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply Git secrets to the playground namespace")

		applySOPSAgeKeyToNamespace(run.namespace)
		run.checkoutDir = playgroundRepo.CheckoutDir
		run.repoURL = playgroundRepo.RepoURLHTTP
	})

	It("prepares a reusable repo-backed playground", func() {
		By("asserting the playground repository checkout exists")
		run.assertCheckoutExists()

		By("applying the shared playground starter manifests")
		run.applyResources()

		By("verifying the starter resources become Ready")
		run.verifyResourcesReady()

		By("marking the playground resources for reuse across Tilt sessions")
		preserveNamespace(playgroundNamespace)

		By("printing playground artifacts for the developer")
		run.logArtifacts()
	})
})

func newPlaygroundRun() playgroundRun {
	return playgroundRun{
		namespace:     playgroundNamespace,
		repoName:      playgroundRepoName,
		providerName:  playgroundProviderName,
		targetName:    playgroundTargetName,
		watchRuleName: playgroundWatchRuleName,
		path:          playgroundPath,
	}
}

func (r playgroundRun) assertCheckoutExists() {
	_, err := os.Stat(filepath.Join(r.checkoutDir, ".git"))
	Expect(err).NotTo(HaveOccurred(), "expected reusable playground checkout to exist")
}

func (r playgroundRun) applyResources() {
	projectDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred(), "failed to resolve project dir for playground manifests")

	_, err = kubectlRun("apply", "-k", filepath.Join(projectDir, "test", "playground"))
	Expect(err).NotTo(HaveOccurred(), "failed to apply playground manifests")
}

func (r playgroundRun) verifyResourcesReady() {
	verifyResourceStatus("gitprovider", r.providerName, r.namespace, "True", "Ready", "")
	verifyResourceCondition("gittarget", r.targetName, r.namespace, "Validated", "True", "OK", "")
	verifyResourceStatus("watchrule", r.watchRuleName, r.namespace, "True", "Ready", "")
}

func (r playgroundRun) logArtifacts() {
	_, _ = fmt.Fprintf(
		GinkgoWriter,
		"Playground ready:\n  namespace=%s\n  repo=%s\n  checkout=%s\n  repoURL=%s\n  path=%s\n",
		r.namespace,
		r.repoName,
		r.checkoutDir,
		r.repoURL,
		r.path,
	)
}
