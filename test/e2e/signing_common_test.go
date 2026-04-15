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
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	gitpkg "github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/giteaclient"
)

// latestCommitHashForPath returns the most recent commit hash that touched
// commitPath in the given checkout.
func latestCommitHashForPath(checkoutDir, commitPath string) (string, error) {
	hash, err := gitRun(checkoutDir, "log", "-1", "--format=%H", "--", commitPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(hash), nil
}

// assertLocalSSHVerification verifies the signed commit with both
// `ssh-keygen -Y verify` and `git verify-commit` using the provided
// authorized_keys-formatted public key and committer email as the identity.
func assertLocalSSHVerification(checkoutDir, commitHash, signingPublicKey, committerEmail string) {
	GinkgoHelper()

	commitRaw, err := gitRun(checkoutDir, "cat-file", "commit", commitHash)
	Expect(err).NotTo(HaveOccurred())

	sigBlock := extractSSHSigBlock(commitRaw)
	Expect(sigBlock).To(ContainSubstring("BEGIN SSH SIGNATURE"),
		"must find SSH signature block in commit %s", commitHash)

	tmpDir, err := os.MkdirTemp("", "e2e-signing-*")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = os.RemoveAll(tmpDir) })

	allowedSigners := fmt.Sprintf("%s namespaces=\"git\" %s\n", committerEmail, signingPublicKey)
	allowedSignersFile := filepath.Join(tmpDir, "allowed-signers")
	Expect(os.WriteFile(allowedSignersFile, []byte(allowedSigners), 0o600)).To(Succeed())

	sigFile := filepath.Join(tmpDir, "commit.sig")
	Expect(os.WriteFile(sigFile, []byte(sigBlock), 0o600)).To(Succeed())

	payload := removeGpgsigHeader(commitRaw)
	payloadFile := filepath.Join(tmpDir, "commit.payload")
	Expect(os.WriteFile(payloadFile, []byte(payload), 0o600)).To(Succeed())

	out, vErr := sshKeygenVerify(allowedSignersFile, committerEmail, sigFile, payloadFile)
	Expect(vErr).NotTo(HaveOccurred(),
		"ssh-keygen -Y verify failed for commit %s.\nOutput: %s", commitHash, out)
	Expect(out).To(ContainSubstring("Good"), "ssh-keygen should report a good signature")

	gitOut, gitErr := gitVerifyCommit(checkoutDir, allowedSignersFile, commitHash)
	Expect(gitErr).NotTo(HaveOccurred(),
		"git verify-commit failed for commit %s.\nOutput: %s", commitHash, gitOut)
}

// assertGiteaVerified queries Gitea's commit API for the given commit and
// asserts that Gitea reports it as verified by the expected signer identity.
func assertGiteaVerified(repoName, commitHash, expectedSignerEmail string) {
	GinkgoHelper()

	v, err := GetCommitVerification(giteaOrg(), repoName, commitHash)
	Expect(err).NotTo(HaveOccurred(),
		"failed to fetch Gitea commit verification for %s/%s@%s", giteaOrg(), repoName, commitHash)
	Expect(v).NotTo(BeNil())
	Expect(v.Verified).To(BeTrue(),
		"Gitea did not report commit as verified.\n  repo=%s/%s\n  commit=%s\n  reason=%q",
		giteaOrg(), repoName, commitHash, v.Reason)
	Expect(v.Signer).NotTo(BeNil(),
		"Gitea did not resolve a signer for verified commit.\n  repo=%s/%s\n  commit=%s",
		giteaOrg(), repoName, commitHash)
	Expect(strings.TrimSpace(v.Signer.Email)).To(Equal(expectedSignerEmail),
		"Gitea resolved the commit to the wrong signer.\n  repo=%s/%s\n  commit=%s\n  reason=%q",
		giteaOrg(), repoName, commitHash, v.Reason)
	Expect(strings.TrimSpace(v.Signature)).To(ContainSubstring("BEGIN SSH SIGNATURE"),
		"Gitea did not return the SSH signature payload.\n  repo=%s/%s\n  commit=%s\n  verified=%t\n  reason=%q",
		giteaOrg(), repoName, commitHash, v.Verified, v.Reason)
	AddReportEntry("gitea-commit-verification",
		fmt.Sprintf("repo=%s/%s commit=%s verified=%t reason=%q signer=%s",
			giteaOrg(), repoName, commitHash, v.Verified, v.Reason, v.Signer.Email))
}

// applySigningSecret creates a Secret in namespace with the signing key
// material using the keys expected by internal/git/signing.go.
func applySigningSecret(namespace, name string, privateKeyPEM, publicKey []byte) {
	GinkgoHelper()

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"signing.key": privateKeyPEM,
			"signing.pub": []byte(strings.TrimSpace(string(publicKey)) + "\n"),
		},
	}

	manifest, err := yaml.Marshal(secret)
	Expect(err).NotTo(HaveOccurred(), "failed to marshal signing secret %s/%s", namespace, name)

	_, err = kubectlRunWithStdin(namespace, string(manifest), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply signing secret %s/%s", namespace, name)
}

func verifySigningPublicKeyInGitea(user *giteaTestUser, publicKey, fingerprint string, privateKeyPEM []byte) error {
	if user == nil {
		return errors.New("user is nil")
	}
	if strings.TrimSpace(user.Login) == "" {
		return errors.New("user login is empty")
	}
	if strings.TrimSpace(user.Password) == "" {
		return errors.New("user password is empty")
	}
	if strings.TrimSpace(publicKey) == "" {
		return errors.New("public key is empty")
	}
	if strings.TrimSpace(fingerprint) == "" {
		return errors.New("fingerprint is empty")
	}
	if len(privateKeyPEM) == 0 {
		return errors.New("private key is empty")
	}

	workDir, err := os.MkdirTemp("", "e2e-gitea-verify-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	privPath := filepath.Join(workDir, "id_sign")
	if err := os.WriteFile(privPath, privateKeyPEM, 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), giteaRequestTimeout)
	defer cancel()

	userClient := giteaclient.New(giteaAPIBase(), user.Login, user.Password)
	token, err := userClient.GetVerificationToken(ctx)
	if err != nil {
		return fmt.Errorf("get verification token: %w", err)
	}

	armoredSig, err := signTokenWithSSHKeygen(workDir, privPath, token)
	if err != nil {
		return err
	}

	sess, err := giteaclient.NewWebSession(ctx, giteaWebBase(), user.Login, user.Password, false)
	if err != nil {
		return fmt.Errorf("create web session: %w", err)
	}

	if err := sess.VerifySSHKey(ctx, publicKey, fingerprint, armoredSig); err != nil {
		return fmt.Errorf("verify SSH key in Gitea: %w", err)
	}
	return nil
}

func secretData(namespace, secretName, dataKey string) ([]byte, error) {
	escapedKey := strings.ReplaceAll(strings.TrimSpace(dataKey), ".", "\\.")
	output, err := kubectlRunInNamespace(namespace, "get", "secret", secretName,
		"-o", fmt.Sprintf("jsonpath={.data.%s}", escapedKey))
	if err != nil {
		return nil, err
	}

	encoded := strings.TrimSpace(output)
	if encoded == "" {
		return nil, fmt.Errorf("secret %s/%s is missing %s", namespace, secretName, dataKey)
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode secret %s/%s %s: %w", namespace, secretName, dataKey, err)
	}
	return decoded, nil
}

func signingPrivateKeyFromSecret(namespace, secretName string) ([]byte, error) {
	return secretData(namespace, secretName, gitpkg.SigningKeyDataKey)
}

func signTokenWithSSHKeygen(workDir, privPath, token string) (string, error) {
	tokenPath := filepath.Join(workDir, "token.txt")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("write verification token: %w", err)
	}

	cmd := exec.Command("ssh-keygen", "-Y", "sign", "-n", "gitea", "-f", privPath, tokenPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh-keygen -Y sign: %w (%s)", err, out)
	}

	sigBytes, err := os.ReadFile(tokenPath + ".sig")
	if err != nil {
		return "", fmt.Errorf("read ssh-keygen signature: %w", err)
	}
	return string(sigBytes), nil
}
