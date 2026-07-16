// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const postBuildTokenManifest = `apiVersion: v1
kind: ConfigMap
metadata:
  name: region
  namespace: default
data:
  value: ${REGION}
`

func postBuildTokenEvent() Event {
	return Event{
		Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "region", "namespace": "default"},
			"data":       map[string]interface{}{"value": "us-east"},
		}},
		Identifier: types.ResourceIdentifier{
			Version: "v1", Resource: "configmaps", Namespace: "default", Name: "region",
		},
		Operation: "UPDATE",
	}
}

func seedPostBuildTokenManifest(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "configmap.yaml")
	require.NoError(t, os.WriteFile(path, []byte(postBuildTokenManifest), 0o600))
	return path
}

// Both mutation paths reach the same boundary. In particular, resync must not be a back door
// that turns an externally resolved ${REGION} value into a source-file edit.
func TestRenderFidelityRefusal_BlocksLiveAndResyncWrites(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(worker *BranchWorker, worktree *gogit.Worktree) error
	}{
		{
			name: "live event",
			run: func(worker *BranchWorker, worktree *gogit.Worktree) error {
				_, err := worker.flushEventsToWorktree(
					context.Background(), worktree, "", []Event{postBuildTokenEvent()}, nil)
				return err
			},
		},
		{
			name: "scoped resync",
			run: func(worker *BranchWorker, worktree *gogit.Worktree) error {
				_, _, err := worker.applyResyncToWorktree(
					context.Background(), worktree, "", "",
					[]manifestanalyzer.DesiredResource{{
						Resource: postBuildTokenEvent().Identifier,
						Object:   postBuildTokenEvent().Object,
					}}, nil, nil)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			worktree := newWorktreeForTest(t)
			path := seedPostBuildTokenManifest(t, worktree.Filesystem.Root())
			worker := &BranchWorker{
				contentWriter: newContentWriter(types.SensitiveResourcePolicy{}),
				mapper:        configMapMapper(),
			}

			err := test.run(worker, worktree)
			var refused *manifestanalyzer.AcceptanceRefusedError
			require.ErrorAs(t, err, &refused)
			assert.True(t, refused.AllIssuesOfKinds(manifestanalyzer.IssueRenderDoesNotMatchLive))
			assert.Contains(t, refused.Error(), "${REGION}")
			assertFileBytes(t, path, postBuildTokenManifest, "a fidelity refusal must not change Git")
		})
	}
}
