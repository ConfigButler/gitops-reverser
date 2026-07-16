// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"bytes"
	"path"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// This file reads ONE path-valued field of a document we already parse — an Argo CD
// Application's helm.valueFiles or a Flux HelmRelease's spec.chart.spec.valuesFiles — and turns
// the file it names into read-only context, so the analyzer stops refusing a folder for holding
// a values file the repository itself points at. It is
// docs/design/support-boundary/values-file-projection.md §2 "Move 1" and
// docs/design/support-boundary/acceptance-precision.md §1c: a values file named by a release is
// understood, never written, and never a refusal that takes the folder down with it.
//
// The boundary this must not move: we read a PATH-valued field and nothing else. We never
// render a chart, never learn what a value means, and never reach into inline values
// (helm.valuesObject / helm.values / HelmRelease spec.values) or an object reference
// (Flux valuesFrom) — those are not files, and only the path-valued spelling is this document's
// subject. The tolerated shape mirrors the kustomize patch precedent exactly: a file named by a
// directive is retained as build context, the render is mirrored, and nothing is routed INTO it.

// The closed vocabulary of release kinds that name a values FILE in a path-valued field. Nothing
// else is honoured — the claim comes from a group + kind we recognise, never from a filename.
const (
	// argoAppGroup owns Application.spec.source(s).helm.valueFiles.
	argoAppGroup = "argoproj.io"
	// fluxHelmGroup owns HelmRelease.spec.chart.spec.valuesFiles.
	fluxHelmGroup = "helm.toolkit.fluxcd.io"
)

// helmSource is the one part of an Argo source this file cares about: its helm.valueFiles. A
// source carries much more, but a values FILE is named only here, so nothing else is decoded.
type helmSource struct {
	Helm *struct {
		ValueFiles []string `yaml:"valueFiles"`
	} `yaml:"helm"`
}

// releaseDoc is the minimal decode of the two release kinds that name a values file by path: an
// Argo CD Application (spec.source and spec.sources[].helm.valueFiles) and a Flux HelmRelease
// (spec.chart.spec.valuesFiles). Both spec shapes live on one struct; the fields the doc's kind
// does not use simply stay nil. Every other field is ignored on purpose.
type releaseDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Spec       struct {
		// Argo CD Application: single source and multi-source spellings.
		Source  *helmSource  `yaml:"source"`
		Sources []helmSource `yaml:"sources"`
		// Flux HelmRelease: the chart's own valuesFiles list.
		Chart *struct {
			Spec struct {
				ValuesFiles []string `yaml:"valuesFiles"`
			} `yaml:"spec"`
		} `yaml:"chart"`
	} `yaml:"spec"`
}

// valueFileEntries returns every values-file path the document names, dispatched by its kind: an
// Argo Application's helm.valueFiles (across its single and multiple sources) or a Flux
// HelmRelease's spec.chart.spec.valuesFiles. A document that is neither names none.
func (d releaseDoc) valueFileEntries() []string {
	group := groupOf(d.APIVersion)
	switch {
	case group == argoAppGroup && d.Kind == "Application":
		var out []string
		if d.Spec.Source != nil && d.Spec.Source.Helm != nil {
			out = append(out, d.Spec.Source.Helm.ValueFiles...)
		}
		for _, s := range d.Spec.Sources {
			if s.Helm != nil {
				out = append(out, s.Helm.ValueFiles...)
			}
		}
		return out
	case group == fluxHelmGroup && d.Kind == "HelmRelease":
		if d.Spec.Chart != nil {
			return d.Spec.Chart.Spec.ValuesFiles
		}
	}
	return nil
}

// groupOf returns the API group of an apiVersion ("group/version" → "group"; a core "v1" → "").
func groupOf(apiVersion string) string {
	if i := strings.IndexByte(apiVersion, '/'); i >= 0 {
		return apiVersion[:i]
	}
	return apiVersion
}

// helmValueFileRefs is the set of scanned files (slash paths) that a release document in the scan
// names through a path-valued values-file field AND that are not themselves Kubernetes manifests
// — the read-only-context set. A referenced file that IS valid KRM is left to the normal rules
// (it is a manifest, mirrored as one); only a non-KRM values file is rescued here, which is
// exactly the file that would otherwise be refused as non-krm-yaml.
//
// It mirrors patchFilesOf: parse the directive, resolve the path it names against the scan, and
// hand the store a set it retains outside the managed model. Fan-in is not gated here — a values
// file shared by several releases is still read-only context (never refused for being shared);
// the fan-in = 1 gate belongs to the editable-projection step (Move 2), not to making a file we
// understand stop refusing its folder.
func helmValueFileRefs(files []manifestedit.FileContent) map[string]struct{} {
	tree := contentByPath(files)
	refs := map[string]struct{}{}
	for _, f := range files {
		dir := slashDir(f.Path)
		for _, doc := range decodeReleases(f.Content) {
			for _, entry := range doc.valueFileEntries() {
				if p := resolveValueFilePath(entry, dir, tree); p != "" {
					refs[p] = struct{}{}
				}
			}
		}
	}
	return refs
}

// decodeReleases returns every release document (Argo Application or Flux HelmRelease) in one
// file's bytes. A file may hold several documents (or none that are releases); each is decoded
// independently and a document this minimal struct cannot read contributes nothing.
func decodeReleases(content []byte) []releaseDoc {
	var out []releaseDoc
	dec := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var doc releaseDoc
		if err := dec.Decode(&doc); err != nil {
			break // EOF, or a document this minimal struct cannot read — neither is our concern
		}
		out = append(out, doc)
	}
	return out
}

// resolveValueFilePath resolves one values-file entry to the scanned file it names, or "" when it
// names nothing we hold as a non-KRM values file. dir is the referencing release's own directory
// (slash, relative to the scanned root).
//
// The entry can arrive in three spellings, and the ordered candidates below resolve all of them
// against both a whole-repo scan and a subtree (the live operator's GitTarget path) scan:
//
//   - a $ref-prefixed Argo entry ($values/platform/cert-manager/values.yaml) is repo-root-relative
//     — the $values ref names a source whose repoURL is this same repo. The leading $ref/ token is
//     stripped, leaving the repo-root path.
//   - a Flux valuesFiles entry is a path relative to its SourceRef (the repo), so it too is
//     effectively repo-root-relative.
//   - a plain relative entry (values.yaml, ../shared/values.yaml) is resolved against the release's
//     own directory.
//
// Candidates, first non-KRM match wins: relative to the release's directory (a co-located relative
// path), the path as a scan-root-relative path (a whole-repo scan of a repo-root-relative
// reference), and finally co-located by basename (a subtree scan that has lost the repo-root prefix
// but holds the file beside the release — the "referenced values file in the same folder" case).
// Only a path that exists AND is non-KRM is returned, so a genuine manifest a release happens to
// reference is never silently un-managed.
func resolveValueFilePath(entry, dir string, tree map[string][]byte) string {
	entry = strings.TrimSpace(entry)
	if entry == "" || isRemoteResource(entry) {
		return ""
	}
	if strings.HasPrefix(entry, "$") {
		entry = stripValuesRef(entry)
		if entry == "" {
			return ""
		}
	}
	for _, cand := range []string{
		cleanJoin(dir, entry),
		path.Clean(entry),
		cleanJoin(dir, path.Base(entry)),
	} {
		if p := existingNonKRM(cand, tree); p != "" {
			return p
		}
	}
	return ""
}

// stripValuesRef drops the leading "$ref/" token from a $-prefixed Argo valueFiles entry, leaving
// the path the ref names. "$values/platform/cert-manager/values.yaml" -> that path;
// a bare "$values" (naming the ref root, no file) -> "".
func stripValuesRef(entry string) string {
	if i := strings.IndexByte(entry, '/'); i >= 0 {
		return entry[i+1:]
	}
	return ""
}

// existingNonKRM returns p when the scan holds a file at p whose content is not a Kubernetes
// manifest (a values file), and "" otherwise. The non-KRM test is the same apiVersion+kind
// presence check the indexer uses to decide a document is not KRM, so the two agree on what a
// values file is.
func existingNonKRM(p string, tree map[string][]byte) string {
	if p == "" {
		return ""
	}
	content, ok := tree[p]
	if !ok || looksLikeKRM(content) {
		return ""
	}
	return p
}

// looksLikeKRM reports whether the first document of content carries both an apiVersion and a
// kind — the minimum that makes a YAML document a Kubernetes manifest rather than a values
// file. It reads the reason code (the two fields), never the payload.
func looksLikeKRM(content []byte) bool {
	var head struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(content, &head); err != nil {
		return false
	}
	return head.APIVersion != "" && head.Kind != ""
}
