// SPDX-License-Identifier: Apache-2.0

// Package corpus turns captured records into the browsable golden tree: one
// directory per scenario, one file per emitted moment, named so an ordered
// fan-out is self-describing (watch.deleted.cm-a.yaml, ...). A single recorded
// observation drives both lab layers; this package owns the corpus (golden YAML)
// layer. The corpus changes only when behavior changes, because the normalizer
// has already rewritten every volatile field.
package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/normalize"
)

const (
	// diffContextLines is the unified-diff context shown around a corpus drift.
	diffContextLines = 3
	corpusDirPerm    = 0o750
	corpusFilePerm   = 0o600
)

// Moment is one normalized record rendered to a single corpus file.
type Moment struct {
	Name    string
	Content []byte
}

// Build normalizes a scenario's records into one shared placeholder space and
// renders each to a deterministically named YAML moment. Records are expected in
// observation order; that order — and the per-base discriminator — make the
// fan-out and event sequencing self-describing on disk.
func Build(records []mutationlab.Record) ([]Moment, error) {
	payloads := make([]json.RawMessage, len(records))
	for i, r := range records {
		payloads[i] = r.Raw
	}
	normalized, err := normalize.Normalize(payloads)
	if err != nil {
		return nil, fmt.Errorf("normalize scenario: %w", err)
	}

	names := fileNames(records)
	moments := make([]Moment, len(records))
	for i := range records {
		y, err := yaml.Marshal(normalized[i])
		if err != nil {
			return nil, fmt.Errorf("marshal %s: %w", names[i], err)
		}
		moments[i] = Moment{Name: names[i], Content: y}
	}
	return moments, nil
}

// fileNames derives one filename per record: <source>.<verb-or-type>.yaml, with a
// discriminator appended only when several records share a base name (so a lone
// create is watch.added.yaml but a three-way deletecollection fan-out becomes
// watch.deleted.cm-a/cm-b/cm-c.yaml).
func fileNames(records []mutationlab.Record) []string {
	bases := make([]string, len(records))
	groups := map[string][]int{}
	for i, r := range records {
		bases[i] = baseName(r)
		groups[bases[i]] = append(groups[bases[i]], i)
	}

	names := make([]string, len(records))
	for base, idxs := range groups {
		if len(idxs) == 1 {
			names[idxs[0]] = base + ".yaml"
			continue
		}
		useName := distinctNames(records, idxs)
		for ordinal, i := range idxs {
			disc := strconv.Itoa(ordinal + 1)
			if useName {
				disc = records[i].Key.Name
			}
			names[i] = base + "." + sanitizeDisc(disc) + ".yaml"
		}
	}
	return names
}

func baseName(r mutationlab.Record) string {
	verb := strings.ToLower(r.Summary.Operation)
	if r.Source == mutationlab.SourceWatch {
		verb = strings.ToLower(r.Summary.WatchType)
	}
	if verb == "" {
		verb = "event"
	}
	return string(r.Source) + "." + verb
}

// distinctNames reports whether the records in a colliding group all carry a
// non-empty, unique Key.Name (so the name is a safe disambiguator).
func distinctNames(records []mutationlab.Record, idxs []int) bool {
	seen := map[string]bool{}
	for _, i := range idxs {
		name := records[i].Key.Name
		if name == "" || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}

func sanitizeDisc(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return '-'
		}
	}, s)
}

// Compare verifies the built moments match the committed files under dir. It
// returns a non-nil error describing the first divergence — a missing file, a
// content drift (with a unified diff), or a stray file — and writes nothing. A
// corpus diff in a PR is a signal, not noise: Kubernetes behavior changed, the
// capture changed, or the cluster version moved.
func Compare(dir string, moments []Moment) error {
	want := map[string]bool{}
	for _, m := range moments {
		want[m.Name] = true
		path := filepath.Join(dir, m.Name)
		have, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("missing corpus file %s (run with MUTATIONLAB_UPDATE=1 to create)", m.Name)
			}
			return fmt.Errorf("read %s: %w", m.Name, err)
		}
		if string(have) != string(m.Content) {
			diff, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
				A:        difflib.SplitLines(string(have)),
				B:        difflib.SplitLines(string(m.Content)),
				FromFile: "committed/" + m.Name,
				ToFile:   "captured/" + m.Name,
				Context:  diffContextLines,
			})
			return fmt.Errorf("corpus drift in %s:\n%s", m.Name, diff)
		}
	}
	stray, err := strayFiles(dir, want)
	if err != nil {
		return err
	}
	if len(stray) > 0 {
		return fmt.Errorf("stray corpus files no longer captured: %s", strings.Join(stray, ", "))
	}
	return nil
}

// Write rewrites dir to exactly the built moments (creating dir and removing
// stale .yaml files), the update path behind MUTATIONLAB_UPDATE=1.
func Write(dir string, moments []Moment) error {
	if err := os.MkdirAll(dir, corpusDirPerm); err != nil {
		return err
	}
	want := map[string]bool{}
	for _, m := range moments {
		want[m.Name] = true
		if err := os.WriteFile(filepath.Join(dir, m.Name), m.Content, corpusFilePerm); err != nil {
			return fmt.Errorf("write %s: %w", m.Name, err)
		}
	}
	stray, err := strayFiles(dir, want)
	if err != nil {
		return err
	}
	for _, name := range stray {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("remove stale %s: %w", name, err)
		}
	}
	return nil
}

// strayFiles lists *.yaml files in dir not present in want, sorted.
func strayFiles(dir string, want map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var stray []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		if !want[name] {
			stray = append(stray, name)
		}
	}
	sort.Strings(stray)
	return stray, nil
}
