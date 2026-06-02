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
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	// auditConsumerRepo holds the repo fixtures shared by the audit-consumer specs.
	auditConsumerRepo     *RepoArtifacts
	auditConsumerRepoOnce sync.Once
)

// ensureAuditConsumerRepo lazily provisions the shared Gitea repo used by the
// audit-consumer specs (commit-window batching, commit request).
func ensureAuditConsumerRepo() *RepoArtifacts {
	GinkgoHelper()

	auditConsumerRepoOnce.Do(func() {
		consumerNs := testNamespaceFor("audit-consumer")

		By("creating the audit consumer namespace for shared repo fixtures")
		_, _ = kubectlRun("create", "namespace", consumerNs)

		By("setting up the shared Gitea repo for audit-consumer tests")
		auditConsumerRepo = SetupRepo(
			resolveE2EContext(),
			consumerNs,
			fmt.Sprintf("e2e-audit-consumer-%d", GinkgoRandomSeed()),
		)
	})

	Expect(auditConsumerRepo).NotTo(BeNil(), "expected audit consumer repo fixtures to be initialised")
	return auditConsumerRepo
}
