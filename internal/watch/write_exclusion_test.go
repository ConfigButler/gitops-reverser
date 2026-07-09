// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func managedFieldsAt(entries ...metav1.ManagedFieldsEntry) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("ConfigMap")
	u.SetName("settings")
	u.SetNamespace("apps")
	u.SetManagedFields(entries)
	return u
}

func entry(manager string, at time.Time) metav1.ManagedFieldsEntry {
	ts := metav1.NewTime(at)
	return metav1.ManagedFieldsEntry{Manager: manager, Operation: metav1.ManagedFieldsOperationApply, Time: &ts}
}

func TestLastWriteFieldManagers(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		obj  *unstructured.Unstructured
		want []string
	}{
		"newest entry wins": {
			obj: managedFieldsAt(
				entry("kubectl", base),
				entry("kustomize-controller", base.Add(time.Minute)),
			),
			want: []string{"kustomize-controller"},
		},
		"older machine write does not mask a newer human write": {
			obj: managedFieldsAt(
				entry("kustomize-controller", base),
				entry("kubectl-edit", base.Add(time.Minute)),
			),
			want: []string{"kubectl-edit"},
		},
		"a timestamp tie reports both": {
			obj: managedFieldsAt(
				entry("kustomize-controller", base),
				entry("kubectl", base),
			),
			want: []string{"kubectl", "kustomize-controller"},
		},
		"entries without a timestamp are ignored": {
			obj: managedFieldsAt(
				metav1.ManagedFieldsEntry{Manager: "no-clock"},
				entry("kustomize-controller", base),
			),
			want: []string{"kustomize-controller"},
		},
		"all entries without a timestamp yield nothing": {
			obj:  managedFieldsAt(metav1.ManagedFieldsEntry{Manager: "no-clock"}),
			want: nil,
		},
		"no managedFields at all": {
			obj:  managedFieldsAt(),
			want: nil,
		},
		"nil object": {obj: nil, want: nil},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, lastWriteFieldManagers(tc.obj))
		})
	}
}

// managedFields names who last WROTE an object, never who deleted it. Evaluating a field
// manager exclusion on a DELETE would silently ignore a human deleting a Flux-managed
// resource — the exact failure a label selector has.
func TestLastWritersForOperation_DeleteHasNoFieldManager(t *testing.T) {
	t.Parallel()

	obj := managedFieldsAt(entry("kustomize-controller", time.Now()))
	assert.Equal(t, []string{"kustomize-controller"}, lastWritersForOperation("UPDATE", obj))
	assert.Equal(t, []string{"kustomize-controller"}, lastWritersForOperation("CREATE", obj))
	assert.Nil(t, lastWritersForOperation("DELETE", obj))
}

func allOps() OperationSet { return OperationSet{"*": struct{}{}} }

func TestRuleSelections_Admits_FieldManagerVeto(t *testing.T) {
	t.Parallel()

	excludingFlux := RuleSelections{{
		Ops:       allOps(),
		Exclusion: newWriteExclusion([]string{"kustomize-controller"}, nil),
	}}

	assert.False(t, excludingFlux.Admits("UPDATE", []string{"kustomize-controller"}, ""),
		"the forward leg's own apply must not be mirrored back")
	assert.True(t, excludingFlux.Admits("UPDATE", []string{"kubectl"}, ""),
		"a human's write is always mirrored")
	assert.True(t, excludingFlux.Admits("UPDATE", nil, ""),
		"an object with no usable managedFields fails open")
}

// A tie means we cannot tell which manager produced this write. Mirroring a machine's
// write is a smaller harm than dropping a human's, so a tie only excludes when every tied
// manager is excluded.
func TestRuleSelections_Admits_TimestampTieFailsOpen(t *testing.T) {
	t.Parallel()

	sel := RuleSelections{{Ops: allOps(), Exclusion: newWriteExclusion([]string{"kustomize-controller"}, nil)}}
	assert.True(t, sel.Admits("UPDATE", []string{"kubectl", "kustomize-controller"}, ""))

	both := RuleSelections{{
		Ops:       allOps(),
		Exclusion: newWriteExclusion([]string{"kustomize-controller", "helm-controller"}, nil),
	}}
	assert.False(t, both.Admits("UPDATE", []string{"helm-controller", "kustomize-controller"}, ""))
}

func TestRuleSelections_Admits_UserVeto(t *testing.T) {
	t.Parallel()

	flux := "system:serviceaccount:flux-system:kustomize-controller"
	sel := RuleSelections{{Ops: allOps(), Exclusion: newWriteExclusion(nil, []string{flux})}}

	assert.False(t, sel.Admits("DELETE", nil, flux), "excludeUsers does apply to deletes")
	assert.True(t, sel.Admits("UPDATE", nil, "jane@acme.com"))
	assert.True(t, sel.Admits("UPDATE", nil, ""),
		"an unresolved author fails open: losing a human's edit is worse than mirroring a machine's")
}

// Rules are a logical OR. An exclusion vetoes only inside the rule that declares it, so an
// unrestricted rule for the same type re-admits everything a restricted one excluded.
func TestRuleSelections_Admits_ExclusionIsPerRuleNotGlobal(t *testing.T) {
	t.Parallel()

	restricted := RuleSelection{Ops: allOps(), Exclusion: newWriteExclusion([]string{"kustomize-controller"}, nil)}
	unrestricted := RuleSelection{Ops: allOps()}

	assert.False(t, RuleSelections{restricted}.Admits("UPDATE", []string{"kustomize-controller"}, ""))
	assert.True(t, RuleSelections{restricted, unrestricted}.Admits("UPDATE", []string{"kustomize-controller"}, ""),
		"a second, unrestricted rule for the same type admits the write")
}

// An exclusion only vetoes writes its own rule selects. A rule that excludes Flux but
// watches only CREATE must not suppress Flux's UPDATE when another rule watches UPDATE.
func TestRuleSelections_Admits_RespectsPerRuleOperations(t *testing.T) {
	t.Parallel()

	createOnlyExcludingFlux := RuleSelection{
		Ops:       OperationSet{"CREATE": struct{}{}},
		Exclusion: newWriteExclusion([]string{"kustomize-controller"}, nil),
	}
	updateOnlyPlain := RuleSelection{Ops: OperationSet{"UPDATE": struct{}{}}}
	selections := RuleSelections{createOnlyExcludingFlux, updateOnlyPlain}

	assert.False(t, selections.Admits("CREATE", []string{"kustomize-controller"}, ""))
	assert.True(t, selections.Admits("UPDATE", []string{"kustomize-controller"}, ""))
}

func TestRuleSelections_Admits_NoRuleSelectsTheOperation(t *testing.T) {
	t.Parallel()
	sel := RuleSelections{{Ops: OperationSet{"CREATE": struct{}{}}}}
	assert.False(t, sel.Admits("DELETE", nil, ""))
}

func TestRuleSelections_NeedsAuthor(t *testing.T) {
	t.Parallel()

	assert.False(t, RuleSelections{{Ops: allOps()}}.NeedsAuthor())
	assert.False(t, RuleSelections{{
		Ops:       allOps(),
		Exclusion: newWriteExclusion([]string{"kustomize-controller"}, nil),
	}}.NeedsAuthor(), "a field-manager exclusion never needs the attribution grace window")
	assert.True(t, RuleSelections{{
		Ops:       allOps(),
		Exclusion: newWriteExclusion(nil, []string{"flux"}),
	}}.NeedsAuthor())
}

func TestRuleSelections_HasExclusionsAndOpsUnion(t *testing.T) {
	t.Parallel()

	plain := RuleSelections{{Ops: OperationSet{"CREATE": struct{}{}}}}
	assert.False(t, plain.HasExclusions())

	mixed := RuleSelections{
		{Ops: OperationSet{"CREATE": struct{}{}}},
		{Ops: OperationSet{"UPDATE": struct{}{}}, Exclusion: newWriteExclusion([]string{"flux"}, nil)},
	}
	assert.True(t, mixed.HasExclusions())
	assert.Equal(t, []string{"CREATE", "UPDATE"}, mixed.Ops().Sorted(),
		"the stream must carry every operation any clause selects")
}

func TestRuleSelections_ExclusionReason(t *testing.T) {
	t.Parallel()

	byManager := RuleSelections{{Ops: allOps(), Exclusion: newWriteExclusion([]string{"flux"}, nil)}}
	assert.Equal(t, exclusionReasonFieldManager, byManager.ExclusionReason("UPDATE", []string{"flux"}, ""))

	byUser := RuleSelections{{Ops: allOps(), Exclusion: newWriteExclusion(nil, []string{"sa:flux"})}}
	assert.Equal(t, exclusionReasonUser, byUser.ExclusionReason("DELETE", nil, "sa:flux"))
}

func TestNewWriteExclusion_NormalizesAndDeduplicates(t *testing.T) {
	t.Parallel()

	e := newWriteExclusion([]string{"  b  ", "a", "b", ""}, []string{"z", "z"})
	require.Equal(t, []string{"a", "b"}, e.FieldManagers)
	require.Equal(t, []string{"z"}, e.Users)
	assert.False(t, e.Empty())

	assert.True(t, newWriteExclusion(nil, nil).Empty())
	assert.True(t, newWriteExclusion([]string{"", "  "}, nil).Empty(),
		"a list of blanks excludes nobody")
	assert.Empty(t, newWriteExclusion(nil, nil).Key())
}

// Two rules declaring the same exclusions in a different order must fold into one clause,
// or the watched-type table would grow a clause per reconcile ordering.
func TestWriteExclusion_KeyIsOrderIndependent(t *testing.T) {
	t.Parallel()

	a := newWriteExclusion([]string{"x", "y"}, []string{"1", "2"})
	b := newWriteExclusion([]string{"y", "x"}, []string{"2", "1"})
	assert.Equal(t, a.Key(), b.Key())

	c := newWriteExclusion([]string{"x"}, []string{"1", "2"})
	assert.NotEqual(t, a.Key(), c.Key())
}
