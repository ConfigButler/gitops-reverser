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

// sharedProviderTarget is a GitTarget named name in namespace, writing path through the GitBranch
// providerNS/monorepo-main (empty providerNS: its own namespace), created age ago.
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
			KubeProviderRef: triggerProviderRef(triggerLocalProvider),
			GitBranchRef: meta.NamespacedObjectReference{
				Name:      gitBranchNameForTest("monorepo", "main"),
				Namespace: providerNS,
			},
			Path: path,
		},
	}
}

func sharedProviderReconciler(t *testing.T, objects ...ctrlclient.Object) *GitTargetReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configbutleraiv1alpha3.AddToScheme(scheme))
	// The KubeProvider every GitTarget here mirrors through admits every namespace.
	local := admitting(triggerLocalProvider)
	local.Spec.AccessFrom = &configbutleraiv1alpha3.NamespaceMatcher{Selector: &metav1.LabelSelector{}}
	objects = append(objects, local)
	for _, ns := range []string{"platform", "team-b"} {
		objects = append(objects, &configbutleraiv1alpha3.GitProvider{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "monorepo"},
			Spec:       configbutleraiv1alpha3.GitProviderSpec{URL: "https://github.com/acme/platform"},
		})
	}
	return &GitTargetReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
}

// TestCheckForConflicts_ASharedGitBranchIsOneBranchAcrossNamespaces: GitTargets in several
// namespaces referencing one GitBranch write one branch, and the path rules hold between them as
// within one namespace. Who may write the branch at all is the GitBranch's claim, not a conflict.
func TestCheckForConflicts_ASharedGitBranchIsOneBranchAcrossNamespaces(t *testing.T) {
	owner := sharedProviderTarget("platform", "apps", "", "apps", 2*time.Hour)
	shared := sharedProviderTarget("team-b", "infra", "platform", "infra", time.Hour)
	nested := sharedProviderTarget("team-c", "inside", "platform", "apps/inside", time.Minute)
	r := sharedProviderReconciler(t, owner, shared, nested)

	conflict, msg, _, err := r.checkForConflicts(context.Background(), shared)
	require.NoError(t, err)
	assert.False(t, conflict, msg)

	conflict, msg, reason, err := r.checkForConflicts(context.Background(), nested)
	require.NoError(t, err)
	assert.True(t, conflict, "a path nested in another namespace's folder on the same branch still overlaps")
	assert.Equal(t, GitTargetReasonTargetConflict, reason)
	assert.Contains(t, msg, "platform/apps")
}
