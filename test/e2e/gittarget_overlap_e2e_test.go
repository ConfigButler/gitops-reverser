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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// This spec is the e2e for the GitTarget non-overlap topology guard (design:
// docs/design/manifest/current-manifest-support-review.md, milestone C1 in
// docs/design/manifest/implementation-plan.md). Within one provider+branch, no
// GitTarget path may be equal to, an ancestor of, or a descendant of another's.
// Sibling folders are fine; nesting is rejected at the Validated gate so every
// materialized folder has exactly one owner. The reject path surfaces as a
// reconcile-time status condition (Ready=False / ValidationFailed), not through
// the current allow-only validating admission webhook.
var _ = Describe("Manager GitTarget Overlap Guard", Label("manager"), Ordered, func() {
	const providerName = "gitprovider-overlap"

	var (
		testNs      string
		overlapRepo *RepoArtifacts
	)

	BeforeAll(func() {
		By("creating GitTarget overlap test namespace")
		testNs = testNamespaceFor("manager-overlap")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		By("setting up Gitea repo and credentials for overlap tests")
		overlapRepo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-manager-overlap-%d", GinkgoRandomSeed()),
		)

		By("applying git secrets to test namespace")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", overlapRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		By("creating shared GitProvider for overlap specs")
		createGitProviderWithURLInNamespace(
			providerName,
			testNs,
			overlapRepo.GitSecretHTTP,
			overlapRepo.RepoURLHTTP,
		)
		verifyResourceStatus(
			"gitprovider", providerName, testNs,
			"True", "Ready", "Repository connectivity validated",
		)
	})

	AfterAll(func() {
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("accepts sibling paths in the same repo+branch", func() {
		createGitTarget("overlap-sibling-a", testNs, providerName, "overlap/team-a", "main")
		createGitTarget("overlap-sibling-b", testNs, providerName, "overlap/team-b", "main")

		verifyResourceStatus("gittarget", "overlap-sibling-a", testNs, "True", "Ready", "")
		verifyResourceStatus("gittarget", "overlap-sibling-b", testNs, "True", "Ready", "")
	})

	It("rejects a path nested inside an existing target's path", func() {
		// The child name sorts after the parent name so the controller's
		// deterministic tie-breaker (later timestamp, then identity) agrees with
		// the creation order: the nested target always loses, even on the rare
		// same-second tie. That keeps this spec from flaking.
		By("creating the parent target first so it wins the overlap election")
		createGitTarget("overlap-parent", testNs, providerName, "overlap/nested", "main")
		verifyResourceStatus("gittarget", "overlap-parent", testNs, "True", "Ready", "")

		By("creating a target nested under the parent (created later, must lose)")
		createGitTarget("overlap-parent-child", testNs, providerName, "overlap/nested/child", "main")

		// The nested target is refused at the Validated gate: Ready=False with the
		// ValidationFailed reason and a TargetConflict message. It owns nothing.
		verifyResourceStatus(
			"gittarget", "overlap-parent-child", testNs,
			"False", "ValidationFailed", "TargetConflict",
		)
	})
})
