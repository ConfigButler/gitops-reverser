// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// forgettingCursorStore satisfies both CursorStore and CursorForgetter.
type forgettingCursorStore struct {
	forgotten []string
	err       error
}

func (s *forgettingCursorStore) LookupWatchCursor(
	context.Context, string, schema.GroupVersionResource, string,
) (string, bool) {
	return "", false
}

func (s *forgettingCursorStore) RecordWatchCursor(
	context.Context, string, schema.GroupVersionResource, string, string,
) error {
	return nil
}

func (s *forgettingCursorStore) ForgetWatchCursors(_ context.Context, uid string) error {
	s.forgotten = append(s.forgotten, uid)
	return s.err
}

// plainCursorStore is a CursorStore that cannot forget — the shape a future non-Redis
// implementation might have. It must simply not be asked.
type plainCursorStore struct{}

func (plainCursorStore) LookupWatchCursor(
	context.Context, string, schema.GroupVersionResource, string,
) (string, bool) {
	return "", false
}

func (plainCursorStore) RecordWatchCursor(
	context.Context, string, schema.GroupVersionResource, string, string,
) error {
	return nil
}

func TestRetargetGitTarget_DropsCursorsForTheTargetsUID(t *testing.T) {
	t.Parallel()

	store := &forgettingCursorStore{}
	m := &Manager{Log: logr.Discard(), WatchCursorStore: store}
	gitDest := types.NewResourceReference("acme", "team-a").WithUID("uid-1")

	require.NoError(t, m.RetargetGitTarget(context.Background(), gitDest))

	assert.Equal(t, []string{"uid-1"}, store.forgotten,
		"a retarget keeps the object's UID, so its cursors must be dropped explicitly")
}

// Silently resuming into a new folder would mirror an arbitrary suffix of the cluster's
// state, so a cursor store that cannot be reached fails the reconcile.
func TestRetargetGitTarget_ReportsAFailingCursorStore(t *testing.T) {
	t.Parallel()

	store := &forgettingCursorStore{err: errors.New("redis down")}
	m := &Manager{Log: logr.Discard(), WatchCursorStore: store}

	err := m.RetargetGitTarget(context.Background(), types.NewResourceReference("acme", "team-a").WithUID("uid-1"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "redis down")
	assert.Contains(t, err.Error(), "team-a/acme")
}

func TestRetargetGitTarget_NoCursorStoreIsFine(t *testing.T) {
	t.Parallel()

	// Without Redis every watch cold-replays anyway, so there is nothing to forget.
	m := &Manager{Log: logr.Discard()}
	require.NoError(t, m.RetargetGitTarget(context.Background(),
		types.NewResourceReference("acme", "team-a").WithUID("uid-1")))

	m = &Manager{Log: logr.Discard(), WatchCursorStore: plainCursorStore{}}
	require.NoError(t, m.RetargetGitTarget(context.Background(),
		types.NewResourceReference("acme", "team-a").WithUID("uid-1")))
}

// A GitTarget that never declared has no UID to key cursors by.
func TestRetargetGitTarget_NeverDeclaredTargetForgetsNothing(t *testing.T) {
	t.Parallel()

	store := &forgettingCursorStore{}
	m := &Manager{Log: logr.Discard(), WatchCursorStore: store}

	require.NoError(t, m.RetargetGitTarget(context.Background(), types.NewResourceReference("acme", "team-a")))
	assert.Empty(t, store.forgotten)
}
