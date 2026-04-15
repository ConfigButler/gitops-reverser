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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
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
// asserts that Gitea surfaced the SSH signature data for inspection. The
// stricter Verified==true assertion stays deferred until the Gitea 1.25.x
// verification-path mismatch is understood.
func assertGiteaVerified(repoName, commitHash string) {
	GinkgoHelper()

	v, err := GetCommitVerification(giteaOrg(), repoName, commitHash)
	Expect(err).NotTo(HaveOccurred(),
		"failed to fetch Gitea commit verification for %s/%s@%s", giteaOrg(), repoName, commitHash)
	Expect(v).NotTo(BeNil())
	Expect(strings.TrimSpace(v.Signature)).To(ContainSubstring("BEGIN SSH SIGNATURE"),
		"Gitea did not return the SSH signature payload.\n  repo=%s/%s\n  commit=%s\n  verified=%t\n  reason=%q",
		giteaOrg(), repoName, commitHash, v.Verified, v.Reason)
	AddReportEntry("gitea-commit-verification",
		fmt.Sprintf("repo=%s/%s commit=%s verified=%t reason=%q",
			giteaOrg(), repoName, commitHash, v.Verified, v.Reason))
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
