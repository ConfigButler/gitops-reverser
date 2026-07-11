// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The bi-directional corner: the only e2e category where a foreign GitOps
// controller reconciles the same path gitops-reverser writes to. It is opt-in
// because it is the only place Argo CD is installed — `task test-e2e` skips it,
// `task test-e2e-bi-directional` runs it.
//
// See docs/design/e2e-bi-directional-corner.md and docs/bi-directional.md.
const biDirectionalEnabledEnv = "E2E_ENABLE_BI_DIRECTIONAL"

const (
	biPollInterval          = time.Second
	biEventuallyTimeout     = 45 * time.Second
	biStableCountShortWait  = 3 * time.Second
	biStableCountMediumWait = 5 * time.Second
	biStableCountLongWait   = 6 * time.Second
)

func biDirectionalEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(biDirectionalEnabledEnv)))
	return value == "1" || value == "true" || value == "yes"
}

// skipUnlessBiDirectionalEnabled aborts the calling spec unless the corner is
// explicitly enabled. The Ginkgo label alone is not enough: the corner needs an
// Argo CD install that `task prepare-e2e` deliberately does not perform.
func skipUnlessBiDirectionalEnabled() {
	GinkgoHelper()
	if !biDirectionalEnabled() {
		Skip(fmt.Sprintf(
			"bi-directional corner is disabled; run `task test-e2e-bi-directional` (sets %s=true)",
			biDirectionalEnabledEnv,
		))
	}
}

// gitCheckout wraps the local clone of a spec's Gitea repo. Both bi-directional
// specs need the same operations — authenticate the remote, commit, push, pull,
// and above all count commits, since "did a second controller cause an extra
// commit?" is the question this whole corner exists to answer.
type gitCheckout struct {
	checkoutDir     string
	localGitRepoURL string
	gitSecretName   string
	// namespace holding gitSecretName; set after the test namespace exists.
	secretNamespace string
}

func newGitCheckout(repo *RepoArtifacts, namespace string) gitCheckout {
	return gitCheckout{
		checkoutDir:     repo.CheckoutDir,
		localGitRepoURL: fmt.Sprintf("http://localhost:%s/testorg/%s.git", giteaLocalPort(), repo.RepoName),
		gitSecretName:   repo.GitSecretHTTP,
		secretNamespace: namespace,
	}
}

// giteaLocalPort mirrors the GITEA_PORT the e2e port-forward publishes.
func giteaLocalPort() string {
	if port := strings.TrimSpace(os.Getenv("GITEA_PORT")); port != "" {
		return port
	}
	return "13000"
}

func (c gitCheckout) assertCheckoutReady() {
	GinkgoHelper()
	_, err := os.Stat(c.repoPath(".git"))
	Expect(err).NotTo(HaveOccurred(), "expected checkout to exist at checkoutDir")
	Expect(c.configureCheckoutAuth()).To(Succeed())
}

func (c gitCheckout) repoPath(parts ...string) string {
	all := append([]string{c.checkoutDir}, parts...)
	return filepath.Join(all...)
}

func (c gitCheckout) runGit(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = c.checkoutDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (c gitCheckout) configureCheckoutAuth() error {
	username, password := c.readGitCredentialSecretDataDecoded()
	authURL, err := c.authenticatedLocalGitURL(username, password)
	if err != nil {
		return err
	}
	return c.runGit("remote", "set-url", "origin", authURL)
}

func (c gitCheckout) authenticatedLocalGitURL(username, password string) (string, error) {
	parsedURL, err := url.Parse(c.localGitRepoURL)
	if err != nil {
		return "", fmt.Errorf("parse local Git repo URL: %w", err)
	}
	parsedURL.User = url.UserPassword(username, password)
	return parsedURL.String(), nil
}

func (c gitCheckout) readGitCredentialSecretDataBase64() (string, string) {
	GinkgoHelper()
	output, err := kubectlRunInNamespace(c.secretNamespace, "get", "secret", c.gitSecretName, "-o", "json")
	Expect(err).NotTo(HaveOccurred(), "failed to read git credential Secret")

	var obj unstructured.Unstructured
	Expect(json.Unmarshal([]byte(output), &obj.Object)).To(Succeed())

	data, found, err := unstructured.NestedStringMap(obj.Object, "data")
	Expect(err).NotTo(HaveOccurred(), "failed to parse Secret data")
	Expect(found).To(BeTrue(), "git credential Secret data not found")

	username := strings.TrimSpace(data["username"])
	password := strings.TrimSpace(data["password"])
	Expect(username).NotTo(BeEmpty(), "git credential Secret username must be present")
	Expect(password).NotTo(BeEmpty(), "git credential Secret password must be present")

	return username, password
}

func (c gitCheckout) readGitCredentialSecretDataDecoded() (string, string) {
	GinkgoHelper()
	usernameB64, passwordB64 := c.readGitCredentialSecretDataBase64()

	username, err := base64.StdEncoding.DecodeString(usernameB64)
	Expect(err).NotTo(HaveOccurred(), "failed to decode git credential Secret username")

	password, err := base64.StdEncoding.DecodeString(passwordB64)
	Expect(err).NotTo(HaveOccurred(), "failed to decode git credential Secret password")

	return strings.TrimSpace(string(username)), strings.TrimSpace(string(password))
}

func (c gitCheckout) commitAllAndPush(message string) error {
	if err := c.runGit("checkout", "-B", "main"); err != nil {
		return err
	}
	if err := c.runGit("add", "."); err != nil {
		return err
	}
	if err := c.runGit("commit", "--allow-empty", "-m", message); err != nil {
		return err
	}
	return c.runGit("push", "--set-upstream", "origin", "main")
}

func (c gitCheckout) revertHEADAndPush() error {
	if err := c.runGit("checkout", "-B", "main"); err != nil {
		return err
	}
	if err := c.runGit("revert", "--no-edit", "HEAD"); err != nil {
		return err
	}
	return c.runGit("push", "origin", "main")
}

func (c gitCheckout) gitPull() error {
	return c.runGit("pull", "--ff-only")
}

func (c gitCheckout) gitMainCommitCount() (int, error) {
	if err := c.runGit("rev-parse", "--verify", "refs/heads/main"); err != nil {
		if strings.Contains(err.Error(), "unknown revision") ||
			strings.Contains(err.Error(), "Needed a single revision") {
			return 0, nil
		}
		return 0, err
	}

	cmd := exec.Command("git", "rev-list", "--count", "refs/heads/main")
	cmd.Dir = c.checkoutDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("git rev-list --count refs/heads/main: %w: %s", err, strings.TrimSpace(string(output)))
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0, fmt.Errorf("parse git commit count %q: %w", strings.TrimSpace(string(output)), err)
	}
	return count, nil
}

func (c gitCheckout) gitHEAD() string {
	GinkgoHelper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = c.checkoutDir
	output, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to get git HEAD: %s", strings.TrimSpace(string(output))))
	return strings.TrimSpace(string(output))
}

func (c gitCheckout) expectRemoteCommitCount(expected int) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		g.Expect(c.gitPull()).To(Succeed())
		count, err := c.gitMainCommitCount()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(count).To(Equal(expected))
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (c gitCheckout) consistentlyExpectRemoteCommitCount(expected int, duration time.Duration) {
	GinkgoHelper()
	Consistently(func(g Gomega) {
		g.Expect(c.gitPull()).To(Succeed())
		count, err := c.gitMainCommitCount()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(count).To(Equal(expected))
	}, duration, biPollInterval).Should(Succeed())
}

// waitForStableRemoteCommitCount waits until the remote stops moving and returns
// the settled count. Specs that assert exact commit deltas need a quiet baseline:
// the reverser may still be flushing its first reconcile when a spec starts.
func (c gitCheckout) waitForStableRemoteCommitCount(duration time.Duration) int {
	GinkgoHelper()
	var stableCount int

	Eventually(func(g Gomega) {
		g.Expect(c.gitPull()).To(Succeed())
		count, err := c.gitMainCommitCount()
		g.Expect(err).NotTo(HaveOccurred())
		stableCount = count

		Consistently(func(inner Gomega) {
			inner.Expect(c.gitPull()).To(Succeed())
			currentCount, currentErr := c.gitMainCommitCount()
			inner.Expect(currentErr).NotTo(HaveOccurred())
			inner.Expect(currentCount).To(Equal(count))
		}, duration, biPollInterval).Should(Succeed())
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())

	return stableCount
}
