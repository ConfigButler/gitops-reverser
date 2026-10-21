// SPDX-License-Identifier: Apache-2.0

package git

import (
	"time"

	k8stypes "k8s.io/apimachinery/pkg/types"
)

// testGitBranch is the GitBranch a test worker writes for: one per provider and branch, in the
// provider's namespace, as a GitBranch's gitProviderRef is always local.
func testGitBranch(provider, namespace, branch string) k8stypes.NamespacedName {
	return k8stypes.NamespacedName{Namespace: namespace, Name: testGitBranchName(provider, branch)}
}

// claimForTest makes testGitBranch(provider, namespace, branch) hold the repository branch, as its
// reconcile does once its credential is proved, and returns the request its GitTargets' reconcile
// would make.
func claimForTest(m *WorkerManager, provider, namespace, branch string, repo RepoIdentity) WorkerRequest {
	gitBranch := testGitBranch(provider, namespace, branch)
	m.Claims().Observe(BranchKeyFor(repo.URL, branch), BranchClaimant{
		GitBranch: gitBranch,
		UID:       k8stypes.UID(gitBranch.String()),
		Created:   time.Unix(0, 0),
	}, true)
	return WorkerRequest{
		GitBranch: gitBranch,
		Provider:  provider,
		Branch:    branch,
		Repo:      repo,
	}
}

// testGitBranchName is the name of testGitBranch(provider, namespace, branch).
func testGitBranchName(provider, branch string) string { return provider + "-" + branch }
