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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// This spec proves the acceptance gate end to end: when a GitTarget's folder holds content
// the operator cannot safely manage — here a kustomization.yaml that uses an unsupported
// feature (a patches block) — the first-materialization resync is REFUSED. The operator
// commits nothing and surfaces the refusal on GitTarget status as a blocked data plane
// (StreamsReady=False, reason UnsupportedContent, phase Degraded) while the control plane
// stays correctly configured (Ready=True). The folder is left untouched until a human cleans
// it. See docs/design/unsupported-folder-refusal-plan.md.
var _ = Describe("Manager Unsupported Folder Refusal", Label("manager", "unsupported-folder"), Ordered, func() {
	var (
		testNs       string
		repo         *RepoArtifacts
		providerName = "unsupported-folder-provider"
		destName     = "unsupported-folder-dest"
		ruleName     = "unsupported-folder-rule"
		gitPath      = "e2e/unsupported-folder"
	)

	const repoName = "e2e-unsupported-folder"

	BeforeAll(func() {
		By("creating the unsupported-folder test namespace")
		testNs = testNamespaceFor("manager-unsupported-folder")
		_, _ = kubectlRun("create", "namespace", testNs)

		By("setting up Gitea repo and credentials")
		repo = SetupRepo(resolveE2EContext(), testNs, fmt.Sprintf("%s-%d", repoName, GinkgoRandomSeed()))

		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")

		By("creating the GitProvider")
		createGitProviderWithURLInNamespace(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")
	})

	AfterAll(func() {
		cleanupWatchRule(ruleName, testNs)
		cleanupGitTarget(destName, testNs)
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
		cleanupNamespace(testNs)
	})

	It("refuses a hard-Kustomize folder and blocks the stream without writing", func() {
		By("seeding the Git repository with a folder that uses an unsupported Kustomize feature")
		seedFolder := writeUnsupportedKustomizeFolder(testNs)
		DeferCleanup(func() { _ = os.RemoveAll(seedFolder) })
		seedRenderedFolderIntoRepo(repo, testNs, seedFolder, gitPath)

		// Capture the seed commit before the GitTarget exists, so any later operator commit is
		// detectable. The operator must add nothing on top of this.
		var seedHead string
		Eventually(func(g Gomega) {
			seedHead = remoteBranchHead(g, repo.CheckoutDir)
			g.Expect(seedHead).NotTo(BeEmpty(), "the seed commit must be on the remote")
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("creating the GitTarget and a ConfigMap WatchRule pointed at the unsupported folder")
		createGitTarget(destName, testNs, providerName, gitPath, "main")
		err := applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", struct {
			Name            string
			Namespace       string
			DestinationName string
		}{Name: ruleName, Namespace: testNs, DestinationName: destName}, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to apply ConfigMap WatchRule")

		By("the control plane is correctly configured: Ready=True")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		By("the data plane is blocked: StreamsReady=False, reason UnsupportedContent")
		waitForGitTargetStreamsBlocked(destName, testNs, "UnsupportedContent")

		By("the operator commits nothing on top of the unsupported folder")
		Consistently(func(g Gomega) {
			g.Expect(remoteBranchHead(g, repo.CheckoutDir)).
				To(Equal(seedHead), "the operator must not commit into a refused folder")
		}, 20*time.Second, 4*time.Second).Should(Succeed())

		By("✅ unsupported folder refused: data plane blocked, nothing written")
	})
})

// writeUnsupportedKustomizeFolder renders a temp folder holding a kustomization.yaml that uses
// an unsupported feature (a patches block) plus the ConfigMap it references. The operator
// cannot map a patched render back to editable source documents, so the acceptance gate must
// refuse the whole folder.
func writeUnsupportedKustomizeFolder(namespace string) string {
	GinkgoHelper()

	dir, err := os.MkdirTemp("", "gitops-reverser-e2e-unsupported-*")
	Expect(err).NotTo(HaveOccurred(), "failed to create unsupported fixture directory")

	kustomization := "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
		"kind: Kustomization\n" +
		"namespace: " + namespace + "\n" +
		"resources:\n  - cm.yaml\n" +
		"patches:\n" +
		"  - target:\n      kind: ConfigMap\n      name: unsupported-sample\n" +
		"    patch: |-\n" +
		"      - op: add\n" +
		"        path: /metadata/labels/patched\n" +
		"        value: \"true\"\n"
	configMap := "apiVersion: v1\n" +
		"kind: ConfigMap\n" +
		"metadata:\n  name: unsupported-sample\n  namespace: " + namespace + "\n" +
		"data:\n  hello: world\n"

	Expect(os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(kustomization), 0o600)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(dir, "cm.yaml"), []byte(configMap), 0o600)).To(Succeed())
	return dir
}

// waitForGitTargetStreamsBlocked polls until the GitTarget's StreamsReady condition is False
// with the expected reason — the user-visible signal that the data plane refused the folder.
func waitForGitTargetStreamsBlocked(name, namespace, expectedReason string) {
	GinkgoHelper()

	Eventually(func(g Gomega) {
		status, err := kubectlRunInNamespace(namespace, "get", "gittarget", name,
			"-o", "jsonpath={.status.conditions[?(@.type=='StreamsReady')].status}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(status)).To(Equal("False"),
			"StreamsReady must be False for an unsupported folder")

		reason, err := kubectlRunInNamespace(namespace, "get", "gittarget", name,
			"-o", "jsonpath={.status.conditions[?(@.type=='StreamsReady')].reason}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(reason)).To(Equal(expectedReason),
			"StreamsReady reason must name the unsupported content")
	}, 150*time.Second, 3*time.Second).Should(Succeed())
}
