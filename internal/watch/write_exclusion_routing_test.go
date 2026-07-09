// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// stubAuthorResolver names every event's author without touching Redis, and counts how
// often it was consulted so the tests can pin that attribution is not paid twice.
type stubAuthorResolver struct {
	username string
	found    bool
	calls    int
}

func (s *stubAuthorResolver) ResolveAuthor(
	context.Context, schema.GroupVersionResource, k8stypes.UID, string, bool,
) (git.UserInfo, bool) {
	s.calls++
	if !s.found {
		return git.UserInfo{}, false
	}
	return git.UserInfo{Username: s.username}, true
}

type exclusionHarness struct {
	manager  *Manager
	enqueuer *recordingEnqueuer
	gitDest  types.ResourceReference
	key      targetWatchKey
}

func newExclusionHarness(t *testing.T, resolver AuthorResolver) *exclusionHarness {
	t.Helper()
	gitDest := types.NewResourceReference("target", "default")
	enqueuer := &recordingEnqueuer{}
	stream := reconcile.NewGitTargetEventStream(gitDest.Name, gitDest.Namespace, enqueuer, logr.Discard())
	router := &EventRouter{
		Log:              logr.Discard(),
		gitTargetStreams: map[string]*reconcile.GitTargetEventStream{gitDest.Key(): stream},
	}
	return &exclusionHarness{
		manager:  &Manager{EventRouter: router, AuthorResolver: resolver},
		enqueuer: enqueuer,
		gitDest:  gitDest,
		key:      targetWatchKey{GVR: configmapsGVR, Namespace: "apps"},
	}
}

func (h *exclusionHarness) route(t *testing.T, filter watchFilter, ev watch.Event) {
	t.Helper()
	_, err := h.manager.routeLiveTargetWatchEvent(context.Background(), logr.Discard(), h.gitDest, h.key, filter, ev)
	require.NoError(t, err)
}

// configMapWrittenBy returns a live ConfigMap whose newest managedFields entry names
// manager — the object a watch delivers after that manager applied it.
func configMapWrittenBy(rv, manager string) *unstructured.Unstructured {
	obj := configMapObject(rv)
	now := metav1.NewTime(time.Now())
	older := metav1.NewTime(now.Add(-time.Hour))
	obj.SetManagedFields([]metav1.ManagedFieldsEntry{
		{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationUpdate, Time: &older},
		{Manager: manager, Operation: metav1.ManagedFieldsOperationApply, Time: &now},
	})
	return obj
}

// configMapWrittenByWithData is configMapWrittenBy with a distinct data value, so the event
// carries a real Git-content change rather than one the content dedup would drop anyway.
func configMapWrittenByWithData(t *testing.T, rv, manager, flavor string) *unstructured.Unstructured {
	t.Helper()
	obj := configMapWrittenBy(rv, manager)
	require.NoError(t, unstructured.SetNestedField(obj.Object, flavor, "data", "flavor"))
	return obj
}

// The whole point: a GitOps forward leg applies this branch back into the cluster, and
// that apply must not be committed as a new change.
func TestRouteLiveTargetWatchEvent_ExcludesForwardLegApply(t *testing.T) {
	h := newExclusionHarness(t, nil)
	filter := exclusionFilter(allOps(), []string{"kustomize-controller"}, nil)

	h.route(t, filter, watch.Event{Type: watch.Modified, Object: configMapWrittenBy("12", "kustomize-controller")})
	assert.Empty(t, h.enqueuer.events, "the forward leg's own apply must not reach the branch worker")

	h.route(t, filter, watch.Event{Type: watch.Modified, Object: configMapWrittenBy("13", "kubectl-edit")})
	require.Len(t, h.enqueuer.events, 1, "a human's later edit of the same object is still mirrored")
	assert.Equal(t, "UPDATE", h.enqueuer.events[0].Operation)
}

// A field-manager exclusion must never suppress a delete: managedFields still names the
// forward leg as the last writer, but the actor who deleted the object may be a human.
func TestRouteLiveTargetWatchEvent_FieldManagerExclusionNeverSuppressesDelete(t *testing.T) {
	h := newExclusionHarness(t, nil)
	filter := exclusionFilter(allOps(), []string{"kustomize-controller"}, nil)

	h.route(t, filter, watch.Event{Type: watch.Deleted, Object: configMapWrittenBy("14", "kustomize-controller")})

	require.Len(t, h.enqueuer.events, 1)
	assert.Equal(t, "DELETE", h.enqueuer.events[0].Operation)
}

func TestRouteLiveTargetWatchEvent_ExcludesAttributedUser(t *testing.T) {
	flux := "system:serviceaccount:flux-system:kustomize-controller"
	resolver := &stubAuthorResolver{username: flux, found: true}
	h := newExclusionHarness(t, resolver)
	filter := exclusionFilter(allOps(), nil, []string{flux})

	h.route(t, filter, watch.Event{Type: watch.Modified, Object: configMapObject("12")})
	assert.Empty(t, h.enqueuer.events)

	// A delete by the same identity is excluded too — the audit fact names the deleter.
	h.route(t, filter, watch.Event{Type: watch.Deleted, Object: configMapObject("13")})
	assert.Empty(t, h.enqueuer.events)
}

// Dropping a change because we failed to identify its author would silently lose a
// human's edit. An unresolved author is mirrored.
func TestRouteLiveTargetWatchEvent_UnresolvedAuthorFailsOpen(t *testing.T) {
	resolver := &stubAuthorResolver{found: false}
	h := newExclusionHarness(t, resolver)
	filter := exclusionFilter(allOps(), nil, []string{"system:serviceaccount:flux-system:kustomize-controller"})

	h.route(t, filter, watch.Event{Type: watch.Modified, Object: configMapObject("12")})

	require.Len(t, h.enqueuer.events, 1)
	assert.Empty(t, h.enqueuer.events[0].UserInfo.Username)
}

// Attribution costs a bounded grace-window wait. It is resolved once when excludeUsers
// forces it early, and the result is reused for the commit rather than looked up again.
func TestRouteLiveTargetWatchEvent_AuthorResolvedOnceWhenExcludeUsersIsSet(t *testing.T) {
	resolver := &stubAuthorResolver{username: "jane@acme.com", found: true}
	h := newExclusionHarness(t, resolver)
	filter := exclusionFilter(allOps(), nil, []string{"someone-else"})

	h.route(t, filter, watch.Event{Type: watch.Added, Object: configMapObject("12")})

	require.Len(t, h.enqueuer.events, 1)
	assert.Equal(t, "jane@acme.com", h.enqueuer.events[0].UserInfo.Username)
	assert.Equal(t, 1, resolver.calls, "the early lookup must be reused, not repeated")
}

// A field-manager exclusion reads watch state alone, so it must not consult the
// attribution index at all — that is the property that makes it race-free and usable in
// configured-author mode.
func TestRouteLiveTargetWatchEvent_FieldManagerExclusionDoesNotResolveAuthorEarly(t *testing.T) {
	resolver := &stubAuthorResolver{username: "jane@acme.com", found: true}
	h := newExclusionHarness(t, resolver)
	filter := exclusionFilter(allOps(), []string{"kustomize-controller"}, nil)

	h.route(t, filter, watch.Event{Type: watch.Modified, Object: configMapWrittenBy("12", "kustomize-controller")})

	assert.Empty(t, h.enqueuer.events)
	assert.Zero(t, resolver.calls, "an excluded write must not pay the attribution grace window")
}

// An excluded write must not seed the content-dedup cache: if it did, a human later
// writing that exact content would be deduped against a change that never reached Git.
func TestRouteLiveTargetWatchEvent_ExcludedWriteDoesNotSeedDedupCache(t *testing.T) {
	h := newExclusionHarness(t, nil)
	filter := exclusionFilter(allOps(), []string{"kustomize-controller"}, nil)

	// A human's write seeds the cache with "vanilla".
	h.route(
		t,
		filter,
		watch.Event{Type: watch.Added, Object: configMapWrittenByWithData(t, "11", "kubectl", "vanilla")},
	)
	require.Len(t, h.enqueuer.events, 1)

	// The forward leg writes "chocolate". Excluded, so it must not become the cached hash.
	h.route(t, filter, watch.Event{
		Type: watch.Modified, Object: configMapWrittenByWithData(t, "12", "kustomize-controller", "chocolate"),
	})
	require.Len(t, h.enqueuer.events, 1, "the forward leg's write is not mirrored")

	// A human now writes the very content the forward leg wrote. If the excluded write had
	// seeded the cache, this would be deduped away and the human's edit lost.
	h.route(t, filter, watch.Event{
		Type: watch.Modified, Object: configMapWrittenByWithData(t, "13", "kubectl-edit", "chocolate"),
	})
	require.Len(t, h.enqueuer.events, 2,
		"the human's write must reach Git; it was never deduped against the excluded one")
}

// A GitOps tool's own labels and annotations are stripped from Git content by
// internal/sanitize, so an apply that only stamps them produces no Git-writable change and
// the content dedup drops it before any exclusion is consulted. Worth pinning: it is why the
// e2e drives the forward leg with a data change rather than a label, and why an operator who
// sees no excluded-event metric should look for a content change, not a label.
func TestRouteLiveTargetWatchEvent_OperationalLabelsAreNotGitContent(t *testing.T) {
	h := newExclusionHarness(t, nil)
	filter := opsFilter(allOps())

	h.route(t, filter, watch.Event{Type: watch.Added, Object: configMapWrittenBy("11", "kubectl")})
	require.Len(t, h.enqueuer.events, 1)

	labelled := configMapWrittenBy("12", "kustomize-controller")
	labelled.SetLabels(map[string]string{"kustomize.toolkit.fluxcd.io/name": "demo"})
	h.route(t, filter, watch.Event{Type: watch.Modified, Object: labelled})

	assert.Len(t, h.enqueuer.events, 1,
		"a Flux label is not Git content, so the update carries no change to mirror")
}
