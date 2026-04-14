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
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

// RepoArtifacts holds the file-local Git repository fixtures produced by one
// SetupRepo call. Each e2e test file owns its own instance so that mutable repo
// state is isolated between files.
type RepoArtifacts struct {
	RepoName           string
	RepoURLHTTP        string
	RepoURLSSH         string
	CheckoutDir        string
	SecretsYAML        string
	GitSecretHTTP      string
	GitSecretSSH       string
	GitSecretInvalid   string
	ReceiverWebhookURL string
	ReceiverWebhookID  string
}

// SetupRepo runs `task e2e-gitea-run-setup` for the given context, namespace,
// and repo name, then reads the resulting stamp files and returns a RepoArtifacts
// that callers can store as file-local variables.
//
// ctx should come from resolveE2EContext(). namespace is the test namespace
// used for stamp paths and must already exist in the cluster before this call
// (gitea-run-setup.sh verifies its presence). Shared suite preparation must
// already have provisioned the cluster services, Gitea, and port-forwards.
// Callers are responsible for applying SecretsYAML to the namespace afterward.
func SetupRepo(ctx, namespace, repoName string) *RepoArtifacts {
	By(fmt.Sprintf("setting up Gitea repo %q for namespace %q via Task target", repoName, namespace))
	cmd := taskCommand(
		fmt.Sprintf("CTX=%s", ctx),
		fmt.Sprintf("NAMESPACE=%s", namespace),
		fmt.Sprintf("REPO_NAME=%s", repoName),
		"e2e-gitea-run-setup",
	)
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "failed to run task target for gitea run setup (repo=%s)", repoName)
	_, _ = fmt.Fprintf(GinkgoWriter, "%s", output)

	return readRepoArtifacts(ctx, namespace, repoName)
}

// readRepoArtifacts reads the stamp files written by gitea-run-setup.sh and
// returns a populated RepoArtifacts.
func readRepoArtifacts(ctx, namespace, repoName string) *RepoArtifacts {
	projectDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred(), "failed to resolve project directory")

	base := filepath.Join(projectDir, ".stamps", "cluster", ctx, namespace, "git-"+repoName)

	activeRepoBytes, err := os.ReadFile(filepath.Join(base, "active-repo.txt"))
	Expect(err).NotTo(HaveOccurred(), "failed to read active-repo.txt for repo %s", repoName)
	activeRepo := strings.TrimSpace(string(activeRepoBytes))
	Expect(activeRepo).NotTo(BeEmpty(), "active-repo.txt must not be empty")

	checkoutPath := resolveRepoCheckoutPath(projectDir, base, activeRepo)

	_, err = os.Stat(filepath.Join(checkoutPath, ".git"))
	Expect(err).NotTo(HaveOccurred(), "expected git checkout at %s", checkoutPath)

	a := &RepoArtifacts{
		RepoName:         activeRepo,
		RepoURLHTTP:      fmt.Sprintf(giteaRepoURLTemplate, activeRepo),
		RepoURLSSH:       fmt.Sprintf(giteaSSHURLTemplate, activeRepo),
		CheckoutDir:      checkoutPath,
		SecretsYAML:      filepath.Join(base, "secrets.yaml"),
		GitSecretHTTP:    repoSecretName("git-creds", activeRepo),
		GitSecretSSH:     repoSecretName("git-creds-ssh", activeRepo),
		GitSecretInvalid: repoSecretName("git-creds-invalid", activeRepo),
	}

	// Optional receiver webhook artifacts — present only when Flux Receiver setup ran.
	if data, readErr := os.ReadFile(filepath.Join(base, "receiver-webhook-url.txt")); readErr == nil {
		a.ReceiverWebhookURL = strings.TrimSpace(string(data))
	}
	if data, readErr := os.ReadFile(filepath.Join(base, "receiver-webhook-id.txt")); readErr == nil {
		a.ReceiverWebhookID = strings.TrimSpace(string(data))
	}

	return a
}

// resolveRepoCheckoutPath determines the local checkout directory for a repo.
// It prefers checkout-path.txt when present, then falls back to REPOS_DIR/<repo>
// or .stamps/repos/<repo>.
func resolveRepoCheckoutPath(projectDir, base, activeRepo string) string {
	checkoutPathBytes, readErr := os.ReadFile(filepath.Join(base, "checkout-path.txt"))
	if readErr != nil && !os.IsNotExist(readErr) {
		Expect(readErr).NotTo(HaveOccurred(), "failed to read checkout-path.txt")
	}

	if checkoutPath := strings.TrimSpace(string(checkoutPathBytes)); checkoutPath != "" {
		return checkoutPath
	}

	checkoutRoot := strings.TrimSpace(os.Getenv("REPOS_DIR"))
	switch {
	case checkoutRoot == "":
		checkoutRoot = filepath.Join(projectDir, ".stamps", "repos")
	case !filepath.IsAbs(checkoutRoot):
		checkoutRoot = filepath.Join(projectDir, checkoutRoot)
	}

	return filepath.Join(checkoutRoot, activeRepo)
}

// repoSecretName returns the K8s Secret name for a given prefix and repo name.
// Follows the convention used by gitea-run-setup.sh: <prefix>-<repoName>.
func repoSecretName(prefix, repoName string) string {
	if strings.TrimSpace(repoName) == "" {
		return prefix
	}
	return prefix + "-" + repoName
}
