// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// F4 (docs/design/applier-echoes.md): an object another applier owns is not mirrored into a coordinated
// folder.

func ownedObject(labels map[string]string, annotations map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}}
	u.SetLabels(labels)
	u.SetAnnotations(annotations)
	return u
}

func TestAppliedByOf_ReadsFluxLabelsAndArgoCDTracking(t *testing.T) {
	cases := []struct {
		name string
		obj  *unstructured.Unstructured
		want AppliedBy
	}{
		{"no owner", ownedObject(nil, nil), AppliedBy{}},
		{"flux", ownedObject(
			map[string]string{fluxOwnerNameLabel: "apps", fluxOwnerNamespaceLabel: "flux-system"}, nil),
			AppliedBy{Kind: configv1alpha3.ApplierKindKustomization, Namespace: "flux-system", Name: "apps"}},
		{"flux, name alone", ownedObject(map[string]string{fluxOwnerNameLabel: "apps"}, nil), AppliedBy{}},
		{"argo cd", ownedObject(nil, map[string]string{argoCDTrackingAnnotation: "shop:/ConfigMap:team-a/cfg"}),
			AppliedBy{Kind: configv1alpha3.ApplierKindApplication, Name: "shop"}},
		{"argo cd, an application in another namespace",
			ownedObject(nil, map[string]string{argoCDTrackingAnnotation: "tenants_shop:apps/Deployment:team-a/api"}),
			AppliedBy{Kind: configv1alpha3.ApplierKindApplication, Namespace: "tenants", Name: "shop"}},
		{"argo cd, malformed", ownedObject(nil, map[string]string{argoCDTrackingAnnotation: "shop"}), AppliedBy{}},
		{"nil", nil, AppliedBy{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, AppliedByOf(tc.obj))
		})
	}
}

func TestAppliedBy_IsTheApplierItNames(t *testing.T) {
	flux := applierRef("alpha")
	assert.True(t, AppliedBy{Kind: flux.Kind, Namespace: flux.Namespace, Name: flux.Name}.is(flux))
	assert.False(t, AppliedBy{Kind: flux.Kind, Namespace: "elsewhere", Name: flux.Name}.is(flux))
	assert.False(t, AppliedBy{Kind: configv1alpha3.ApplierKindApplication, Name: flux.Name}.is(flux))

	argo := argoApplierRef("shop")
	assert.True(t, AppliedBy{Kind: argo.Kind, Name: "shop"}.is(argo), "the control plane's own namespace is unnamed")
	assert.True(t, AppliedBy{Kind: argo.Kind, Namespace: argo.Namespace, Name: "shop"}.is(argo))
	assert.False(t, AppliedBy{Kind: argo.Kind, Namespace: "tenants", Name: "shop"}.is(argo))
}

// TestOwnership_ALiveWriteOfAnotherAppliersObjectIsNotMirrored: in alpha's coordinated folder, an
// object another Kustomization owns is kept out, counted; one alpha's applier owns, or nobody does,
// is mirrored. bravo's folder names no applier claim and excludes nothing.
func TestOwnership_ALiveWriteOfAnotherAppliersObjectIsNotMirrored(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	loop, _ := manualLoop(t, applierTestTarget("alpha", true), applierTestTarget("bravo", false))
	loop.followRecordedOperations(time.Now())
	other := AppliedBy{Kind: configv1alpha3.ApplierKindKustomization, Namespace: "flux-system", Name: "infra"}
	own := AppliedBy{Kind: configv1alpha3.ApplierKindKustomization, Namespace: "team-a", Name: "alpha"}

	assert.True(t, loop.ownedElsewhere("alpha", "team-a", "cm/x", other))
	assert.False(t, loop.ownedElsewhere("alpha", "team-a", "cm/y", own))
	assert.False(t, loop.ownedElsewhere("alpha", "team-a", "cm/z", AppliedBy{}))
	assert.False(
		t,
		loop.ownedElsewhere("bravo", "team-a", "cm/x", other),
		"a folder with no applier claim excludes nothing",
	)
	got, _ := telemetry.CollectInt64Sum(reader, "gitopsreverser_apply_ownership_conflicts_total", nil)
	assert.Equal(t, int64(1), got)

	event := windowEvent("alpha", "2", "kubectl-edit")
	event.AppliedBy = other
	loop.handleLiveEvents(&WriteRequest{Events: []Event{event}, CommitMode: CommitModePerEvent})
	assert.Nil(t, loop.openWindow, "the object another applier owns opened no window")
}

// TestOwnership_ASnapshotObjectOfAnotherApplierIsNeitherUpsertedNorSwept: the resync drops the object
// from what it upserts, and the sweep leaves its document alone even under prune.mode: Always.
func TestOwnership_ASnapshotObjectOfAnotherApplierIsNeitherUpsertedNorSwept(t *testing.T) {
	loop, _ := manualLoop(t, applierTestTarget("alpha", true))
	loop.followRecordedOperations(time.Now())
	theirs := desiredCM("theirs", "red")
	ours := desiredCM("ours", "blue")
	req := &ResyncRequest{
		GitTargetName: "alpha", GitTargetNamespace: "team-a",
		Desired: []manifestanalyzer.DesiredResource{theirs, ours},
		Owners: map[types.ResourceIdentifier]AppliedBy{
			theirs.Resource: {Kind: configv1alpha3.ApplierKindApplication, Name: "infra"},
		},
	}
	desired, kept := loop.withoutOwnedElsewhere(req)
	require.Len(t, desired, 1)
	assert.Equal(t, "ours", desired[0].Resource.Name)
	assert.Equal(t, map[string]bool{objectKey(theirs.Resource): true}, kept)

	writer := newContentWriter(types.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	theirFile := seedPlacedManifest(t, worktree, "apps/theirs.yaml", cmManifest("theirs", "green"))
	w := &BranchWorker{contentWriter: writer, mapper: configMapMapper()}
	stats, _, _, err := w.planResyncToWorktree(context.Background(), worktree, "",
		ResolvedTargetMetadata{PruneMode: configv1alpha3.PruneAlways}, desired,
		resyncScopePtr(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, ""), kept)
	require.NoError(t, err)
	assert.Zero(t, stats.Deleted, "the other applier's document is not swept")
	content, err := os.ReadFile(theirFile)
	require.NoError(t, err)
	assert.Contains(t, string(content), "green", "and not upserted")
}
