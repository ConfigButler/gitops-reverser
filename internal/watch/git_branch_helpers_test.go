// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"time"

	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// gitBranchNameForTest is the GitBranch a test GitTarget references: one per provider and branch.
func gitBranchNameForTest(provider, branch string) string { return provider + "-" + branch }

// gitBranchForTest is that GitBranch, in the provider's namespace.
func gitBranchForTest(provider, namespace, branch string) k8stypes.NamespacedName {
	return k8stypes.NamespacedName{Namespace: namespace, Name: gitBranchNameForTest(provider, branch)}
}

// claimWorkerForTest makes gitBranchForTest(provider, namespace, branch) hold the repository branch,
// as its reconcile does once its credential is proved, and returns the request its GitTargets make.
func claimWorkerForTest(
	m *git.WorkerManager, provider, namespace, branch string, repo git.RepoIdentity, parentBranch string,
) git.WorkerRequest {
	gitBranch := gitBranchForTest(provider, namespace, branch)
	m.Claims().Observe(git.BranchKeyFor(repo.URL, branch), git.BranchClaimant{
		GitBranch: gitBranch,
		UID:       k8stypes.UID(gitBranch.String()),
		Created:   time.Unix(0, 0),
	}, true)
	return git.WorkerRequest{
		GitBranch: gitBranch, Provider: provider, Branch: branch, Repo: repo, ParentBranch: parentBranch,
	}
}
