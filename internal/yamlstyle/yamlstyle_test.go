// SPDX-License-Identifier: Apache-2.0

package yamlstyle_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/yamlstyle"
)

// TestEncodeIndentsSequencesUnderTheirKey pins the one visible property of the house style,
// because it is the property that was inconsistent: a sequence is indented under its mapping
// key. Everything the operator writes looks like this, whether it was created whole or
// edited in place.
func TestEncodeIndentsSequencesUnderTheirKey(t *testing.T) {
	out, err := yamlstyle.Encode(map[string]any{
		"repositories": []any{
			map[string]any{"name": "a"},
			map[string]any{"name": "b"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "repositories:\n  - name: a\n  - name: b\n", string(out))
}

// TestEncodeIsDeterministic guards the property every diff-producing encoder needs: the same
// value renders the same bytes, so a commit that changed nothing produces no commit.
func TestEncodeIsDeterministic(t *testing.T) {
	value := map[string]any{"b": []any{2, 1}, "a": "x", "c": map[string]any{"k": true}}
	first, err := yamlstyle.Encode(value)
	require.NoError(t, err)
	for range 5 {
		again, err := yamlstyle.Encode(value)
		require.NoError(t, err)
		assert.Equal(t, string(first), string(again))
	}
}

// TestGitWritePathHasNoSecondEncoder is the test that keeps consumer ask #11 fixed.
//
// The bug was not an indentation choice, it was TWO ENCODERS: a create serialized through
// sigs.k8s.io/yaml and an update through gopkg.in/yaml.v3, so the first update after a create
// rewrote every sequence line in the file. Fixing the style without removing the second
// encoder would leave the defect one refactor away from returning, and it would return
// silently — both encoders produce valid YAML, so nothing fails, and the damage is only
// visible as a diff nobody can read.
//
// So: inside the packages that produce committed bytes, this package is the only place a YAML
// encoder is constructed. Parsing is unrestricted (a decoder imposes no style), and so is
// everything outside these packages — the analyzer's report and the in-memory kustomization
// copies it hands to kustomize are Go structs that must round-trip through their JSON tags,
// which is sigs.k8s.io/yaml's job and not a manifest style at all.
func TestGitWritePathHasNoSecondEncoder(t *testing.T) {
	writePathDirs := []string{
		filepath.Join("..", "sanitize"),
		filepath.Join("..", "git"),
	}

	var offenders []string
	for _, dir := range writePathDirs {
		require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			found, err := yamlEncodersIn(path)
			offenders = append(offenders, found...)
			return err
		}))
	}

	assert.Empty(t, offenders,
		"the Git write path must encode through internal/yamlstyle so a create and an "+
			"update cannot serialize differently; found: %v", offenders)
}

// yamlEncodersIn reports every call in one file that would define a competing YAML style:
// an encoder constructed, or a package-default Marshal. Decoding is unrestricted, because a
// decoder imposes no style.
func yamlEncodersIn(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	yamlAliases := yamlImportAliases(file)
	if len(yamlAliases) == 0 {
		return nil, nil
	}

	forbidden := map[string]string{
		"NewEncoder": "constructs a YAML encoder",
		"Marshal":    "serializes YAML with a package default",
	}

	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || !yamlAliases[pkg.Name] {
			return true
		}
		if why, bad := forbidden[sel.Sel.Name]; bad {
			out = append(out, path+": "+pkg.Name+"."+sel.Sel.Name+" "+why)
		}
		return true
	})
	return out, nil
}

// yamlImportAliases returns the local names a file uses for any YAML library.
func yamlImportAliases(file *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if !strings.Contains(path, "yaml") || strings.HasSuffix(path, "internal/yamlstyle") {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = true
	}
	return out
}

// TestEncodeReturnsErrorInsteadOfPanicking pins the seam's other job. yaml.v3 panics with a
// bare string on a type it cannot marshal, and that panic escapes the library's own recover,
// so without this the branch worker would crash rather than fail one write.
func TestEncodeReturnsErrorInsteadOfPanicking(t *testing.T) {
	_, err := yamlstyle.Encode(map[string]any{"bad": make(chan int)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "yaml encode failed")

	_, err = yamlstyle.NodeFor(map[string]any{"bad": func() {}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "yaml encode failed")
}
