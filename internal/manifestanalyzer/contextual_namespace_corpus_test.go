// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// wantDoc is one expected document outcome in a contextual-namespace example folder:
// the effective identity the store should index it under, and where its namespace
// came from. An empty namespace with NamespaceNone is the refused/unsupplied case.
type wantDoc struct {
	namespace string
	name      string
	source    NamespaceSourceKind
}

// TestContextualNamespaceCorpus drives the supported/unsupported example folders under
// testdata/contextual-namespace. Each folder is built as a GitTarget subtree and the
// per-document namespace provenance is asserted, so the supported boundary is pinned by
// real layouts rather than prose. See
// docs/design/manifest/contextual-namespace-and-kustomize-folder-editing.md.
func TestContextualNamespaceCorpus(t *testing.T) {
	cases := []struct {
		dir           string
		docs          []wantDoc
		ambiguousDiag bool
	}{
		{
			dir: "supported/flat-namespace",
			docs: []wantDoc{
				{namespace: "app", name: "a", source: NamespaceKustomize},
				{namespace: "app", name: "b", source: NamespaceKustomize},
			},
		},
		{
			dir: "supported/nested-base",
			docs: []wantDoc{
				{namespace: "app", name: "root", source: NamespaceKustomize},
				{namespace: "app", name: "child", source: NamespaceKustomize},
			},
		},
		{
			dir: "supported/multi-doc",
			docs: []wantDoc{
				{namespace: "app", name: "one", source: NamespaceKustomize},
				{namespace: "app", name: "two", source: NamespaceKustomize},
			},
		},
		{
			dir:  "supported/explicit-namespace",
			docs: []wantDoc{{namespace: "explicit-ns", name: "cm", source: NamespaceExplicit}},
		},
		{
			dir:           "unsupported/ambiguous-two-roots",
			docs:          []wantDoc{{name: "shared", source: NamespaceNone}},
			ambiguousDiag: true,
		},
		{dir: "unsupported/patches", docs: []wantDoc{{name: "cm", source: NamespaceNone}}},
		{dir: "unsupported/generators", docs: []wantDoc{{name: "cm", source: NamespaceNone}}},
		{dir: "unsupported/components", docs: []wantDoc{{name: "cm", source: NamespaceNone}}},
		{dir: "unsupported/helm", docs: []wantDoc{{name: "cm", source: NamespaceNone}}},
		{dir: "unsupported/remote-base", docs: []wantDoc{{name: "cm", source: NamespaceNone}}},
		{dir: "unsupported/name-prefix", docs: []wantDoc{{name: "cm", source: NamespaceNone}}},
		{dir: "unsupported/no-context", docs: []wantDoc{{name: "cm", source: NamespaceNone}}},
	}

	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			mapper := typeset.NewSnapshotRegistry(sampleClusterSnapshot())
			fsys := os.DirFS(filepath.Join("testdata", "contextual-namespace", tc.dir))
			store := BuildStore(context.Background(), fsys, mapper)

			for _, want := range tc.docs {
				id := manifestedit.Identity{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Namespace:  want.namespace,
					Name:       want.name,
				}
				dm := store.ByManifestIdentity[id]
				if dm == nil {
					t.Fatalf("%s: ConfigMap %q should be indexed under namespace %q", tc.dir, want.name, want.namespace)
				}
				if dm.NamespaceSource.Kind != want.source {
					t.Errorf("%s: ConfigMap %q NamespaceSource.Kind = %q, want %q",
						tc.dir, want.name, dm.NamespaceSource.Kind, want.source)
				}
				if (dm.NamespaceSource.Kind == NamespaceKustomize) != dm.NamespaceInheritedFromContext() {
					t.Errorf("%s: ConfigMap %q NamespaceInheritedFromContext disagrees with Kind %q",
						tc.dir, want.name, dm.NamespaceSource.Kind)
				}
			}

			if got := hasAmbiguousNamespaceDiag(store); got != tc.ambiguousDiag {
				t.Errorf("%s: ambiguous-namespace diagnostic present = %v, want %v", tc.dir, got, tc.ambiguousDiag)
			}
		})
	}
}

func hasAmbiguousNamespaceDiag(store *ManifestStore) bool {
	for _, d := range store.Diagnostics {
		if d.Reason == reasonAmbiguousNamespace {
			return true
		}
	}
	return false
}
