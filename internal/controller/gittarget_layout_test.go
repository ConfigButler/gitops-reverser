// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/layoutfixture"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func layoutTestTarget() *configbutleraiv1alpha3.GitTarget {
	return &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop", Generation: 4},
	}
}

func publishForTest(t *testing.T, report git.LayoutReport, scanned bool) *configbutleraiv1alpha3.GitTarget {
	t.Helper()
	target := layoutTestTarget()
	st := beginStatus(nil, nil, target, &target.Status.Conditions)
	publishLayout(st, target, report, scanned)
	return target
}

func layoutConditionOf(t *testing.T, target *configbutleraiv1alpha3.GitTarget) metav1.Condition {
	t.Helper()
	for _, c := range target.Status.Conditions {
		if c.Type == GitTargetConditionLayoutResolved {
			return c
		}
	}
	t.Fatalf("LayoutResolved is not published")
	return metav1.Condition{}
}

// The stanza is a fact about the folder, so a target that has never written still carries it.
// This is the property status.placement rests on: renderRoot must not wait for a placement.
func TestPublishLayout_SingleKustomization(t *testing.T) {
	observed := time.Date(2026, 7, 30, 9, 14, 22, 0, time.UTC)
	target := publishForTest(t, git.LayoutReport{
		LayoutResolution: manifestanalyzer.LayoutResolution{
			Reason:             manifestanalyzer.LayoutSingleKustomization,
			RenderRoot:         ".",
			SerializeNamespace: new(bool),
			Examples: []manifestanalyzer.LayoutExample{
				{Type: "v1/secrets", Path: "secrets/example.yaml", Source: manifestanalyzer.PlacementSourceDeclared},
			},
		},
		ByTypeEntries: 1,
		Revision:      "9f3c1ab",
		ObservedTime:  observed,
	}, true)

	require.NotNil(t, target.Status.Placement)
	assert.Equal(t, ".", target.Status.Placement.RenderRoot)
	require.NotNil(t, target.Status.Placement.SerializeNamespace)
	assert.False(t, *target.Status.Placement.SerializeNamespace)
	assert.Equal(t, int32(1), target.Status.Placement.ByTypeEntries)
	assert.Equal(t, "9f3c1ab", target.Status.Placement.ObservedRevision)
	require.NotNil(t, target.Status.Placement.ObservedTime)
	assert.Equal(t, observed, target.Status.Placement.ObservedTime.Time.UTC())
	require.Len(t, target.Status.Placement.Examples, 1)
	assert.Equal(t, "secrets/example.yaml", target.Status.Placement.Examples[0].Path)
	assert.Equal(t, "declared", target.Status.Placement.Examples[0].Source)

	condition := layoutConditionOf(t, target)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, "SingleKustomization", condition.Reason)
	assert.Equal(t, int64(4), condition.ObservedGeneration)
}

// None is True, not False. A folder with no kustomization is the ordinary case, and reporting the
// ordinary case as False is how a condition gets trained out of a reader's attention.
func TestPublishLayout_NoKustomizationIsHealthy(t *testing.T) {
	target := publishForTest(t, git.LayoutReport{
		LayoutResolution: manifestanalyzer.LayoutResolution{Reason: manifestanalyzer.LayoutNone},
	}, true)

	condition := layoutConditionOf(t, target)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, "None", condition.Reason)
}

// The rule PR 1 ships, as a user reads it: False, named Ambiguous, and the message says which
// folders the target actually covers rather than only how many.
func TestPublishLayout_AmbiguousNamesTheRoots(t *testing.T) {
	target := publishForTest(t, git.LayoutReport{
		LayoutResolution: manifestanalyzer.LayoutResolution{
			Reason:      manifestanalyzer.LayoutAmbiguous,
			RenderRoots: []string{"overlays/prod", "overlays/test"},
		},
	}, true)

	condition := layoutConditionOf(t, target)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, "Ambiguous", condition.Reason)
	assert.Contains(t, condition.Message, "overlays/prod, overlays/test")
	require.NotNil(t, target.Status.Placement)
	assert.Empty(t, target.Status.Placement.RenderRoot, "no arbitrary pick reaches status")
	assert.Nil(t, target.Status.Placement.SerializeNamespace)
}

// The controller's half of docs/layout/shapes/6-kustomize-base-and-overlays. The corpus asserts
// that fixture's GitPathAccepted condition, which is the writer's own; LayoutResolved is projected
// here, from data no write-path test can produce, so without this the fixture could claim anything
// about it and stay green.
func TestPublishLayout_AmbiguousMatchesTheCorpusFixture(t *testing.T) {
	want, err := layoutfixture.ReadCondition(
		layoutfixture.Path("shapes", "6-kustomize-base-and-overlays", "expected-app-root-status.yaml"),
		GitTargetConditionLayoutResolved)
	require.NoError(t, err)

	// The roots the fixture's repository actually holds, in the order a scan reports them.
	target := publishForTest(t, git.LayoutReport{
		LayoutResolution: manifestanalyzer.LayoutResolution{
			Reason: manifestanalyzer.LayoutAmbiguous,
			RenderRoots: []string{
				"base", "overlays/acceptance", "overlays/prod", "overlays/test",
			},
		},
	}, true)

	got := layoutConditionOf(t, target)
	assert.Equal(t, want.Status, string(got.Status))
	assert.Equal(t, want.Reason, got.Reason)
	assert.Equal(t, want.Message, got.Message,
		"the message the controller publishes and the one the fixture claims disagree")
}

// The third condition those fixtures carry. A refused Git path is terminal until a human changes
// the folder, so it must reach the kstatus trio as Stalled=True under its own reason rather than
// as a transient — otherwise a target that will never converge reads as one that is still trying.
func TestGitTargetReadiness_StalledFollowsGitPathAccepted(t *testing.T) {
	for _, fixture := range []struct{ dir, file string }{
		{"6-kustomize-base-and-overlays", "expected-app-root-status.yaml"},
		{"8-base-owned-field-edit", "expected-env-change-status.yaml"},
	} {
		t.Run(fixture.dir, func(t *testing.T) {
			path := layoutfixture.Path("shapes", fixture.dir, fixture.file)
			gitPath, err := layoutfixture.ReadCondition(path, GitTargetConditionGitPathAccepted)
			require.NoError(t, err)
			wantStalled, err := layoutfixture.ReadCondition(path, ConditionTypeStalled)
			require.NoError(t, err)

			rd := newGitTargetReadiness()
			gitTargetReadinessGates(rd, dataPlaneObservation{
				axes: gitTargetAxes{
					Streams: conditionValue{Status: metav1.ConditionTrue},
					GitPath: conditionValue{
						Status:  metav1.ConditionFalse,
						Reason:  gitPath.Reason,
						Message: gitPath.Message,
					},
					Render: conditionValue{Status: metav1.ConditionTrue},
				},
			}, healthyDependency(), healthyDependency(), healthyDependency())

			trio := rd.trio()
			assert.Equal(t, wantStalled.Status, string(trio.Stalled.Status))
			assert.Equal(t, wantStalled.Reason, trio.Stalled.Reason,
				"Stalled must carry the refusal's own reason, not a generic one")
			assert.Equal(t, metav1.ConditionFalse, trio.Ready.Status,
				"a refused Git path is not a converged target")
		})
	}
}

func healthyDependency() conditionValue {
	return conditionValue{Status: metav1.ConditionTrue, Reason: ReasonSucceeded}
}

// Not-yet-scanned is Unknown and writes no stanza. It is a different state from a folder that
// resolved to nothing, and collapsing the two would make a target that has never read its folder
// indistinguishable from one that read it and found a plain directory.
func TestPublishLayout_UnscannedIsUnknownAndWritesNoStanza(t *testing.T) {
	target := publishForTest(t, git.LayoutReport{}, false)

	condition := layoutConditionOf(t, target)
	assert.Equal(t, metav1.ConditionUnknown, condition.Status)
	assert.Equal(t, GitTargetReasonLayoutNotScanned, condition.Reason)
	assert.Nil(t, target.Status.Placement)
}

// A standing annotation forces exactly one re-read. Without the tracker every reconcile after the
// annotation was set would force one, which turns a one-shot request into a permanent re-anchor.
func TestReconcileRequestTracker_TakesEachValueOnce(t *testing.T) {
	var tracker reconcileRequestTracker
	ref := types.NewResourceReference("checkout", "shop")

	assert.False(t, tracker.take(ref, ""), "no annotation is not a request")
	assert.True(t, tracker.take(ref, "2026-08-31T10:00:00Z"), "a new request is taken")
	assert.False(t, tracker.take(ref, "2026-08-31T10:00:00Z"), "the same request is not taken twice")
	assert.True(t, tracker.take(ref, "2026-08-31T10:05:00Z"), "a changed value is a new request")

	// A different target's request is independent of this one's.
	assert.True(t, tracker.take(types.NewResourceReference("other", "shop"), "2026-08-31T10:05:00Z"))

	// Forgetting a deleted target releases its record, so a recreate is a fresh request.
	tracker.forget(ref)
	assert.True(t, tracker.take(ref, "2026-08-31T10:05:00Z"))
}

// The predicate has to admit the annotation change specifically: it does not bump
// metadata.generation, so GenerationChangedPredicate alone would filter the request out along
// with the controller's own status writes.
func TestReconcileRequestedOrSpecChanged(t *testing.T) {
	annotated := func(value string, generation int64) *configbutleraiv1alpha3.GitTarget {
		target := layoutTestTarget()
		target.Generation = generation
		if value != "" {
			target.Annotations = map[string]string{ReconcileRequestAnnotation: value}
		}
		return target
	}
	p := reconcileRequestedOrSpecChanged()

	assert.True(t, p.Update(event.UpdateEvent{
		ObjectOld: annotated("", 1), ObjectNew: annotated("now", 1),
	}), "a reconcile request must pass the predicate")

	assert.True(t, p.Update(event.UpdateEvent{
		ObjectOld: annotated("", 1), ObjectNew: annotated("", 2),
	}), "a spec change must still pass")

	assert.False(t, p.Update(event.UpdateEvent{
		ObjectOld: annotated("now", 1), ObjectNew: annotated("now", 1),
	}), "a status-only write must still be filtered")
}
