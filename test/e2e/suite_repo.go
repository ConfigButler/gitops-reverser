// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ConfigButler/gitops-reverser/internal/giteaclient"
)

// RepoArtifacts holds the Git repository fixtures produced by one SetupRepo
// call. Each e2e test file owns its own instance so that mutable repo state is
// isolated between files.
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
	User               *giteaclient.TestUser
}

// SetupRepo prepares a repo fixture directly from the e2e process and returns
// the resulting RepoArtifacts for file-local storage by callers.
//
// namespace must already exist in the cluster before this call. Shared suite
// preparation must already have provisioned the cluster services, Gitea, and
// port-forwards. Callers are responsible for applying SecretsYAML to the
// namespace afterward.
func SetupRepo(_ string, namespace, repoName string) *RepoArtifacts {
	By(fmt.Sprintf("setting up Gitea repo %q for namespace %q via Go helper", repoName, namespace))
	artifacts, err := bootstrapRepoArtifacts(namespace, repoName)
	Expect(err).NotTo(HaveOccurred(), "failed to prepare repo artifacts for repo %s", repoName)
	gitea := giteaTestInstance()

	By(fmt.Sprintf("ensuring dedicated Gitea user exists for repo %q", artifacts.RepoName))
	user, err := gitea.EnsureTestUser(repoName)
	Expect(err).NotTo(HaveOccurred(), "failed to create or reuse test Gitea user for repo %s", repoName)

	By(fmt.Sprintf("ensuring repo user %q is a collaborator on %q", user.Login, artifacts.RepoName))
	err = gitea.EnsureRepoCollaborator(gitea.Org, artifacts.RepoName, user)
	Expect(err).NotTo(HaveOccurred(), "failed to add collaborator %s to repo %s", user.Login, artifacts.RepoName)

	artifacts.User = user
	return artifacts
}

// repoSecretName returns the K8s Secret name for a given prefix and repo name.
// Format: <prefix>-<repoName>.
func repoSecretName(prefix, repoName string) string {
	if strings.TrimSpace(repoName) == "" {
		return prefix
	}
	return prefix + "-" + repoName
}
