// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// line returns a doc comment text of exactly n characters.
func line(n int) string {
	return strings.Repeat("x", n)
}

func TestCheckFile_MeasuresTypeAndFieldDocComments(t *testing.T) {
	src := `package v1

// Spec is a type doc comment that is too wide: ` + line(60) + `
type Spec struct {
	// Field is a field doc comment that is too wide: ` + line(60) + `
	Field string
}
`
	findings, err := checkFile("types.go", []byte(src), defaultWidth)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("want 2 findings, got %d: %+v", len(findings), findings)
	}
	if findings[0].pos.Line != 3 || findings[1].pos.Line != 5 {
		t.Errorf("want findings on lines 3 and 5, got %d and %d", findings[0].pos.Line, findings[1].pos.Line)
	}
}

func TestCheckFile_LimitIsInclusive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		width int
		want  int
	}{
		{name: "exactly the limit", width: defaultWidth, want: 0},
		{name: "one over", width: defaultWidth + 1, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "package v1\n\ntype Spec struct {\n\t// " + line(tc.width) + "\n\tField string\n}\n"
			findings, err := checkFile("types.go", []byte(src), defaultWidth)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != tc.want {
				t.Errorf("want %d findings, got %d", tc.want, len(findings))
			}
		})
	}
}

func TestCheckFile_CountsCharactersNotBytes(t *testing.T) {
	// Each en dash is three bytes and one character, as kubectl explain counts it.
	src := "package v1\n\ntype Spec struct {\n\t// " + strings.Repeat("–", defaultWidth) + "\n\tField string\n}\n"
	findings, err := checkFile("types.go", []byte(src), defaultWidth)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Errorf("want no findings for %d characters, got %+v", defaultWidth, findings)
	}
}

func TestCheckFile_SkipsTextThatNeverReachesTheCRD(t *testing.T) {
	src := `package v1

// A rationale block, separated from the doc comment by a blank line: ` + line(60) + `

// Spec is short.
// +kubebuilder:validation:XValidation:rule="` + line(80) + `"
type Spec struct {
	// A field rationale block: ` + line(80) + `

	// Field is short.
	// +kubebuilder:validation:Pattern=` + "`" + line(80) + "`" + `
	Field string
}

// Helper is a function, whose doc comment is not a description: ` + line(60) + `
func Helper() {}

// Constant is not a description either: ` + line(60) + `
const Constant = 1
`
	findings, err := checkFile("types.go", []byte(src), defaultWidth)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Errorf("want no findings, got %+v", findings)
	}
}

func TestCheckFile_ReadsAGroupedTypeSpecsOwnDoc(t *testing.T) {
	src := `package v1

// The group's doc is not any one type's: ` + line(60) + `
type (
	// Mode is too wide: ` + line(70) + `
	Mode string
)
`
	findings, err := checkFile("types.go", []byte(src), defaultWidth)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].pos.Line != 5 {
		t.Errorf("want one finding on line 5, got %+v", findings)
	}
}

func TestIsAPISource(t *testing.T) {
	for path, want := range map[string]bool{
		"api/v1alpha3/gittarget_types.go":       true,
		"api/v1alpha3/gittarget_types_test.go":  false,
		"api/v1alpha3/zz_generated.deepcopy.go": false,
		"api/v1alpha3/README.md":                false,
	} {
		if got := isAPISource(path); got != want {
			t.Errorf("isAPISource(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestCheckFile_ReportsAParseError(t *testing.T) {
	if _, err := checkFile("broken.go", []byte("package v1\n\ntype {"), defaultWidth); err == nil {
		t.Error("want a parse error, got nil")
	}
}
