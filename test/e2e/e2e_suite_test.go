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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purpose of being used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, and installs
// CertManager.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting gitops-reverser integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	if img := os.Getenv("PROJECT_IMAGE"); img == "" {
		By("local run: preparing cluster via Makefile target")
	} else {
		By(fmt.Sprintf("using pre-built image: %s", img))
	}

	By("preparing e2e prerequisites via Makefile target")
	ensureE2EPrepared()
})

var _ = AfterEach(func() {
	dumpFailureDiagnostics()
})

func ensureE2EPrepared() {
	ctx := resolveE2EContext()
	setE2EKubectlContext(ctx)
	ns := resolveE2ENamespace()
	installMode, err := resolveE2EInstallMode()
	Expect(err).NotTo(HaveOccurred(), "INSTALL_MODE environment variable must be set for e2e runs")
	installName := resolveE2EInstallName(ns)
	cmd := makeCommand(
		fmt.Sprintf("CTX=%s", ctx),
		fmt.Sprintf("INSTALL_NAME=%s", installName),
		fmt.Sprintf("INSTALL_MODE=%s", installMode),
		fmt.Sprintf("NAMESPACE=%s", ns),
		"prepare-e2e",
	)
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "failed to run make target for e2e prepare")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s", output)

	By("setting up Gitea repo, credentials and checkout via Makefile target")
	repoName := resolveE2ERepoName()
	cmd = makeCommand(
		fmt.Sprintf("CTX=%s", ctx),
		fmt.Sprintf("NAMESPACE=%s", ns),
		fmt.Sprintf("REPO_NAME=%s", repoName),
		"e2e-gitea-run-setup",
	)
	output, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "failed to run make target for gitea run setup")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s", output)
	exportGiteaArtifacts(ctx, ns, repoName)

	// Some e2e shell scripts call `kubectl` without an explicit `--context` flag. Ensure `kubectl` is
	// pointed at the intended cluster context for the remainder of the test run.
	output, err = kubectlRun("config", "use-context", ctx)
	Expect(err).NotTo(HaveOccurred(), "failed to set kubectl context for e2e run")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s", output)

	By("ensuring IceCreamOrder CRD is removed before tests")
	output, err = kubectlRun(
		"delete", "crd", "icecreamorders.shop.example.com",
		"--ignore-not-found=true",
	)
	Expect(err).NotTo(HaveOccurred(), "failed to delete IceCreamOrder CRD before tests")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s", output)

	// The Makefile prepares the age key under the stamp directory. When running `go test` directly (without
	// `make test-e2e`), ensure the suite uses that prepared key file.
	if strings.TrimSpace(os.Getenv("E2E_AGE_KEY_FILE")) == "" {
		wd, err := os.Getwd()
		Expect(err).NotTo(HaveOccurred(), "failed to get working directory for e2e run")
		ageKeyPath := filepath.Join(wd, ".stamps", "cluster", ctx, "age-key.txt")
		Expect(os.Setenv("E2E_AGE_KEY_FILE", ageKeyPath)).To(Succeed())
	}
}

func resolveE2ERepoName() string {
	if value := strings.TrimSpace(os.Getenv("REPO_NAME")); value != "" {
		return value
	}
	return "e2e-test-" + strconv.FormatInt(GinkgoRandomSeed(), 10)
}

func exportGiteaArtifacts(ctx, namespace, repoName string) {
	projectDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred(), "failed to resolve project directory for gitea artifacts")

	base := filepath.Join(projectDir, ".stamps", "cluster", ctx, namespace, "git-"+repoName)

	activeRepoBytes, err := os.ReadFile(filepath.Join(base, "active-repo.txt"))
	Expect(err).NotTo(HaveOccurred(), "failed to read active repo file")
	activeRepo := strings.TrimSpace(string(activeRepoBytes))
	Expect(activeRepo).NotTo(BeEmpty(), "active repo file must contain a repo name")

	checkoutPathBytes, err := os.ReadFile(filepath.Join(base, "checkout-path.txt"))
	if err != nil && !os.IsNotExist(err) {
		Expect(err).NotTo(HaveOccurred(), "failed to read checkout path file")
	}

	checkoutPath := strings.TrimSpace(string(checkoutPathBytes))
	if checkoutPath == "" {
		checkoutRoot := strings.TrimSpace(os.Getenv("REPOS_DIR"))
		if checkoutRoot == "" {
			checkoutRoot = filepath.Join(projectDir, ".stamps", "repos")
		} else if !filepath.IsAbs(checkoutRoot) {
			checkoutRoot = filepath.Join(projectDir, checkoutRoot)
		}

		checkoutPath = filepath.Join(checkoutRoot, activeRepo)
	}

	_, err = os.Stat(filepath.Join(checkoutPath, ".git"))
	Expect(err).NotTo(HaveOccurred(), "expected checkout to exist for active repo")

	Expect(os.Setenv("E2E_REPO_NAME", activeRepo)).To(Succeed())
	Expect(os.Setenv("E2E_CHECKOUT_DIR", checkoutPath)).To(Succeed())
	Expect(os.Setenv("E2E_GIT_SECRET_HTTP", resolveE2EHTTPSecretName(activeRepo))).To(Succeed())
	Expect(os.Setenv("E2E_GIT_SECRET_SSH", resolveE2ESSHSecretName(activeRepo))).To(Succeed())
	Expect(os.Setenv("E2E_GIT_SECRET_INVALID", resolveE2EInvalidSecretName(activeRepo))).To(Succeed())
	// Propagate the resolved namespace so quickstartSetupNamespace() uses the same
	// namespace as the install (where the operator and secrets live).
	Expect(os.Setenv("SUT_NAMESPACE", namespace)).To(Succeed())
}

func resolveE2EHTTPSecretName(repoName string) string {
	if value := strings.TrimSpace(os.Getenv("E2E_GIT_SECRET_HTTP")); value != "" {
		return value
	}
	if strings.TrimSpace(repoName) == "" {
		return "git-creds"
	}
	return "git-creds-" + repoName
}

func resolveE2ESSHSecretName(repoName string) string {
	if value := strings.TrimSpace(os.Getenv("E2E_GIT_SECRET_SSH")); value != "" {
		return value
	}
	if strings.TrimSpace(repoName) == "" {
		return "git-creds-ssh"
	}
	return "git-creds-ssh-" + repoName
}

func resolveE2EInvalidSecretName(repoName string) string {
	if value := strings.TrimSpace(os.Getenv("E2E_GIT_SECRET_INVALID")); value != "" {
		return value
	}
	if strings.TrimSpace(repoName) == "" {
		return "git-creds-invalid"
	}
	return "git-creds-invalid-" + repoName
}

func makeCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("make", args...)
	_, _ = fmt.Fprintf(
		GinkgoWriter,
		"make invocation: args=%v\n",
		args,
	)
	return cmd
}

func resolveE2EInstallName(namespace string) string {
	if value := strings.TrimSpace(os.Getenv("INSTALL_NAME")); value != "" {
		return value
	}
	return namespace
}

func resolveE2EInstallMode() (string, error) {
	if value := strings.TrimSpace(os.Getenv("INSTALL_MODE")); value != "" {
		return value, nil
	}
	return "", errors.New("INSTALL_MODE environment variable must be set")
}

func resolveE2EContext() string {
	if value := strings.TrimSpace(os.Getenv("CTX")); value != "" {
		return value
	}

	output, err := kubectlRun("config", "current-context")
	if err == nil {
		if value := strings.TrimSpace(output); value != "" {
			return value
		}
	}

	return "kind-gitops-reverser-test-e2e"
}
