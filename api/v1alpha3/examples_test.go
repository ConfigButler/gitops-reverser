// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
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

// TestExamplesDecodeStrictly folds every checked-in example of our own kinds through the REAL types
// with strict decoding, which is the only thing that catches a field the API no longer has.
//
// It exists because the fields the breaking wave removed are PRUNED rather than refused: an example
// still naming one applies cleanly, does nothing, and reports nothing, so a stale example is
// invisible both to a reader and to a cluster. The layout corpus already decodes the GitTargets it
// executes, but an example folder with no input/ is executed by nothing, and that is exactly where
// a `spec.commit.author` survived a rename.
//
// Every document is parsed before anything decides whether to skip it. A document this test cannot
// read, or one in our own group naming a version or kind we do not serve, is a failure rather than
// something quietly passed over as somebody else's schema: those are the shapes a stale example
// takes, so skipping them would leave the hole this test was written to close.
func TestExamplesDecodeStrictly(t *testing.T) {
	decoded := 0
	for _, root := range exampleRoots {
		require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
				return nil
			}
			decoded += decodeExampleFile(t, path)
			return nil
		}))
	}
	require.NotZero(t, decoded, "the example roots moved: this test decoded nothing")
}

// decodeExampleFile strict-decodes every document of our own group in one file and returns how many
// it decoded. It reads documents through a YAML reader rather than splitting on a "---" line, which
// is not the separator YAML actually defines: a document can open with one, and the sequence can
// appear inside a block scalar.
func decodeExampleFile(t *testing.T, path string) int {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	decoded := 0
	for i := 0; ; i++ {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return decoded
		}
		require.NoError(t, err, "%s: document %d could not be read", path, i)
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}

		var parsed any
		require.NoError(t, yaml.Unmarshal(doc, &parsed),
			"%s: document %d is not parseable YAML", path, i)

		// A document that is valid YAML but not a mapping (a bare list, a scalar) carries no
		// apiVersion and cannot be one of ours.
		fields, isMapping := parsed.(map[string]any)
		if !isMapping {
			continue
		}
		apiVersion, _ := fields["apiVersion"].(string)
		gv, err := schema.ParseGroupVersion(apiVersion)
		if err != nil || gv.Group != GroupVersion.Group {
			// Somebody else's schema, including the neighbouring examples.configbutler.ai and
			// manifestanalyzer.configbutler.ai groups, which are not this API.
			continue
		}

		require.Equal(t, GroupVersion.Version, gv.Version,
			"%s: document %d is %q; this group serves only %s, and an example on a version the "+
				"operator no longer installs cannot be applied", path, i, apiVersion, GroupVersion)

		kind, _ := fields["kind"].(string)
		obj := newExampleObject(kind)
		require.NotNil(t, obj, "%s: document %d declares kind %q, which %s does not serve",
			path, i, kind, GroupVersion)

		require.NoError(t, yaml.UnmarshalStrict(doc, obj),
			"%s: document %d names a field this API does not have; applied to a cluster it would "+
				"be pruned in silence", path, i)
		decoded++
	}
}

// newExampleObject returns an empty object of the named kind, or nil when this group does not serve
// it. Every root kind this API registers belongs here: a kind missing from the switch would make a
// valid example fail rather than be checked, which is why the list is asserted against the scheme
// by TestExampleKindsCoverTheScheme.
func newExampleObject(kind string) any {
	switch kind {
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

// TestExampleKindsCoverTheScheme pins newExampleObject to the scheme, so a kind added to this API
// group cannot quietly fall outside the example guard. Without it, adding a CRD and an example for
// it in the same change would leave that example unchecked: newExampleObject returns nil, and the
// document reads as somebody else's schema.
//
// List kinds are excluded. A manifest is a single object, never a List, so an example carrying one
// is not a case the guard has to decode.
func TestExampleKindsCoverTheScheme(t *testing.T) {
	s := runtime.NewScheme()
	require.NoError(t, AddToScheme(s))

	ourPackage := reflect.TypeOf(GitTarget{}).PkgPath()
	for gvk, goType := range s.AllKnownTypes() {
		if gvk.GroupVersion() != GroupVersion || strings.HasSuffix(gvk.Kind, "List") {
			continue
		}
		// Every scheme carries meta kinds (GetOptions, WatchEvent, ...) under each registered
		// group version. They are apimachinery's types, not ours, and no example declares one, so
		// identify them by the package they come from rather than by keeping a list of names.
		if goType.PkgPath() != ourPackage {
			continue
		}
		require.NotNil(t, newExampleObject(gvk.Kind),
			"%s is registered in the scheme but newExampleObject does not build one, so every "+
				"example of it is skipped by TestExamplesDecodeStrictly", gvk.Kind)
	}
}
