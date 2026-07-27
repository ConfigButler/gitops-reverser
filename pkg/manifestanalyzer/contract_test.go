// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/pkg/manifestanalyzer"
)

// The engine defined seventeen refusal kinds and this package re-declared fourteen. Since
// the projection copies the internal string through verbatim, the missing three could
// reach a consumer that had no exported constant to match on — a consumer doing the
// correct thing, matching on our published constants, silently failed to recognise them.
//
// This test makes the two counts unable to disagree again. It reads both sets out of the
// source, because a constant cannot be enumerated by reflection and a hand-kept list is
// exactly what drifted.
//
// See docs/design/analyzer-consumer-contract-asks.md (Ask 1).
func TestPublicIssueKindsCoverEveryEngineKind(t *testing.T) {
	t.Parallel()

	engine := issueKindConstants(t, "../../internal/manifestanalyzer")
	published := issueKindConstants(t, ".")

	require.GreaterOrEqual(t, len(engine), 15, "the source scan is broken, not the code")
	for _, kind := range engine {
		require.Contains(t, published, kind,
			"the engine can raise %q but this package publishes no constant for it, so a consumer "+
				"matching on our constants cannot recognise it", kind)
	}
	for _, kind := range published {
		if kind == string(manifestanalyzer.ReasonRefusedStructural) ||
			kind == string(manifestanalyzer.ReasonOverlayFanOutUnsupported) {
			continue // reason codes that are deliberately not issue kinds
		}
		require.Contains(t, engine, kind,
			"this package publishes %q but nothing can raise it", kind)
	}
}

// issueKindConstants returns the string value of every `X IssueKind = "..."` constant
// declared in the package at dir.
func issueKindConstants(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "read %s", dir)

	fset := token.NewFileSet()
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		require.NoError(t, err, "parse %s", name)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			out = append(out, issueKindsInDecl(gen)...)
		}
	}
	return out
}

func issueKindsInDecl(gen *ast.GenDecl) []string {
	var out []string
	for _, spec := range gen.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		if ident, ok := vs.Type.(*ast.Ident); !ok || ident.Name != "IssueKind" {
			continue
		}
		for _, value := range vs.Values {
			lit, ok := value.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			if s, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, s)
			}
		}
	}
	return out
}

// A refusal the consumer receives must say whether it can stop being one, and name who can
// clear it when it can. This is the whole ask, checked at the surface a consumer actually
// reads rather than at the raise site.
func TestScanFolder_RefusalsCarryPermanence(t *testing.T) {
	t.Parallel()

	report := manifestanalyzer.ScanFolderFS(t.Context(), refusedTree())
	require.False(t, report.Status.Accepted)
	require.NotEmpty(t, report.Status.Issues)

	for _, issue := range report.Status.Issues {
		require.NotEqual(t, manifestanalyzer.SolvabilityUnknown, issue.Solvability,
			"%s reached a consumer unclassified", issue.Kind)
		if issue.Solvability == manifestanalyzer.SolvabilityYes {
			require.NotEqual(t, manifestanalyzer.ActorUnknown, issue.Actor,
				"%s is fixable but does not say by whom", issue.Kind)
		} else {
			require.Equal(t, manifestanalyzer.ActorUnknown, issue.Actor,
				"%s names an actor for a refusal nobody can act on", issue.Kind)
		}
	}
}

// Version is never empty, so a consumer may read an ABSENT generator as "produced before
// this field shipped" rather than having to tell that apart from a build that did not know
// itself.
func TestVersion_IsNeverEmpty(t *testing.T) {
	t.Parallel()
	require.NotEmpty(t, manifestanalyzer.Version())
}

// refusedTree is a folder the operator refuses for two different reasons at once — a
// stray non-KRM document and a kustomization it cannot map back to source — so the test
// sees more than one classification path.
func refusedTree() fstest.MapFS {
	return fstest.MapFS{
		"kustomization.yaml": {Data: []byte(kustomizationHelmYAML)},
		"notes.txt":          {Data: []byte("scratch\n")},
		"values.yaml":        {Data: []byte("replicaCount: 2\n")},
	}
}
