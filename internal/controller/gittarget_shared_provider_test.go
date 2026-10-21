// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// sharedProviderTarget is a GitTarget named name in namespace, writing path on main through the
// GitProvider providerNS/monorepo (empty providerNS: its own namespace), created age ago.
func sharedProviderTarget(
	namespace, name, providerNS, path string,
	age time.Duration,
) *configbutleraiv1alpha3.GitTarget {
	return &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC).Add(-age)),
		},
		Spec: configbutleraiv1alpha3.GitTargetSpec{
			GitProviderRef: meta.NamespacedObjectReference{Namespace: providerNS, Name: "monorepo"},
			Branch:         "main",
			Path:           path,
		},
	}
}

func sharedProviderReconciler(t *testing.T, objects ...ctrlclient.Object) *GitTargetReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	// The chart's default ClusterProvider admits every namespace; only the GitProviders decide here.
	objects = append(objects, &configbutleraiv1alpha3.ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: configbutleraiv1alpha3.DefaultClusterProviderName},
		Spec: configbutleraiv1alpha3.ClusterProviderSpec{
			AccessFrom: &configbutleraiv1alpha3.NamespaceMatcher{Selector: &metav1.LabelSelector{}},
		},
	})
	for _, ns := range []string{"platform", "team-b"} {
		objects = append(objects, &configbutleraiv1alpha3.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "monorepo"},
			Spec:       configbutleraiv1alpha3.GitProviderSpec{URL: "https://github.com/acme/platform"},
		})
	}
	return &GitTargetReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
}

// TestCheckForConflicts_ARepositoryBranchHasOneGitProvider: one worker writes a repository branch,
// with one GitProvider's credential. A GitTarget writing it through another GitProvider than the
// oldest GitTarget's is refused, and the message says how to share the owner's instead.
func TestCheckForConflicts_ARepositoryBranchHasOneGitProvider(t *testing.T) {
	owner := sharedProviderTarget("platform", "apps", "", "apps", 2*time.Hour)
	late := sharedProviderTarget("team-b", "infra", "", "infra", time.Hour)
	r := sharedProviderReconciler(t, owner, late)

	conflict, msg, reason, err := r.checkForConflicts(context.Background(), late, "team-b")
	require.NoError(t, err)
	assert.True(t, conflict)
	assert.Equal(t, GitTargetReasonTargetConflict, reason)
	assert.Contains(t, msg, "GitProvider 'platform/monorepo'")
	assert.Contains(t, msg, "spec.gitProviderRef.namespace")

	conflict, _, _, err = r.checkForConflicts(context.Background(), owner, "platform")
	require.NoError(t, err)
	assert.False(t, conflict, "the oldest GitTarget's GitProvider owns the branch")
}

// TestCheckForConflicts_ASharedGitProviderIsOneBranchAcrossNamespaces: a GitTarget that shares the
// owner's GitProvider from another namespace is on the owner's branch: no ownership conflict, and
// the path rules hold between it and the owner's GitTargets as within one namespace.
func TestCheckForConflicts_ASharedGitProviderIsOneBranchAcrossNamespaces(t *testing.T) {
	owner := sharedProviderTarget("platform", "apps", "", "apps", 2*time.Hour)
	shared := sharedProviderTarget("team-b", "infra", "platform", "infra", time.Hour)
	nested := sharedProviderTarget("team-c", "inside", "platform", "apps/inside", time.Minute)
	r := sharedProviderReconciler(t, owner, shared, nested)

	conflict, msg, _, err := r.checkForConflicts(context.Background(), shared, "platform")
	require.NoError(t, err)
	assert.False(t, conflict, msg)

	conflict, msg, reason, err := r.checkForConflicts(context.Background(), nested, "platform")
	require.NoError(t, err)
	assert.True(t, conflict, "a path nested in another namespace's folder on the same branch still overlaps")
	assert.Equal(t, GitTargetReasonTargetConflict, reason)
	assert.Contains(t, msg, "platform/apps")
}

// TestCheckForConflicts_AGitTargetThatMayNotWriteKeepsNoBranch: an older GitTarget naming a
// GitProvider whose accessFrom does not admit its namespace may not write the branch, so it must not
// get the GitTarget that may refused.
func TestCheckForConflicts_AGitTargetThatMayNotWriteKeepsNoBranch(t *testing.T) {
	refused := sharedProviderTarget("team-x", "early", "platform", "early", 2*time.Hour)
	own := sharedProviderTarget("team-b", "infra", "", "infra", time.Hour)
	r := sharedProviderReconciler(t, refused, own)

	conflict, msg, _, err := r.checkForConflicts(context.Background(), own, "team-b")
	require.NoError(t, err)
	assert.False(t, conflict, msg)
}
