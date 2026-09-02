// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// exampleRoots are the folders whose manifests a reader is invited to copy: the samples, the
// worked examples in the layout corpus, the playground, and the e2e setup fixtures that are checked
// in as YAML rather than rendered from a template.
var exampleRoots = []string{
	"../../config/samples",
	"../../test/fixtures/layout-corpus",
	"../../test/playground",
	"../../test/e2e/setup",
}

// TestExamplesDecodeStrictly folds every checked-in example of our own kinds through the REAL
// types with strict decoding, which is the only thing that catches a field the API no longer has.
//
// It exists because the fields this release removed are PRUNED rather than refused: an example
// still naming one applies cleanly, does nothing, and reports nothing — so a stale example is
// invisible both to a reader and to a cluster. The layout corpus already decodes the GitTargets it
// executes, but an example folder with no input/ (the prerequisites GitProvider) is executed by
// nothing, and that is exactly where a `spec.commit.author` survived a rename.
func TestExamplesDecodeStrictly(t *testing.T) {
	decoded := 0
	for _, root := range exampleRoots {
		require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || filepath.Ext(path) != ".yaml" {
				return err
			}
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			for i, doc := range strings.Split(string(raw), "\n---\n") {
				obj := exampleTargetFor(doc)
				if obj == nil {
					continue
				}
				require.NoError(t, yaml.UnmarshalStrict([]byte(doc), obj),
					"%s document %d names a field this API does not have; applied to a cluster it "+
						"would be pruned in silence", path, i)
				decoded++
			}
			return nil
		}))
	}
	require.NotZero(t, decoded, "the example roots moved: this test decoded nothing")
}

// exampleTargetFor returns an empty object of the kind a document declares, or nil when the
// document is not one of ours. Anything outside configbutler.ai/v1alpha3 is somebody else's
// schema and is not this test's business.
func exampleTargetFor(doc string) any {
	if !strings.Contains(doc, GroupVersion.String()) {
		return nil
	}
	var probe struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	if err := yaml.Unmarshal([]byte(doc), &probe); err != nil || probe.APIVersion != GroupVersion.String() {
		return nil
	}
	switch probe.Kind {
	case "GitProvider":
		return &GitProvider{}
	case "GitTarget":
		return &GitTarget{}
	case "ClusterProvider":
		return &ClusterProvider{}
	case "WatchRule":
		return &WatchRule{}
	case "ClusterWatchRule":
		return &ClusterWatchRule{}
	case "CommitRequest":
		return &CommitRequest{}
	}
	return nil
}
