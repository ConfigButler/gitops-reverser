// SPDX-License-Identifier: Apache-2.0

// Command crdfields renders the settings index in docs/configuration.md from the
// CRD schemas in config/crd/bases, so the table cannot drift from the API.
//
// The index is the one place a reader looks to answer "what can I set, and what
// happens if I don't". Hand-maintaining it went wrong three times in the commit
// that introduced it: spec.accessFrom omitted DENIES every namespace rather than
// admitting them, an omitted spec.serializeNamespace INFERS per document rather
// than meaning false, and qps/burst fall back to operator-wide flags and are
// ignored without a kubeConfig. Each was written from memory of the prose and
// each was wrong in the direction that costs a reader an outage.
//
// So the two halves are split by who can be trusted with them:
//
//   - The SCHEMA owns whether a field is required and what it defaults to. Those
//     come from config/crd/bases, which controller-gen regenerates from the Go
//     types, so they cannot be stale without `task manifests` being stale too.
//
//   - fields.yaml owns the one-line description and the link to the section that
//     explains the field. Prose is not derivable: a CRD description is written for
//     `kubectl explain` and runs to paragraphs, which is the wrong shape for a
//     table and the wrong voice for a guide.
//
// What makes it a gate rather than a convenience is the coverage check. Adding a
// field to the API and not describing it fails the build, and so does describing
// one that no longer exists. The tool cannot write the sentence for you; it can
// refuse to let the table pretend the field is not there.
//
// Depth is decided by fields.yaml, not by a fixed limit. A path listed there is a
// leaf: the walk stops and its children are not required. A top-level property
// with no entry of its own must have every one of its children described, or the
// run fails naming it. That is what keeps `spec.commit.window` in the table while
// `spec.commit.message.liveTemplate` stays in its own document.
//
// Usage:
//
//	go run ./hack/crdfields           # rewrite the generated block
//	go run ./hack/crdfields -check    # fail if the block is out of date
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	crdDir     = "config/crd/bases"
	fieldsFile = "hack/crdfields/fields.yaml"
	targetFile = "docs/configuration.md"

	beginMarker = "<!-- BEGIN GENERATED: settings-index (task settings-index) -->"
	endMarker   = "<!-- END GENERATED: settings-index -->"
)

// fieldDoc is one row of the index: the prose half, which no schema can supply.
type fieldDoc struct {
	// Path is dotted, relative to spec: "commit.window". For an array field it
	// names the item's property: "rules.resources" renders as `rules[].resources`.
	Path string `json:"path"`
	// Doc is the description cell, rendered as markdown so it can carry links.
	Doc string `json:"doc"`
	// Default describes the behaviour of omitting a field the SCHEMA gives no
	// default for ("anonymous access"). Setting it for a field that has a schema
	// default is an error: the schema would be the better answer and the two
	// could disagree.
	Default string `json:"default,omitempty"`
	// Skip keeps a field out of the table. The reason is required and is there
	// for the next reader, who will want to know why the field is missing.
	Skip string `json:"skip,omitempty"`
}

type kindDoc struct {
	Kind   string     `json:"kind"`
	Title  string     `json:"title"`
	Intro  string     `json:"intro,omitempty"`
	Outro  string     `json:"outro,omitempty"`
	Fields []fieldDoc `json:"fields"`
}

type indexDoc struct {
	Preamble string    `json:"preamble"`
	Kinds    []kindDoc `json:"kinds"`
}

// schema is the slice of an OpenAPI v3 schema this tool reads.
type schema struct {
	Type       string             `json:"type"`
	Default    *json.RawMessage   `json:"default"`
	Required   []string           `json:"required"`
	Properties map[string]*schema `json:"properties"`
	Items      *schema            `json:"items"`
}

// children returns the properties a path may descend into, treating an array as
// its item type so that rules[].resources is reachable from rules.
func (s *schema) children() map[string]*schema {
	if s == nil {
		return nil
	}
	if s.Type == "array" && s.Items != nil {
		return s.Items.Properties
	}
	return s.Properties
}

func (s *schema) requires(name string) bool {
	target := s
	if s.Type == "array" && s.Items != nil {
		target = s.Items
	}
	for _, r := range target.Required {
		if r == name {
			return true
		}
	}
	return false
}

func main() {
	check := flag.Bool("check", false, "fail if the generated block is out of date instead of rewriting it")
	flag.Parse()

	if err := run(*check); err != nil {
		fmt.Fprintf(os.Stderr, "crdfields: %v\n", err)
		os.Exit(1)
	}
}

func run(check bool) error {
	index, err := loadIndexDoc(fieldsFile)
	if err != nil {
		return err
	}
	schemas, err := loadSchemas(crdDir)
	if err != nil {
		return err
	}
	rendered, err := render(index, schemas)
	if err != nil {
		return err
	}
	return writeBlock(targetFile, rendered, check)
}

func loadIndexDoc(path string) (*indexDoc, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- a fixed path in this repository
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc indexDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(doc.Kinds) == 0 {
		return nil, fmt.Errorf("%s describes no kinds", path)
	}
	if err := validateIndexDoc(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &doc, nil
}

// validateIndexDoc catches the two ways an entry goes wrong before the schema is
// ever consulted, so the error names the cause rather than a missing table cell.
func validateIndexDoc(doc *indexDoc) error {
	var problems []error
	for _, kd := range doc.Kinds {
		for _, f := range kd.Fields {
			switch {
			case f.Path == "":
				problems = append(problems, fmt.Errorf("%s: an entry has no `path:`", kd.Kind))
			case f.Skip == "" && f.Doc == "":
				problems = append(problems, fmt.Errorf(
					"%s.%s: needs a `doc:`, or a `skip:` saying why it is left out", kd.Kind, f.Path))
			case isBareYAMLBool(f.Default):
				// YAML reads a bare off/on/yes/no as a boolean, so `default: off`
				// reaches this table as "false". It is silent, and it produced a
				// wrong cell the first time this file was written.
				problems = append(problems, fmt.Errorf(
					"%s.%s: `default: %s` was read as a YAML boolean; quote it",
					kd.Kind, f.Path, f.Default))
			}
		}
	}
	return errors.Join(problems...)
}

// isBareYAMLBool reports a default that YAML coerced out of off/on/yes/no.
// A genuine boolean default belongs in the schema, so it never needs spelling here.
func isBareYAMLBool(value string) bool {
	return value == "true" || value == "false"
}

// loadSchemas reads every CRD in dir and returns each kind's stored-version spec schema.
func loadSchemas(dir string) (map[string]*schema, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("glob %s: %w", dir, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no CRDs found in %s", dir)
	}

	out := make(map[string]*schema, len(entries))
	for _, path := range entries {
		kind, spec, err := loadCRD(path)
		if err != nil {
			return nil, err
		}
		out[kind] = spec
	}
	return out, nil
}

func loadCRD(path string) (string, *schema, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- paths come from a glob of a fixed directory
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", path, err)
	}

	var crd struct {
		Spec struct {
			Names struct {
				Kind string `json:"kind"`
			} `json:"names"`
			Versions []struct {
				Storage bool `json:"storage"`
				Schema  struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec *schema `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		return "", nil, fmt.Errorf("parse %s: %w", path, err)
	}

	for _, v := range crd.Spec.Versions {
		if v.Storage {
			if v.Schema.OpenAPIV3Schema.Properties.Spec == nil {
				return "", nil, fmt.Errorf("%s: stored version has no spec schema", path)
			}
			return crd.Spec.Names.Kind, v.Schema.OpenAPIV3Schema.Properties.Spec, nil
		}
	}
	return "", nil, fmt.Errorf("%s: no stored version", path)
}

func render(index *indexDoc, schemas map[string]*schema) (string, error) {
	var buf bytes.Buffer
	buf.WriteString(strings.TrimRight(index.Preamble, "\n") + "\n\n")

	var problems []error
	for _, kd := range index.Kinds {
		spec, ok := schemas[kd.Kind]
		if !ok {
			problems = append(problems, fmt.Errorf("%s: no CRD defines this kind", kd.Kind))
			continue
		}
		if err := checkCoverage(kd, spec); err != nil {
			problems = append(problems, err)
		}
		section, err := renderKind(kd, spec)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		buf.WriteString(section)
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("%s is out of step with the CRDs:\n  %w",
			fieldsFile, errors.Join(problems...))
	}
	return buf.String(), nil
}

// checkCoverage is the half that makes this a gate. A top-level property is
// covered when it is described itself, skipped, or has every one of its children
// described or skipped. Anything else is a field the guide does not mention.
func checkCoverage(kd kindDoc, spec *schema) error {
	described := make(map[string]bool, len(kd.Fields))
	for _, f := range kd.Fields {
		described[f.Path] = true
	}

	var missing []string
	for name, child := range spec.Properties {
		if described[name] {
			continue
		}
		kids := child.children()
		if len(kids) == 0 {
			missing = append(missing, name)
			continue
		}
		anyDescribed := false
		var undescribed []string
		for kid := range kids {
			if described[name+"."+kid] {
				anyDescribed = true
			} else {
				undescribed = append(undescribed, name+"."+kid)
			}
		}
		if !anyDescribed {
			missing = append(missing, name)
			continue
		}
		missing = append(missing, undescribed...)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf(
		"%s: %d field(s) in the CRD that %s does not describe: %s\n"+
			"      add an entry with a `doc:`, or a `skip:` saying why it is left out",
		kd.Kind, len(missing), fieldsFile, strings.Join(missing, ", "))
}

func renderKind(kd kindDoc, spec *schema) (string, error) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "### %s\n\n", kd.Title)
	if kd.Intro != "" {
		fmt.Fprintf(&buf, "%s\n\n", kd.Intro)
	}
	buf.WriteString("| Field | Default | What it does |\n|---|---|---|\n")

	var problems []error
	for _, f := range kd.Fields {
		if f.Skip != "" {
			continue
		}
		row, err := renderRow(kd.Kind, f, spec)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		buf.WriteString(row)
	}
	if len(problems) > 0 {
		return "", errors.Join(problems...)
	}
	if kd.Outro != "" {
		fmt.Fprintf(&buf, "\n%s\n", kd.Outro)
	}
	buf.WriteString("\n")
	return buf.String(), nil
}

func renderRow(kind string, f fieldDoc, spec *schema) (string, error) {
	node, required, err := resolve(spec, f.Path)
	if err != nil {
		return "", fmt.Errorf("%s.%s: %w", kind, f.Path, err)
	}

	schemaDefault := ""
	if node.Default != nil {
		schemaDefault = formatDefault(*node.Default)
	}
	if schemaDefault != "" && f.Default != "" {
		return "", fmt.Errorf(
			"%s.%s: the schema already defaults this to %s, so remove the `default:` from %s",
			kind, f.Path, schemaDefault, fieldsFile)
	}

	cell := "none"
	switch {
	case required:
		cell = "**required**"
	case schemaDefault != "":
		cell = schemaDefault
	case f.Default != "":
		cell = f.Default
	}
	return fmt.Sprintf("| `%s` | %s | %s |\n", displayPath(f.Path, spec), cell, f.Doc), nil
}

// resolve walks a dotted path and reports whether the last segment is required
// by its immediate parent.
func resolve(spec *schema, path string) (*schema, bool, error) {
	node, required := spec, false
	for _, seg := range strings.Split(path, ".") {
		kids := node.children()
		next, ok := kids[seg]
		if !ok {
			return nil, false, errors.New("no such field in the CRD schema")
		}
		required = node.requires(seg)
		node = next
	}
	return node, required, nil
}

// displayPath spells an array field the way a manifest does, so a reader can
// match the row to the YAML they are writing: rules[].resources, not rules.resources.
func displayPath(path string, spec *schema) string {
	segs := strings.Split(path, ".")
	node := spec
	out := make([]string, 0, len(segs))
	for i, seg := range segs {
		next := node.children()[seg]
		name := seg
		if next != nil && next.Type == "array" && i < len(segs)-1 {
			name += "[]"
		}
		out = append(out, name)
		node = next
	}
	return strings.Join(out, ".")
}

func formatDefault(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if asString == "" {
			return ""
		}
		return "`" + asString + "`"
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return "`" + trimmed + "`"
	}
	return "`" + compact.String() + "`"
}

// writeBlock replaces the marked region of path, or reports what a run would change.
func writeBlock(path, body string, check bool) error {
	raw, err := os.ReadFile(path) // #nosec G304 -- a fixed path in this repository
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	content := string(raw)

	start := strings.Index(content, beginMarker)
	end := strings.Index(content, endMarker)
	if start < 0 || end < 0 || end < start {
		return fmt.Errorf("%s: cannot find the generated block; it must contain\n  %s\n  %s",
			path, beginMarker, endMarker)
	}

	updated := content[:start] + beginMarker + "\n\n" + body + content[end:]
	if updated == content {
		return nil
	}
	if check {
		return fmt.Errorf(
			"%s is out of date with the CRDs. Run `task settings-index` and commit the result", path)
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("crdfields: updated the settings index in %s\n", path)
	return nil
}
