// SPDX-License-Identifier: Apache-2.0

package manifestreport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// dirtyConfigMap is an API object carrying operational noise the projection
// strips: a status, a server-set resourceVersion, and an operational annotation.
func dirtyConfigMap() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":            "app",
			"namespace":       "default",
			"resourceVersion": "12345",
			"annotations": map[string]interface{}{
				"kubectl.kubernetes.io/last-applied-configuration": "{}",
				"team": "payments",
			},
		},
		"data":   map[string]interface{}{"color": "blue"},
		"status": map[string]interface{}{"observedGeneration": int64(2)},
	}}
}

// The integration renderer must be byte-identical to the renderer the Git writer
// uses (internal/git/content_writer.go buildContentForWrite calls
// sanitize.MarshalToOrderedYAML on an already-sanitized object). If these ever
// diverge, whole-replace/new-file output would no longer match committed content.
func TestRender_MatchesWriterHouseFormat(t *testing.T) {
	raw := dirtyConfigMap()

	// What the writer would commit: MarshalToOrderedYAML on the sanitized object.
	want, err := sanitize.MarshalToOrderedYAML(sanitize.Sanitize(raw))
	require.NoError(t, err)

	got, err := Render(Project(raw))
	require.NoError(t, err)

	assert.Equal(t, string(want), string(got), "the integration renderer must match the Git writer")
}

// Whole-document replacement through manifestedit, using the injected production
// options, must produce exactly the house format — proving new-file and
// fallback output stay in lockstep with the writer.
func TestRender_WholeReplaceMatchesHouseFormat(t *testing.T) {
	raw := dirtyConfigMap()
	want, err := sanitize.MarshalToOrderedYAML(sanitize.Sanitize(raw))
	require.NoError(t, err)

	// A top-level sequence is not a KRM object, forcing manifestedit to fall back
	// to a canonical whole-document render via the injected Render.
	res, diags := manifestedit.PatchDocument([]byte("- a\n- b\n"), 0, Project(raw), EditOptions())
	require.Equal(t, manifestedit.EditWholeReplace, res.Mode)
	require.NotEmpty(t, diags)

	// The file had a single document, so its whole content is that rendered body.
	assert.Equal(t, string(want), string(res.Content),
		"whole-replace output must be the house canonical format")
}
