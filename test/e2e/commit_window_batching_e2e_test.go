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
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Commit-window batching exercises the grouped-commit path: multiple events
// arriving within the rolling silence window collapse into one grouped commit
// and one push. The audit-redis consumer pipeline is the real path under test
// — events flow kubectl → audit webhook → Valkey stream → consumer →
// BranchWorker.commitGroups → BranchWorker.pushPendingCommits.
var _ = Describe("Commit Window Batching", Label("commit-window-batching", "audit-redis", "smoke"), Ordered, func() {
	var (
		testNs        string
		gitProvName   string
		gitTargetName string
		watchRuleName string
	)

	const commitWindow = "3s"

	BeforeAll(func() {
		By("creating commit-window-batching test namespace and applying git secrets")
		testNs = testNamespaceFor("commit-window-batching")
		_, _ = kubectlRun("create", "namespace", testNs)
		repo := ensureAuditRedisRepo()
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to namespace")
		applySOPSAgeKeyToNamespace(testNs)

		seed := GinkgoRandomSeed()
		gitProvName = fmt.Sprintf("commit-window-gitprovider-%d", seed)
		gitTargetName = fmt.Sprintf("commit-window-gittarget-%d", seed)
		watchRuleName = fmt.Sprintf("commit-window-watchrule-%d", seed)

		By(fmt.Sprintf("creating GitProvider with commitWindow=%s", commitWindow))
		createGitProviderWithCommitWindow(
			gitProvName,
			testNs,
			repo.GitSecretHTTP,
			repo.RepoURLHTTP,
			commitWindow,
		)
		verifyResourceStatus("gitprovider", gitProvName, testNs, "True", "Ready", "")

		createGitTarget(gitTargetName, testNs, gitProvName, "e2e/commit-window-test", "main")
		verifyResourceStatus("gittarget", gitTargetName, testNs, "True", "Ready", "")

		watchRuleData := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{
			Name:            watchRuleName,
			Namespace:       testNs,
			DestinationName: gitTargetName,
		}
		err = applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", watchRuleData, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to apply WatchRule")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
	})

	AfterAll(func() {
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(gitTargetName, testNs)
		cleanupNamespacedResource(testNs, "gitprovider", gitProvName)
		cleanupNamespace(testNs)
	})

	It("collapses a burst of events into one grouped commit and one push", func() {
		const burstSize = 4

		repo := auditRedisRepo
		basePath := "e2e/commit-window-test"
		seed := GinkgoRandomSeed()
		burstPrefix := fmt.Sprintf("commit-window-burst-%d", seed)

		// Seed: ensure the repo has at least one commit for this target so
		// commit-count math is meaningful and not racing against bootstrap.
		seedCM := fmt.Sprintf("%s-seed", burstPrefix)
		applyConfigMap(testNs, seedCM)
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			expected := filepath.Join(repo.CheckoutDir, basePath,
				"v1", "configmaps", testNs, seedCM+".yaml")
			_, statErr := os.Stat(expected)
			g.Expect(statErr).NotTo(HaveOccurred(),
				fmt.Sprintf("seed ConfigMap should land before the burst: %s", expected))
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		commitsBefore := mustCommitCount(repo.CheckoutDir)

		By(fmt.Sprintf("creating %d ConfigMaps in rapid succession", burstSize))
		burstStart := time.Now()
		burstNames := make([]string, burstSize)
		for i := range burstSize {
			name := fmt.Sprintf("%s-%d", burstPrefix, i)
			burstNames[i] = name
			applyConfigMap(testNs, name)
		}
		burstSubmitted := time.Since(burstStart)
		// Burst must fit comfortably inside the commit window so the
		// batching property is what we are actually exercising.
		Expect(burstSubmitted).To(BeNumerically("<", 2*time.Second),
			"burst should be submitted faster than the commit window")

		By("waiting for every burst ConfigMap to land in the repository")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			for _, name := range burstNames {
				expected := filepath.Join(repo.CheckoutDir, basePath,
					"v1", "configmaps", testNs, name+".yaml")
				_, statErr := os.Stat(expected)
				g.Expect(statErr).NotTo(HaveOccurred(),
					fmt.Sprintf("ConfigMap file should exist: %s", expected))
			}
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		commitsAfter := mustCommitCount(repo.CheckoutDir)
		commitsAdded := commitsAfter - commitsBefore

		// Phase 3 groups same-author events in the window into one commit. The
		// batching property is observable as one new remote commit touching the
		// whole burst after the commitWindow expires.
		Expect(commitsAdded).To(Equal(1),
			fmt.Sprintf("expected 1 grouped commit for the burst, got %d", commitsAdded))

		assertBurstFilesAreGroupedIntoLatestCommit(repo.CheckoutDir, burstNames, basePath, testNs)

		By("cleaning up burst ConfigMaps")
		for _, name := range burstNames {
			_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", name, "--ignore-not-found=true")
		}
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", seedCM, "--ignore-not-found=true")
	})
})

// applyConfigMap renders the standard manager configmap template and applies
// it. The template carries the ConfigMap data the burst-event audit pipeline
// expects to see.
func applyConfigMap(namespace, name string) {
	GinkgoHelper()
	data := struct {
		Name      string
		Namespace string
	}{Name: name, Namespace: namespace}
	err := applyFromTemplate("test/e2e/templates/manager/configmap.tmpl", data, namespace)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to apply ConfigMap %s/%s", namespace, name))
}

// mustCommitCount returns git rev-list --count main in the checkout. Any error
// fails the spec — callers use this for arithmetic, so an error would silently
// distort the assertion.
func mustCommitCount(checkoutDir string) int {
	GinkgoHelper()
	out, err := gitRun(checkoutDir, "rev-list", "--count", "main")
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("git rev-list failed: %s", out))
	value := strings.TrimSpace(out)
	if value == "" {
		return 0
	}
	count, err := strconv.Atoi(value)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("parse commit count %q", value))
	return count
}

// assertBurstFilesAreGroupedIntoLatestCommit verifies the latest commit on
// main touches every burst path. This is what the phase 3 grouped-commit path
// should publish after one quiet-window flush.
func assertBurstFilesAreGroupedIntoLatestCommit(checkoutDir string, burstNames []string, basePath, namespace string) {
	GinkgoHelper()

	expectedPaths := make(map[string]struct{}, len(burstNames))
	for _, name := range burstNames {
		p := fmt.Sprintf("%s/v1/configmaps/%s/%s.yaml", basePath, namespace, name)
		expectedPaths[p] = struct{}{}
	}

	out, err := gitRun(checkoutDir, "show", "--pretty=format:", "--name-only", "HEAD")
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("git log failed: %s", out))

	seen := make(map[string]struct{}, len(burstNames))
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, ok := expectedPaths[line]; ok {
			seen[line] = struct{}{}
		}
	}

	Expect(seen).To(HaveLen(len(expectedPaths)),
		fmt.Sprintf("the latest grouped commit should touch every burst ConfigMap; saw %v of %v",
			keysOf(seen), keysOf(expectedPaths)))
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
