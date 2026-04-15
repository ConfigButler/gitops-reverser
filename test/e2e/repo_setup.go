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
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/giteaclient"
	"github.com/ConfigButler/gitops-reverser/test/utils"
)

const (
	giteaReadyAttempts       = 30
	giteaReadyPollInterval   = 2 * time.Second
	giteaReadyRequestTimeout = 5 * time.Second
	transportSSHKeyBits      = 4096
	fluxReceiverNamespace    = "flux-system"
	fluxWebhookServiceName   = "webhook-receiver"
	giteaServiceHTTPBaseURL  = "http://gitea-http.gitea-e2e.svc.cluster.local:13000"
	giteaServiceSSHURLFormat = "ssh://git@gitea-ssh.gitea-e2e.svc.cluster.local:2222/%s/%s.git"
	ownerWritePerms          = 0o750
)

func bootstrapRepoArtifacts(namespace, repoName string) (*RepoArtifacts, error) {
	projectDir, err := utils.GetProjectDir()
	if err != nil {
		return nil, err
	}

	gitea := giteaTestInstance()
	if err := waitForGiteaAPI(gitea.Client()); err != nil {
		return nil, err
	}

	ctx, cancel := gitea.Context()
	defer cancel()

	if _, err := gitea.Client().EnsureOrg(ctx, gitea.Org, "Test Organization", "E2E Test Organization"); err != nil {
		return nil, fmt.Errorf("ensure Gitea org %q: %w", gitea.Org, err)
	}

	if _, err := gitea.Client().EnsureOrgRepo(
		ctx,
		gitea.Org,
		repoName,
		"E2E Test Repository",
		false,
		false,
	); err != nil {
		return nil, fmt.Errorf("ensure Gitea repo %q: %w", repoName, err)
	}

	tokenName := fmt.Sprintf("e2e-%s-%s-%d", namespace, repoName, time.Now().UnixNano())
	token, err := gitea.Client().CreateAccessToken(ctx, gitea.AdminUser, tokenName, []string{
		"write:repository",
		"read:repository",
		"write:organization",
		"read:organization",
	})
	if err != nil {
		return nil, fmt.Errorf("create Gitea access token for %q: %w", repoName, err)
	}

	privateKeyPEM, publicKey, err := generateTransportSSHKeyPair()
	if err != nil {
		return nil, err
	}

	if _, err := gitea.Client().RegisterUserKeyAsAdmin(
		ctx,
		gitea.AdminUser,
		"E2E Transport Key "+repoName,
		publicKey,
	); err != nil {
		return nil, fmt.Errorf("register transport SSH key for %q: %w", repoName, err)
	}

	receiverWebhookURL, receiverWebhookID, err := ensureRepoWebhook(gitea, repoName)
	if err != nil {
		return nil, err
	}

	secretsPath := repoSecretsManifestPath(projectDir, namespace, repoName)
	if err := writeRepoSecretsManifest(
		secretsPath,
		namespace,
		repoName,
		gitea.AdminUser,
		token,
		privateKeyPEM,
	); err != nil {
		return nil, err
	}

	checkoutDir := resolveRepoCheckoutDir(projectDir, repoName)
	if err := ensureRepoCheckout(checkoutDir, localCloneURL(gitea, repoName)); err != nil {
		return nil, err
	}

	return &RepoArtifacts{
		RepoName:           repoName,
		RepoURLHTTP:        inClusterHTTPRepoURL(gitea.Org, repoName),
		RepoURLSSH:         inClusterSSHRepoURL(gitea.Org, repoName),
		CheckoutDir:        checkoutDir,
		SecretsYAML:        secretsPath,
		GitSecretHTTP:      repoSecretName("git-creds", repoName),
		GitSecretSSH:       repoSecretName("git-creds-ssh", repoName),
		GitSecretInvalid:   repoSecretName("git-creds-invalid", repoName),
		ReceiverWebhookURL: receiverWebhookURL,
		ReceiverWebhookID:  receiverWebhookID,
	}, nil
}

func waitForGiteaAPI(client *giteaclient.Client) error {
	var lastErr error
	for range giteaReadyAttempts {
		ctx, cancel := context.WithTimeout(context.Background(), giteaReadyRequestTimeout)
		code, raw, err := client.Do(ctx, http.MethodGet, "/version", nil, nil)
		cancel()
		if err == nil && code == http.StatusOK {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("GET /version: HTTP %d: %s", code, giteaclient.TruncateBody(string(raw)))
		}
		time.Sleep(giteaReadyPollInterval)
	}

	if lastErr == nil {
		lastErr = errors.New("gitea API did not become ready")
	}
	return fmt.Errorf("wait for Gitea API: %w", lastErr)
}

func generateTransportSSHKeyPair() ([]byte, string, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, transportSSHKeyBits)
	if err != nil {
		return nil, "", fmt.Errorf("generate RSA transport key: %w", err)
	}

	block := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}
	privateKeyPEM := pem.EncodeToMemory(block)

	publicKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, "", fmt.Errorf("derive RSA transport public key: %w", err)
	}

	return privateKeyPEM, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey))), nil
}

func ensureRepoWebhook(gitea *GiteaTestInstance, repoName string) (string, string, error) {
	receiverSecretName := getenvOrDefault("FLUX_RECEIVER_SECRET_NAME", "gitea-webreceiver-"+repoName)
	receiverName := getenvOrDefault("FLUX_RECEIVER_NAME", "gitea-webreceiver-"+repoName)
	webhookURLBase := getenvOrDefault(
		"FLUX_WEBHOOK_URL_BASE",
		fmt.Sprintf("http://%s.%s.svc.cluster.local", fluxWebhookServiceName, fluxReceiverNamespace),
	)

	receiverToken, found, err := fluxReceiverToken(receiverSecretName)
	if err != nil {
		return "", "", err
	}
	if !found || receiverToken == "" {
		return "", "", nil
	}

	receiverPath, found, err := waitForFluxReceiverPath(receiverName)
	if err != nil {
		return "", "", err
	}
	if !found || receiverPath == "" {
		return "", "", nil
	}

	callbackURL := webhookURLBase + receiverPath
	ctx, cancel := gitea.Context()
	defer cancel()

	hooks, err := gitea.Client().ListRepoHooks(ctx, gitea.Org, repoName)
	if err != nil {
		return "", "", fmt.Errorf("list repo hooks for %q: %w", repoName, err)
	}
	for _, hook := range hooks {
		if hook.Type != "gitea" {
			continue
		}
		if !strings.HasPrefix(hook.Config.URL, webhookURLBase+"/hook/") {
			continue
		}
		if err := gitea.Client().DeleteRepoHook(ctx, gitea.Org, repoName, hook.ID); err != nil {
			return "", "", fmt.Errorf("delete repo hook %d for %q: %w", hook.ID, repoName, err)
		}
	}

	hook, err := gitea.Client().CreateGiteaWebhook(ctx, gitea.Org, repoName, callbackURL, receiverToken, []string{
		"push",
		"create",
		"delete",
	})
	if err != nil {
		return "", "", fmt.Errorf("create repo webhook for %q: %w", repoName, err)
	}

	return callbackURL, strconv.FormatInt(hook.ID, 10), nil
}

func fluxReceiverToken(secretName string) (string, bool, error) {
	output, err := kubectlRunInNamespace(
		fluxReceiverNamespace,
		"get",
		"secret",
		secretName,
		"-o",
		"jsonpath={.data.token}",
	)
	if err != nil {
		if isKubectlNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}

	encoded := strings.TrimSpace(output)
	if encoded == "" {
		return "", true, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", true, fmt.Errorf(
			"decode Flux receiver token from secret %s/%s: %w",
			fluxReceiverNamespace,
			secretName,
			err,
		)
	}
	return strings.TrimSpace(string(decoded)), true, nil
}

func waitForFluxReceiverPath(receiverName string) (string, bool, error) {
	if _, err := kubectlRunInNamespace(fluxReceiverNamespace, "get", "receiver", receiverName); err != nil {
		if isKubectlNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}

	for range giteaReadyAttempts {
		output, err := kubectlRunInNamespace(
			fluxReceiverNamespace,
			"get",
			"receiver",
			receiverName,
			"-o",
			"jsonpath={.status.webhookPath}",
		)
		if err == nil {
			if path := strings.TrimSpace(output); path != "" {
				return path, true, nil
			}
		}
		if err != nil && !isKubectlNotFound(err) {
			return "", true, err
		}
		time.Sleep(giteaReadyPollInterval)
	}

	return "", true, nil
}

func repoSecretsManifestPath(projectDir, namespace, repoName string) string {
	return filepath.Join(projectDir, ".stamps", "e2e-repo-artifacts", namespace, repoName, "secrets.yaml")
}

func resolveRepoCheckoutDir(projectDir, repoName string) string {
	if checkoutDir := strings.TrimSpace(os.Getenv("CHECKOUT_DIR")); checkoutDir != "" {
		if filepath.IsAbs(checkoutDir) {
			return checkoutDir
		}
		return filepath.Join(projectDir, checkoutDir)
	}

	checkoutRoot := strings.TrimSpace(os.Getenv("REPOS_DIR"))
	switch {
	case checkoutRoot == "":
		checkoutRoot = filepath.Join(projectDir, ".stamps", "repos")
	case !filepath.IsAbs(checkoutRoot):
		checkoutRoot = filepath.Join(projectDir, checkoutRoot)
	}

	return filepath.Join(checkoutRoot, repoName)
}

func ensureRepoCheckout(checkoutDir, repoURL string) error {
	if err := os.MkdirAll(filepath.Dir(checkoutDir), ownerWritePerms); err != nil {
		return fmt.Errorf("create checkout parent dir for %s: %w", checkoutDir, err)
	}

	exists, err := checkoutExists(checkoutDir)
	if err != nil {
		return err
	}

	if exists {
		if err := runGitCommand(checkoutDir, "remote", "set-url", "origin", repoURL); err != nil {
			return fmt.Errorf("set checkout origin for %s: %w", checkoutDir, err)
		}
	} else {
		if err := os.RemoveAll(checkoutDir); err != nil {
			return fmt.Errorf("remove stale checkout dir %s: %w", checkoutDir, err)
		}
		if err := runGitCommand("", "clone", repoURL, checkoutDir); err != nil {
			return fmt.Errorf("clone %s into %s: %w", repoURL, checkoutDir, err)
		}
	}

	if err := runGitCommand(checkoutDir, "config", "user.name", "E2E Test"); err != nil {
		return fmt.Errorf("configure checkout user.name: %w", err)
	}
	if err := runGitCommand(checkoutDir, "config", "user.email", "e2e-test@gitops-reverser.local"); err != nil {
		return fmt.Errorf("configure checkout user.email: %w", err)
	}
	if err := runGitCommand(checkoutDir, "config", "commit.gpgsign", "false"); err != nil {
		return fmt.Errorf("configure checkout commit.gpgsign: %w", err)
	}

	if _, err := os.Stat(filepath.Join(checkoutDir, ".git", "HEAD")); err != nil {
		return fmt.Errorf("expected checkout to contain .git/HEAD at %s: %w", checkoutDir, err)
	}

	return nil
}

func writeRepoSecretsManifest(
	secretsPath,
	namespace,
	repoName,
	username,
	token string,
	privateKeyPEM []byte,
) error {
	if err := os.MkdirAll(filepath.Dir(secretsPath), ownerWritePerms); err != nil {
		return fmt.Errorf("create repo artifact dir for %s: %w", repoName, err)
	}

	secrets := []*corev1.Secret{
		{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "Secret",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:        repoSecretName("git-creds", repoName),
				Annotations: reflectorAnnotations(repoName),
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"username": []byte(username),
				"password": []byte(token),
			},
		},
		{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "Secret",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: repoSecretName("git-creds-ssh", repoName),
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"ssh-privatekey": privateKeyPEM,
			},
		},
		{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "Secret",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: repoSecretName("git-creds-invalid", repoName),
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"username": []byte("invaliduser"),
				"password": []byte("invalidpassword"),
			},
		},
	}

	var docs []string
	for _, secret := range secrets {
		manifest, err := yaml.Marshal(secret)
		if err != nil {
			return fmt.Errorf("marshal secret manifest %s/%s: %w", namespace, secret.Name, err)
		}
		docs = append(docs, strings.TrimSpace(string(manifest)))
	}

	content := strings.Join(docs, "\n---\n") + "\n"
	if err := os.WriteFile(secretsPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write secret manifest %s: %w", secretsPath, err)
	}

	return nil
}

func reflectorAnnotations(repoName string) map[string]string {
	if repoName != "demo" {
		return nil
	}

	return map[string]string{
		"reflector.v1.k8s.emberstack.com/reflection-allowed":         "true",
		"reflector.v1.k8s.emberstack.com/reflection-auto-enabled":    "true",
		"reflector.v1.k8s.emberstack.com/reflection-auto-namespaces": "podinfos-intent,podinfos-preview,podinfos-production",
	}
}

func localCloneURL(gitea *GiteaTestInstance, repoName string) string {
	baseURL := strings.TrimSuffix(strings.TrimRight(gitea.APIBaseURL, "/"), "/api/v1")
	return fmt.Sprintf("%s/%s/%s.git", baseURL, gitea.Org, repoName)
}

func inClusterHTTPRepoURL(org, repoName string) string {
	return fmt.Sprintf("%s/%s/%s.git", giteaServiceHTTPBaseURL, org, repoName)
}

func inClusterSSHRepoURL(org, repoName string) string {
	return fmt.Sprintf(giteaServiceSSHURLFormat, org, repoName)
}

func checkoutExists(checkoutDir string) (bool, error) {
	_, err := os.Stat(filepath.Join(checkoutDir, ".git"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat checkout %s: %w", checkoutDir, err)
}

func runGitCommand(dir string, args ...string) error {
	commandArgs := append([]string{}, args...)
	if strings.TrimSpace(dir) != "" {
		commandArgs = append([]string{"-C", dir}, commandArgs...)
	}

	cmd := exec.CommandContext(e2eCommandContext(context.Background()), "git", commandArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%q failed with error %q: %w", strings.Join(cmd.Args, " "), string(out), err)
	}
	return nil
}

func getenvOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func isKubectlNotFound(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
}
