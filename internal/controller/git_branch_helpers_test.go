// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// gitBranchNameForTest is the GitBranch a test GitTarget references: one per provider and branch.
func gitBranchNameForTest(provider, branch string) string { return provider + "-" + branch }

// shopRepo1Main is the GitBranch shop/repo1-main, which the worker tests' GitTargets reference.
var shopRepo1Main = k8stypes.NamespacedName{Namespace: "shop", Name: gitBranchNameForTest("repo1", "main")}

// claimWorkerForTest makes shopRepo1Main hold its repository branch, as its reconcile does once its
// credential is proved, and returns the request its GitTargets make for repo.
func claimWorkerForTest(m *git.WorkerManager, repo git.RepoIdentity) git.WorkerRequest {
	m.Claims().Observe(git.BranchKeyFor(repo.URL, "main"), git.BranchClaimant{
		GitBranch: shopRepo1Main,
		UID:       k8stypes.UID(shopRepo1Main.String()),
		Created:   time.Unix(0, 0),
	}, true)
	return git.WorkerRequest{
		GitBranch: shopRepo1Main,
		Provider:  k8stypes.NamespacedName{Namespace: "shop", Name: "repo1"},
		Branch:    "main",
		Repo:      repo,
	}
}

// gitBranchOn is the GitBranch "<provider>-<branch>" in namespace, writing branch through provider.
func gitBranchOn(provider, namespace, branch string) *configbutleraiv1alpha3.GitBranch {
	return &configbutleraiv1alpha3.GitBranch{
		ObjectMeta: metav1.ObjectMeta{Name: gitBranchNameForTest(provider, branch), Namespace: namespace},
		Spec: configbutleraiv1alpha3.GitBranchSpec{
			GitProviderRef: meta.NamespacedObjectReference{Name: provider}, Branch: branch,
			AllowedGitTargets: writersFromSame(),
		},
	}
}
