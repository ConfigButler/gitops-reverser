// SPDX-License-Identifier: Apache-2.0

package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// labeledConfigMapEvent is newConfigMapEvent with metadata.labels set on the LIVE object —
// the only place a "{label:key}" template can read them from, since placement runs precisely
// when the resource has no document in Git.
func labeledConfigMapEvent(name, namespace string, labels map[string]string) Event {
	event := newConfigMapEvent(name, namespace)
	meta, _ := event.Object.Object["metadata"].(map[string]interface{})
	asAny := make(map[string]interface{}, len(labels))
	for k, v := range labels {
		asAny[k] = v
	}
	meta["labels"] = asAny
	return event
}

func targetedLabeledConfigMapEvent(name, namespace string, labels map[string]string) Event {
	event := labeledConfigMapEvent(name, namespace, labels)
	event.GitTargetName = metricsTestGitTargetName
	event.GitTargetNamespace = metricsTestGitTargetNamespace
	return event
}

// The write path's half of the feature: the label has to survive the trip from the live object
// into PlacementRequest, which is the one wiring a unit test of the analyzer cannot prove.
func TestPlacement_LabelTemplate_ReadsTheLiveObjectsLabels(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	policy := &manifestanalyzer.PlacementPolicy{
		ByType: map[string]string{"v1/configmaps": "{label:app.kubernetes.io/instance}/configmaps.yaml"},
	}

	changed := applyEventsWithPolicy(t, worktree, policy,
		labeledConfigMapEvent("cache", "app", map[string]string{"app.kubernetes.io/instance": "voter"}))
	require.True(t, changed)

	got, err := os.ReadFile(filepath.Join(root, "voter", "configmaps.yaml"))
	require.NoError(t, err, "the new file must land under the label's value")
	assert.Contains(t, string(got), "name: cache")
}

// Two resources sharing a label value bundle into one file, exactly as any other bundling
// template does — that grouping is the whole reason to place by label.
func TestPlacement_LabelTemplate_BundlesResourcesSharingAValue(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	policy := &manifestanalyzer.PlacementPolicy{
		ByType: map[string]string{"v1/configmaps": "{label:round}/submissions.yaml"},
	}

	applyEventsWithPolicy(t, worktree, policy,
		labeledConfigMapEvent("first", "app", map[string]string{"round": "r7"}))
	applyEventsWithPolicy(t, worktree, policy,
		labeledConfigMapEvent("second", "app", map[string]string{"round": "r7"}))

	got, err := os.ReadFile(filepath.Join(root, "r7", "submissions.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(got), "name: first")
	assert.Contains(t, string(got), "name: second")
}

// A resource missing the label still lands in the mirror, at the built-in "_unlabeled" sentinel,
// rather than being silently absent until somebody labels it.
func TestPlacement_MissingLabel_WritesToTheUnlabeledSentinel(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	policy := &manifestanalyzer.PlacementPolicy{
		ByType: map[string]string{"v1/configmaps": "{label:round}/submissions.yaml"},
	}

	changed := applyEventsWithPolicy(t, worktree, policy,
		targetedLabeledConfigMapEvent("cache", "app", nil))
	require.True(t, changed)

	got, err := os.ReadFile(filepath.Join(root, "_unlabeled", "submissions.yaml"))
	require.NoError(t, err, "an unlabeled resource must still be placed, at the sentinel bucket")
	assert.Contains(t, string(got), "name: cache")
}

// A later label does not move the file already written: placement is match-first and create-time
// only, so a resource labeled after the fact keeps living at the sentinel path it first resolved.
func TestPlacement_MissingLabel_LaterLabelDoesNotMoveTheFile(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	policy := &manifestanalyzer.PlacementPolicy{
		ByType: map[string]string{"v1/configmaps": "{label:round}/submissions.yaml"},
	}

	applyEventsWithPolicy(t, worktree, policy, labeledConfigMapEvent("cache", "app", nil))
	applyEventsWithPolicy(t, worktree, policy,
		labeledConfigMapEvent("cache", "app", map[string]string{"round": "r7"}))

	got, err := os.ReadFile(filepath.Join(root, "_unlabeled", "submissions.yaml"))
	require.NoError(t, err, "the document is match-first: it stays where it was first placed")
	assert.Contains(t, string(got), "name: cache")

	_, statErr := os.Stat(filepath.Join(root, "r7", "submissions.yaml"))
	assert.True(t, os.IsNotExist(statErr), "a later label must not create or move to a new file")
}
