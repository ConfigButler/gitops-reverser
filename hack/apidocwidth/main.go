// SPDX-License-Identifier: Apache-2.0

// Command apidocwidth checks that every line of an API doc comment fits in 76
// characters after the "// " prefix.
//
// A type or field doc comment under api/ becomes a CRD description, which is the
// text an operator reads in `kubectl explain`. kubectl keeps the comment's line
// breaks and wraps each line again at 76 characters of text, so a longer line leaves
// its last words on a line of their own. The measurements are in
// docs/facts/crd-descriptions-in-kubectl-explain.md.
//
// It reads the Go source rather than config/crd/bases. The CRDs also carry the
// descriptions controller-gen copies from upstream types (metav1.Condition,
// LabelSelector, Flux's object references, apiVersion and kind), which this
// repository cannot rewrap: about 150 of their lines are wider than 76. The source
// holds exactly the descriptions written here, and a finding in it names the file and
// line to fix rather than a schema path.
//
// Only the doc comments of type declarations and struct fields are measured, because
// those are the only comments controller-gen turns into descriptions. Marker lines
// (`+kubebuilder:...`, `+optional`) never reach the CRD, so they are skipped. A
// rationale block needs no special case: it is separated from the doc comment by a
// blank line, so Go does not attach it as the doc comment at all.
//
// Usage:
//
//	apidocwidth [-width N] [dir ...]
//
// The directories default to api. Exits non-zero and prints file:line for every line
// that is too wide.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	defaultWidth = 76
	exitFindings = 1
	exitUsage    = 2
)

// finding is one doc comment line wider than the limit.
type finding struct {
	pos   token.Position
	width int
	text  string
}

func main() {
	width := flag.Int("width", defaultWidth, "maximum characters per doc comment line, after the \"// \" prefix")
	flag.Parse()
	dirs := flag.Args()
	if len(dirs) == 0 {
		dirs = []string{"api"}
	}

	var findings []finding
	files := 0
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !isAPISource(path) {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			found, err := checkFile(path, src, *width)
			if err != nil {
				return err
			}
			files++
			findings = append(findings, found...)
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "apidocwidth: %v\n", err)
			os.Exit(exitUsage)
		}
	}

	for _, f := range findings {
		fmt.Printf("%s:%d: %d characters, over %d: %s\n", f.pos.Filename, f.pos.Line, f.width, *width, f.text)
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "apidocwidth: %d doc comment lines are wider than %d characters; "+
			"kubectl explain prints their last words on a line of their own\n", len(findings), *width)
		os.Exit(exitFindings)
	}
	fmt.Printf("apidocwidth: %d files, every API doc comment line fits in %d characters\n", files, *width)
}

// isAPISource reports whether path is hand-written Go that controller-gen reads: not a
// test, and not generated deepcopy code.
func isAPISource(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, ".go") &&
		!strings.HasSuffix(base, "_test.go") &&
		!strings.HasPrefix(base, "zz_generated")
}

// checkFile returns every type and struct-field doc comment line in src wider than width.
func checkFile(filename string, src []byte, width int) ([]finding, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var findings []finding
	for _, doc := range descriptionDocs(file) {
		if doc == nil {
			continue
		}
		for _, c := range doc.List {
			text, ok := docText(c.Text)
			if !ok {
				continue
			}
			if n := utf8.RuneCountInString(text); n > width {
				findings = append(findings, finding{pos: fset.Position(c.Pos()), width: n, text: text})
			}
		}
	}
	return findings, nil
}

// descriptionDocs returns the doc comments controller-gen can turn into descriptions: those
// of type declarations and of struct fields.
func descriptionDocs(file *ast.File) []*ast.CommentGroup {
	var docs []*ast.CommentGroup
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			// A lone `type X struct` carries its doc on the declaration, a grouped one on the
			// spec; controller-gen reads whichever is set, and so does this.
			doc := typeSpec.Doc
			if doc == nil {
				doc = gen.Doc
			}
			docs = append(docs, doc)
			if structType, ok := typeSpec.Type.(*ast.StructType); ok {
				for _, field := range structType.Fields.List {
					docs = append(docs, field.Doc)
				}
			}
		}
	}
	return docs
}

// docText is the description text of one comment line, and false for a line that never
// reaches a description: a marker, or a /* */ comment.
func docText(comment string) (string, bool) {
	text, ok := strings.CutPrefix(comment, "//")
	if !ok {
		return "", false
	}
	text = strings.TrimPrefix(text, " ")
	if strings.HasPrefix(text, "+") {
		return "", false
	}
	return text, true
}
