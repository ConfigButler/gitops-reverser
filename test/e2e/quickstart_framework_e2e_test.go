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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

const (
	quickstartFrameworkEnabledEnv = "E2E_ENABLE_QUICKSTART_FRAMEWORK"
	quickstartFrameworkModeEnv    = "E2E_QUICKSTART_MODE"
	quickstartSetupNamespace      = "sut"
)

type quickstartFrameworkRun struct {
	mode             string
	installNamespace string
	testID           string
	repoName         string
	checkoutDir      string
	repoURL          string
	providerName     string
	targetName       string
	watchRuleName    string
	invalidProvName  string
	encryptionName   string
}

var _ = Describe("Quickstart Framework", Label("quickstart-framework"), Ordered, func() {
	var run quickstartFrameworkRun

	BeforeAll(func() {
		if !quickstartFrameworkEnabled() {
			Skip(fmt.Sprintf(
				"quickstart framework is disabled; set %s=true to run",
				quickstartFrameworkEnabledEnv,
			))
		}

		run = newQuickstartFrameworkRun()
	})

	It("sets up quickstart flow via Go framework", func() {
		By("resetting install state for clean quickstart framework execution")
		run.resetInstallState()

		By(fmt.Sprintf("installing controller for quickstart mode %q", run.mode))
		run.installController()
		run.waitForControllerRollout()

		By("creating dedicated Gitea repository and bootstrap credentials")
		run.setupGiteaRepository()

		By("applying quickstart resources from Go")
		run.applyQuickstartResources()
	})

})

func quickstartFrameworkEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(quickstartFrameworkEnabledEnv)))
	return value == "1" || value == "true" || value == "yes"
}

func quickstartFrameworkMode() string {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(quickstartFrameworkModeEnv)))
	if mode == "" {
		return "helm"
	}

	Expect(mode == "config-dir" || mode == "helm" || mode == "plain-manifests-file").To(
		BeTrue(),
		fmt.Sprintf(
			"unsupported %s value %q (expected config-dir|helm|plain-manifests-file)",
			quickstartFrameworkModeEnv,
			mode,
		),
	)
	return mode
}

func newQuickstartFrameworkRun() quickstartFrameworkRun {
	mode := quickstartFrameworkMode()
	testID := strconv.FormatInt(time.Now().UnixNano(), 10)
	repoName := fmt.Sprintf("quickstart-framework-%s-%s", mode, testID)
	checkoutDir := filepath.Join("/tmp/gitops-reverser", repoName)

	return quickstartFrameworkRun{
		mode:             mode,
		installNamespace: quickstartInstallerNamespace(),
		testID:           testID,
		repoName:         repoName,
		checkoutDir:      checkoutDir,
		repoURL:          fmt.Sprintf("http://gitea-http.gitea-e2e.svc.cluster.local:13000/testorg/%s.git", repoName),
		providerName:     fmt.Sprintf("quickstart-provider-%s", testID),
		targetName:       fmt.Sprintf("quickstart-target-%s", testID),
		watchRuleName:    fmt.Sprintf("quickstart-watchrule-%s", testID),
		invalidProvName:  fmt.Sprintf("quickstart-invalid-provider-%s", testID),
		encryptionName:   fmt.Sprintf("quickstart-sops-age-key-%s", testID),
	}
}

func (r *quickstartFrameworkRun) resetInstallState() {
	deleteClusterScopeObjects := [][]string{
		{
			"delete", "clusterrole",
			"gitops-reverser",
			"gitops-reverser-manager-role",
			"gitops-reverser-metrics-reader",
			"gitops-reverser-proxy-role",
			"--ignore-not-found=true",
		},
		{
			"delete", "clusterrolebinding",
			"gitops-reverser",
			"gitops-reverser-manager-rolebinding",
			"gitops-reverser-proxy-rolebinding",
			"--ignore-not-found=true",
		},
		{"delete", "validatingwebhookconfiguration", "gitops-reverser-validating-webhook-configuration",
			"--ignore-not-found=true"},
	}

	for _, args := range deleteClusterScopeObjects {
		cmd := exec.Command("kubectl", args...)
		_, _ = utils.Run(cmd)
	}

	deleteNamespace := exec.Command(
		"kubectl", "delete", "namespace", r.installNamespace, "--wait=true", "--ignore-not-found=true",
	)
	_, _ = utils.Run(deleteNamespace)
}

func (r *quickstartFrameworkRun) installController() {
	cmd := r.installCommand(r.mode)
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to install %s controller via make install", r.mode))
	_, _ = fmt.Fprintf(GinkgoWriter, "%s", output)
}

func (r *quickstartFrameworkRun) installCommand(mode string) *exec.Cmd {
	ctx := resolveE2EContext()
	args := []string{
		fmt.Sprintf("CTX=%s", ctx),
		"install",
		fmt.Sprintf("INSTALL_MODE=%s", mode),
		fmt.Sprintf("NAMESPACE=%s", r.installNamespace),
		fmt.Sprintf("INSTALL_NAMESPACE=%s", r.installNamespace),
		fmt.Sprintf("INSTALL_NAME=%s", r.installNamespace),
	}
	return makeCommandWithSeed(args...)
}

func (r *quickstartFrameworkRun) waitForControllerRollout() {
	rolloutCmd := exec.Command(
		"kubectl", "-n", r.installNamespace, "rollout", "status",
		"deployment/gitops-reverser", "--timeout=120s",
	)
	_, err := utils.Run(rolloutCmd)
	Expect(err).NotTo(HaveOccurred(), "controller rollout did not complete")
}

func quickstartInstallerNamespace() string {
	suiteConfig, _ := GinkgoConfiguration()
	return fmt.Sprintf("run-%d", suiteConfig.RandomSeed)
}

func (r *quickstartFrameworkRun) setupGiteaRepository() {
	cmd := exec.Command("bash", "test/e2e/scripts/setup-gitea.sh", r.repoName, r.checkoutDir)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "failed to bootstrap gitea repository for quickstart")
}

func (r *quickstartFrameworkRun) applyQuickstartResources() {
	createGitProviderWithURL(r.providerName, "main", "git-creds", r.repoURL)

	createGitTargetWithEncryptionOptions(
		r.targetName,
		quickstartSetupNamespace,
		r.providerName,
		"live-cluster",
		"main",
		r.encryptionName,
		true,
	)

	watchRuleData := struct {
		Name            string
		Namespace       string
		DestinationName string
	}{
		Name:            r.watchRuleName,
		Namespace:       quickstartSetupNamespace,
		DestinationName: r.targetName,
	}

	err := applyFromTemplate("test/e2e/templates/watchrule.tmpl", watchRuleData, quickstartSetupNamespace)
	Expect(err).NotTo(HaveOccurred(), "failed to apply quickstart watchrule")

	createGitProviderWithURL(r.invalidProvName, "main", "git-creds-invalid", r.repoURL)
}
