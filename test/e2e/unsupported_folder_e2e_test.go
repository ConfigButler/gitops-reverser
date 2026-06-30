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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// This spec proves the acceptance gate end to end: when a GitTarget's path holds content
// the operator cannot safely manage — here a kustomization.yaml that uses an unsupported
// feature (a patches block) — the first-materialization resync is REFUSED. The operator
// commits nothing and surfaces the refusal on GitTarget status as GitPathAccepted=False,
// Stalled=True, and Ready=False. WatchRule and ClusterWatchRule surface the same dependency
// on GitTargetReady. The source streams stay truthful to watch state; the path is left untouched
// until a human cleans it. See docs/design/unsupported-folder-refusal-plan.md.
var _ = Describe("Manager Unsupported Folder Refusal", Label("manager", "unsupported-folder"), Ordered, func() {
	var (
		testNs          string
		repo            *RepoArtifacts
		providerName    = "unsupported-folder-provider"
		destName        = "unsupported-folder-dest"
		ruleName        = "unsupported-folder-rule"
		clusterRuleName = "unsupported-folder-cluster-rule"
		gitPath         = "e2e/unsupported-folder"
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

		// createGitTarget enables SOPS encryption referencing the shared sops-age-key
		// secret, so it must exist or the GitTarget's EncryptionConfigured gate (and thus
		// Ready) never goes True — independent of the refusal under test.
		applySOPSAgeKeyToNamespace(testNs)

		By("creating the GitProvider")
		createGitProviderWithURLInNamespace(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")
	})

	AfterAll(func() {
		cleanupWatchRule(ruleName, testNs)
		cleanupClusterWatchRule(clusterRuleName)
		cleanupGitTarget(destName, testNs)
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", providerName, "--ignore-not-found=true")
		cleanupNamespace(testNs)
	})

	It("refuses a hard-Kustomize path without writing", func() {
		By("seeding the Git repository with a path that uses an unsupported Kustomize feature")
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

		By("creating the GitTarget, WatchRule, and ClusterWatchRule pointed at the unsupported path")
		createGitTarget(destName, testNs, providerName, gitPath, "main")
		err := applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", struct {
			Name            string
			Namespace       string
			DestinationName string
		}{Name: ruleName, Namespace: testNs, DestinationName: destName}, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to apply ConfigMap WatchRule")
		applyUnsupportedPathClusterWatchRule(clusterRuleName, testNs, destName)

		By("the path is refused and the GitTarget is stalled")
		waitForGitTargetGitPathRefused(destName, testNs, "UnsupportedContent")

		By("the WatchRule and ClusterWatchRule surface the refused GitTarget dependency")
		waitForRuleBlockedByGitPath("watchrule", ruleName, testNs, "UnsupportedContent")
		waitForRuleBlockedByGitPath("clusterwatchrule", clusterRuleName, "", "UnsupportedContent")

		By("the operator commits nothing on top of the unsupported path")
		Consistently(func(g Gomega) {
			g.Expect(remoteBranchHead(g, repo.CheckoutDir)).
				To(Equal(seedHead), "the operator must not commit into a refused path")
		}, 20*time.Second, 4*time.Second).Should(Succeed())

		By("unsupported Git path refused: GitTarget stalled, nothing written")
	})
})

// writeUnsupportedKustomizeFolder renders a temp folder holding a kustomization.yaml that uses
// an unsupported feature (a patches block) plus the ConfigMap it references. The operator
// cannot map a patched render back to editable source documents, so the acceptance gate must
// refuse the whole Git path.
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

// waitForGitTargetGitPathRefused polls until the GitTarget reports the target-side Git path
// refusal in the kstatus shape generic tooling expects.
func waitForGitTargetGitPathRefused(name, namespace, expectedReason string) {
	GinkgoHelper()

	// The refusal is computed during data-plane materialization (a worker git op), which is
	// slower than a plain reconcile — hence the 150s budget on the gating condition. Ready and
	// Stalled flip in the same reconcile, so they settle by the time GitPathAccepted is False.
	verifyResourceCondition("gittarget", name, namespace, "GitPathAccepted", "False", expectedReason, "", "150s")
	verifyResourceCondition("gittarget", name, namespace, "Ready", "False", "", "", "150s")
	verifyResourceCondition("gittarget", name, namespace, "Stalled", "True", "", "", "150s")
}

func waitForRuleBlockedByGitPath(resourceType, name, namespace, expectedReason string) {
	GinkgoHelper()

	verifyResourceCondition(
		resourceType,
		name,
		namespace,
		"GitTargetReady",
		"False",
		expectedReason,
		"kustomization.yaml",
	)
	verifyResourceCondition(resourceType, name, namespace, "Ready", "False", expectedReason, "kustomization.yaml")
	verifyResourceCondition(resourceType, name, namespace, "Stalled", "True", expectedReason, "kustomization.yaml")
	verifyResourceCondition(resourceType, name, namespace, "Reconciling", "False", expectedReason, "stalled")
}

func applyUnsupportedPathClusterWatchRule(name, targetNamespace, targetName string) {
	GinkgoHelper()

	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha3
kind: ClusterWatchRule
metadata:
  name: %s
spec:
  targetRef:
    name: %s
    namespace: %s
  rules:
  - scope: Namespaced
    apiGroups: [""]
    apiVersions: ["v1"]
    resources: ["configmaps"]
`, name, targetName, targetNamespace)

	_, err := kubectlRunWithStdin("", manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply ConfigMap ClusterWatchRule")
}
