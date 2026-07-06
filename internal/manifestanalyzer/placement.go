// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// PlacementPolicy is a resolved GitTarget placement declaration (Option B2 of
// docs/design/manifest/version2/gittarget-new-file-placement-rules.md): a single
// exact-type map plus a fallback default template, consulted for every resource
// regardless of sensitivity. It mirrors api/v1alpha3.GitTargetPlacementSpec
// field-for-field but is defined locally so this analyzer package stays free of any
// Kubernetes API type dependency; the git package converts the CRD spec into this
// shape.
//
// There is no sensitive/normal split here: sensitivity is a write-safety property
// (encrypt the content, keep the path identity-complete, never append or
// co-mingle) enforced after resolution — in finishPlacement (sensitive never
// appends), in the writer (encrypt by classification), and in cohortMembers
// (inference never crosses the encrypted boundary) — not a second map to configure.
//
// A nil *PlacementPolicy, or one with no matching ByType entry and no Default,
// falls through to sibling inference (Option C) and then the canonical fallback.
type PlacementPolicy struct {
	ByType  map[string]string
	Default string
}

// PlacementRequest describes a resource with no existing document in Git — the
// only case placement runs for (an existing document is always updated in place at
// its current location; see docs/design/manifest/version2/
// gittarget-new-file-placement-rules.md, "Existing manifests are still match-first").
type PlacementRequest struct {
	Identifier types.ResourceIdentifier
	Kind       string
	Sensitive  bool
}

// PlacementSource names which mechanism produced a PlacementResult's Path, for
// logging and the scan/dry-run "why here" trace (P8 in the design doc).
type PlacementSource string

const (
	// PlacementSourceDeclared is Option B: an explicit placement.byType/default
	// template matched.
	PlacementSourceDeclared PlacementSource = "declared"
	// PlacementSourceInferred is Option C: no declared template matched, but an
	// existing sibling cohort determined the destination.
	PlacementSourceInferred PlacementSource = "inferred"
	// PlacementSourceCanonical is the built-in {group}/{version}/{resource}/
	// {namespace}/{name}.yaml fallback: no declared template and no sibling to
	// follow (e.g. an empty repository, or the type/namespace is new).
	PlacementSourceCanonical PlacementSource = "canonical"
)

// PlacementResult is where a new resource should be written.
type PlacementResult struct {
	// Path is the resolved file path (slash-separated), relative to the scanned
	// root (the GitTarget's spec.path).
	Path string
	// Append is true when Path already exists as a managed file the new document
	// should be appended to as an additional document; false for a brand-new file.
	Append bool
	// Source names which mechanism produced Path.
	Source PlacementSource
	// Cohort describes the sibling cohort and ladder step that produced Path;
	// empty unless Source is PlacementSourceInferred.
	Cohort string
	// Kustomization is set when Path's directory carries a supported
	// kustomization.yaml whose resources: list does not already name Path — the
	// writer must add it as part of the same commit so kustomize picks the file
	// up (F4's "add to the right kustomize file").
	Kustomization *KustomizationInfo
	// NamespaceInherited is true when Path's destination infers its namespace
	// from build context (a kustomization.yaml's namespace: transformer) rather
	// than from metadata.namespace in the file — mirroring
	// DocumentModel.NamespaceInheritedFromContext for a document that does not
	// exist yet. The writer must keep metadata.namespace out of the written
	// bytes, exactly as it already does for an in-place edit of an existing
	// document in the same context (see design doc: "the new file inherits its
	// sibling's NamespaceSource").
	NamespaceInherited bool
}

// LocateNew resolves the placement of a resource with no existing document, per
// docs/design/manifest/version2/gittarget-new-file-placement-rules.md: a declared
// template (Option B) wins when present; otherwise an existing sibling cohort
// decides (Option C, steps 1/2 — same type+namespace, then same type+any
// namespace); otherwise the canonical path.
//
// store MUST be the pre-plan snapshot for the whole batch and must never be mutated
// mid-batch, so a batch of several new creates resolves order-independently
// regardless of event order — a new resource never becomes another new resource's
// sibling within the same commit (P2 of the design doc).
//
// Step 3 (same namespace, any type) is deliberately not implemented: the design
// doc's own P5 discussion flags it as the highest-risk rung (an unbounded
// namespace-wide bundle that swallows every new type sharing a namespace), and
// steps 1/2/4 already cover the launch use cases (per-type bundles, per-type files,
// canonical). A namespace-bundle layout remains reachable via Option B.
//
// An error is returned only when the resolved placement cannot be honoured safely
// — currently, a sensitive resource whose resolved path already exists (sensitive
// documents are never appended; see "Sensitive placement and uniqueness" in the
// design doc). The caller must skip creating that resource and surface the error as
// a diagnostic rather than writing into a shared or multi-document sensitive file.
func LocateNew(store *ManifestStore, policy *PlacementPolicy, req PlacementRequest) (PlacementResult, error) {
	vars := placementVars(req)

	if path, ok, err := resolveDeclared(policy, req, vars); err == nil && ok {
		return finishPlacement(store, req, path, PlacementSourceDeclared, "", false)
	}

	if path, cohort, nsInherited, ok := resolveInferred(store, req); ok {
		return finishPlacement(store, req, path, PlacementSourceInferred, cohort, nsInherited)
	}

	if path, ok, nsInherited := resolveKustomizeRoot(store, req); ok {
		return finishPlacement(
			store, req, path, PlacementSourceInferred, "the GitTarget's one kustomization root", nsInherited,
		)
	}

	return finishPlacement(store, req, canonicalPath(req), PlacementSourceCanonical, "", false)
}

// resolveKustomizeRoot is a narrow, F4-specific fallback for when no sibling cohort
// exists (steps 1/2 both miss) — typically a resource whose type has never before
// appeared in this GitTarget. The canonical path (step 4) is a
// {group}/{version}/{resource}/{namespace}/{name}.yaml tree a kustomization's
// resources: graph can never reach, so a brand-new type in an otherwise
// kustomize-managed folder would silently land outside the folder's own
// convention — precisely the problem F4 exists to fix. When the whole scanned
// subtree is governed by exactly one supported kustomization (today's
// single-context baseline), the new resource belongs beside that kustomization's
// other files instead.
//
// This is intentionally narrower than the design doc's shelved step 3 (same
// namespace, any type): it never appends into an existing bundle file, and it only
// ever fires when there is exactly one supported kustomization for the whole
// GitTarget to be about — the destination follows from there being one root, not
// from picking the largest matching cohort — so it cannot become the "sink that
// swallows every new type" risk (P5) the doc's own step 3 raised. More than one
// supported kustomization under the scanned root is ambiguous and declines rather
// than guessing.
func resolveKustomizeRoot(store *ManifestStore, req PlacementRequest) (string, bool, bool) {
	var only *KustomizationInfo
	for _, k := range store.Kustomizations {
		if k.Unsupported {
			continue
		}
		if only != nil {
			return "", false, false
		}
		only = k
	}
	if only == nil {
		return "", false, false
	}
	name := req.Identifier.Name + ".yaml"
	if req.Sensitive {
		name = req.Identifier.Name + ".sops.yaml"
	}
	return cleanJoin(slashDir(only.Path), name), true, only.Namespace != ""
}

// finishPlacement fills in the parts of a PlacementResult that depend only on the
// resolved path (whether it already exists, and whether its directory needs a
// kustomize resources: entry), and enforces the "sensitive never appends" rule.
func finishPlacement(
	store *ManifestStore,
	req PlacementRequest,
	resolvedPath string,
	source PlacementSource,
	cohort string,
	namespaceInherited bool,
) (PlacementResult, error) {
	// This is the one gate every resolution path — declared, inferred, the
	// kustomize-root fallback, and canonical alike — funnels through before a
	// byte is ever written, so a rendered path can never escape the GitTarget's
	// spec.path regardless of which mechanism produced it. See "Path validation"
	// in the design doc: non-empty, a clean relative path, no "..", and a YAML
	// suffix.
	if err := ValidateResolvedPlacementPath(resolvedPath); err != nil {
		return PlacementResult{}, fmt.Errorf(
			"placement for resource %s resolved to an invalid path: %w", req.Identifier.String(), err,
		)
	}
	res := PlacementResult{Path: resolvedPath, Source: source, Cohort: cohort, NamespaceInherited: namespaceInherited}
	// A resolved path that already holds a file is only a safe append target when
	// every document already in it is cleanly editable. A file that tolerates a
	// non-editable construct (an anchor, alias, or other disallowed pattern) may
	// hold a document that looks like a match but does not actually claim its
	// identity — appending after it is not the data-loss risk that overwriting it
	// would be, but treating it as an ordinary bundle is still wrong: the writer
	// cannot vouch for what is already in that file. Append stays false, so the
	// caller falls back to writeWholeFile, whose own multi-document guard is the
	// established, tested safety net for exactly this collision.
	fm, exists := store.FilesByPath[resolvedPath]
	if exists && fileIsAppendSafe(fm) {
		res.Append = true
	}
	if req.Sensitive && res.Append {
		return PlacementResult{}, fmt.Errorf(
			"placement for sensitive resource %s resolved to %q, which already holds a document; "+
				"sensitive resources are never appended to an existing file",
			req.Identifier.String(), resolvedPath,
		)
	}
	// A plaintext resource must never join a file that already holds an encrypted
	// document: appending would sit its cleartext beside SOPS-encrypted data (a
	// partially-encrypted file), and falling through to writeWholeFile would
	// instead overwrite — destroy — the encrypted document. Under Option B2 the one
	// declared map is consulted for sensitive and normal resources alike, so this
	// runtime guard (not a separate sensitive placement block) is what keeps the two
	// classes from co-mingling for every sensitive type, core or operator-configured.
	if res.Append && !req.Sensitive && fileHoldsEncryptedDocument(fm) {
		return PlacementResult{}, fmt.Errorf(
			"placement for resource %s resolved to %q, which already holds an encrypted document; "+
				"a plaintext resource is never appended to an encrypted file",
			req.Identifier.String(), resolvedPath,
		)
	}
	if k := store.Kustomizations[slashDir(resolvedPath)]; k != nil && !k.Unsupported &&
		!kustomizationListsResource(k, resolvedPath) {
		res.Kustomization = k
	}
	return res, nil
}

func kustomizationListsResource(k *KustomizationInfo, resolvedPath string) bool {
	dir := slashDir(k.Path)
	for _, entry := range k.Resources {
		if cleanJoin(dir, entry) == resolvedPath {
			return true
		}
	}
	return false
}

// ValidateResolvedPlacementPath enforces the design doc's "Path validation"
// contract against a fully-resolved (variable-substituted) placement path,
// regardless of which mechanism produced it: non-empty, a clean relative path
// staying under the GitTarget's spec.path (no "..", not absolute, no redundant
// segments), no Windows-style backslash separators, a non-empty final file name,
// and a recognized YAML suffix (".sops.yaml"/".sops.yml" satisfy this too, since
// they end in ".yaml"/".yml"). finishPlacement runs this on every path before a
// single byte is written, so a bad declared template (F4 Option B) can never
// escape the folder the writer owns — sanitizePlacementSegment already defends
// each individual variable's value, but the template's own literal text is
// author-supplied and unconstrained without this gate.
func ValidateResolvedPlacementPath(p string) error {
	if p == "" {
		return errors.New("path is empty")
	}
	if strings.ContainsRune(p, '\\') {
		return fmt.Errorf("path %q must use \"/\" separators, not \"\\\"", p)
	}
	if path.IsAbs(p) {
		return fmt.Errorf("path %q must be relative, not absolute", p)
	}
	cleaned := path.Clean(p)
	if cleaned != p || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("path %q must be a clean relative path that stays under the GitTarget's spec.path", p)
	}
	base := path.Base(cleaned)
	if base == "" || base == "." || base == "/" {
		return fmt.Errorf("path %q has no file name", p)
	}
	if !strings.HasSuffix(cleaned, ".yaml") && !strings.HasSuffix(cleaned, ".yml") {
		return fmt.Errorf("path %q must end in .yaml or .yml", p)
	}
	return nil
}

// canonicalPath mirrors internal/git's generateFilePath (ResourceIdentifier.ToGitPath
// plus the .sops.yaml suffix for a sensitive resource). It is re-implemented here,
// not imported, because internal/git already imports manifestanalyzer and importing
// the other way would cycle; the duplicated logic is six lines and covered by tests
// on both sides.
func canonicalPath(req PlacementRequest) string {
	base := req.Identifier.ToGitPath()
	if !req.Sensitive {
		return base
	}
	if strings.HasSuffix(base, ".yaml") {
		return strings.TrimSuffix(base, ".yaml") + ".sops.yaml"
	}
	return base + ".sops.yaml"
}

// --- Option B: declared type-map placement -------------------------------------

func resolveDeclared(policy *PlacementPolicy, req PlacementRequest, vars map[string]string) (string, bool, error) {
	if policy == nil {
		return "", false, nil
	}
	key := PlacementTypeKey(req.Identifier.Group, req.Identifier.Version, req.Identifier.Resource)
	var tmpl string
	switch {
	case strings.TrimSpace(policy.ByType[key]) != "":
		tmpl = policy.ByType[key]
	case strings.TrimSpace(policy.Default) != "":
		tmpl = policy.Default
	default:
		return "", false, nil
	}
	rendered, err := RenderPlacementTemplate(tmpl, vars)
	if err != nil {
		return "", false, err
	}
	return rendered, true, nil
}

// PlacementTypeKey renders the exact-type key used by GitTargetPlacementSpec.ByType:
// "{group}/{version}/{resource}", with the group segment omitted for core resources
// ("v1/secrets", "apps/v1/deployments", "cert-manager.io/v1/certificates").
func PlacementTypeKey(group, version, resource string) string {
	if group == "" {
		return fmt.Sprintf("%s/%s", version, resource)
	}
	return fmt.Sprintf("%s/%s/%s", group, version, resource)
}

var placementVariablePattern = regexp.MustCompile(`\{[a-zA-Z]+\}`)

// isKnownPlacementVariable reports whether name is one of the variables
// RenderPlacementTemplate accepts. Keep in sync with placementVars and
// placementVariableNames.
func isKnownPlacementVariable(name string) bool {
	switch name {
	case "group", "groupPath", "version", "apiVersion", "resource",
		"kind", "scope", "namespace", "namespaceOrCluster", "name", "sensitiveSuffix":
		return true
	default:
		return false
	}
}

// placementVariableNames lists every variable isKnownPlacementVariable accepts,
// for callers (ValidPlacementTemplateSyntax) that need the full set rather than a
// single-name membership check.
func placementVariableNames() []string {
	return []string{
		"group", "groupPath", "version", "apiVersion", "resource",
		"kind", "scope", "namespace", "namespaceOrCluster", "name", "sensitiveSuffix",
	}
}

func placementVars(req PlacementRequest) map[string]string {
	id := req.Identifier
	scope := "namespaced"
	nsOrCluster := id.Namespace
	if id.IsClusterScoped() {
		scope = "cluster"
		nsOrCluster = "cluster"
	}
	apiVersion := id.Version
	if id.Group != "" {
		apiVersion = id.Group + "/" + id.Version
	}
	sensitiveSuffix := ".yaml"
	if req.Sensitive {
		sensitiveSuffix = ".sops.yaml"
	}
	return map[string]string{
		"group":              id.Group,
		"groupPath":          id.Group,
		"version":            id.Version,
		"apiVersion":         apiVersion,
		"resource":           id.Resource,
		"kind":               req.Kind,
		"scope":              scope,
		"namespace":          id.Namespace,
		"namespaceOrCluster": nsOrCluster,
		"name":               id.Name,
		"sensitiveSuffix":    sensitiveSuffix,
	}
}

// RenderPlacementTemplate expands a brace-variable path template ("{namespace}/
// secret-{name}.sops.yaml") against vars, then collapses empty path segments left
// behind by an omitted variable (e.g. "{groupPath}" for a core resource) so
// "{groupPath}/{version}/..." renders "v1/..." rather than "/v1/...". It returns an
// error naming any "{...}"-shaped placeholder that is not a known variable, so a
// typo in a declared template is caught rather than silently left as literal text.
func RenderPlacementTemplate(tmpl string, vars map[string]string) (string, error) {
	var unknown []string
	rendered := placementVariablePattern.ReplaceAllStringFunc(tmpl, func(match string) string {
		name := strings.Trim(match, "{}")
		if !isKnownPlacementVariable(name) {
			unknown = append(unknown, match)
			return match
		}
		return sanitizePlacementSegment(vars[name])
	})
	if len(unknown) > 0 {
		return "", fmt.Errorf(
			"placement template %q references unknown variable(s): %s",
			tmpl,
			strings.Join(unknown, ", "),
		)
	}
	return collapseEmptyPathSegments(rendered), nil
}

// sanitizePlacementSegment defends the identity-completeness guarantee: a
// Kubernetes name/namespace can never legally contain "/", but a template
// variable's value is substituted verbatim, so any stray path separator is
// percent-encoded rather than allowed to silently fold two distinct resources onto
// the same rendered path.
func sanitizePlacementSegment(v string) string {
	if !strings.ContainsAny(v, "/\\%") {
		return v
	}
	v = strings.ReplaceAll(v, "%", "%25")
	v = strings.ReplaceAll(v, "/", "%2F")
	v = strings.ReplaceAll(v, "\\", "%5C")
	return v
}

func collapseEmptyPathSegments(p string) string {
	parts := strings.Split(p, "/")
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "/")
}

// ValidPlacementTemplateSyntax reports whether tmpl references only known
// placement variables, independent of any resource identity — the check a
// GitTarget's Validated gate runs statically at reconcile time, before any
// repository scan.
func ValidPlacementTemplateSyntax(tmpl string) error {
	names := placementVariableNames()
	stub := make(map[string]string, len(names))
	for _, name := range names {
		stub[name] = ""
	}
	_, err := RenderPlacementTemplate(tmpl, stub)
	return err
}

// ValidPlacementTemplatePath statically rejects a declared template whose own
// literal text (never mind any variable substitution, which sanitizePlacementSegment
// already defends per-value) could render outside the GitTarget's spec.path or
// with the wrong kind of file name: an explicit ".." path segment, a leading "/"
// (absolute), a "\" separator, or a suffix that isn't ".yaml"/".yml" (a template
// ending in the literal "{sensitiveSuffix}" placeholder is accepted without
// rendering it, since that variable only ever expands to ".yaml" or ".sops.yaml").
// This runs at the GitTarget's Validated gate — before any repository scan, and
// before any resource can ever trigger a write — so a bad template fails fast and
// visibly instead of silently skipping (or, without ValidateResolvedPlacementPath's
// runtime backstop, escaping) resource by resource.
func ValidPlacementTemplatePath(tmpl string) error {
	trimmed := strings.TrimSpace(tmpl)
	if trimmed == "" {
		return errors.New("placement template is empty")
	}
	if strings.ContainsRune(trimmed, '\\') {
		return fmt.Errorf("placement template %q must use \"/\" separators, not \"\\\"", tmpl)
	}
	if strings.HasPrefix(trimmed, "/") {
		return fmt.Errorf("placement template %q must be relative, not absolute", tmpl)
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == ".." {
			return fmt.Errorf("placement template %q must not contain a \"..\" path segment", tmpl)
		}
	}
	if !strings.HasSuffix(trimmed, "{sensitiveSuffix}") &&
		!strings.HasSuffix(trimmed, ".yaml") && !strings.HasSuffix(trimmed, ".yml") {
		return fmt.Errorf("placement template %q must end in .yaml, .yml, or {sensitiveSuffix}", tmpl)
	}
	return nil
}

// IdentityCompletePlacementTemplate reports whether tmpl is guaranteed to render a
// distinct path for every distinct resource identity — the structural guarantee
// "Sensitive placement and uniqueness" in the design doc requires of every accepted
// sensitive template. narrowedToOneType is true for a ByType entry (the map key
// itself already names one exact type); a Default template must additionally carry
// the type variables since it applies across every type the class does not name
// explicitly.
func IdentityCompletePlacementTemplate(tmpl string, narrowedToOneType bool) bool {
	hasName := strings.Contains(tmpl, "{name}")
	hasScope := strings.Contains(tmpl, "{namespace}") || strings.Contains(tmpl, "{namespaceOrCluster}")
	if !hasName || !hasScope {
		return false
	}
	if narrowedToOneType {
		return true
	}
	return strings.Contains(tmpl, "{groupPath}") &&
		strings.Contains(tmpl, "{version}") &&
		strings.Contains(tmpl, "{resource}")
}

// --- Option C: sibling inference -------------------------------------------------

// resolveInferred implements Option C steps 1 and 2. See LocateNew's doc comment for
// why step 3 is not implemented.
func resolveInferred(store *ManifestStore, req PlacementRequest) (string, string, bool, bool) {
	id := req.Identifier

	if members := cohortMembers(
		store,
		id.Group,
		id.Version,
		id.Resource,
		id.Namespace,
		true,
		req.Sensitive,
	); len(
		members,
	) > 0 {
		if path, cohort, nsInherited, ok := cohortDestination(
			store,
			members,
			req,
			"same type and namespace",
			false,
		); ok {
			return path, cohort, nsInherited, true
		}
	}
	// Step 2 matches across namespaces, so — unlike step 1, where every candidate
	// already shares the new resource's own namespace — a candidate here must prove
	// it is namespace-agnostic before it can be trusted for a namespace it has never
	// seen (P4 of the design doc): a per-namespace-segmented layout (a dedicated
	// bundle or directory per namespace) must NOT be extended by guessing one of the
	// existing namespaces' files/directories for a brand-new namespace. cohortDestination
	// disqualifies any candidate that has not demonstrated it already spans more than
	// one namespace (a bundle) or lives in a single shared directory regardless of
	// namespace (singleton style); an unseen namespace then correctly falls through to
	// the canonical path, which builds the right namespace segment directly.
	if members := cohortMembers(store, id.Group, id.Version, id.Resource, "", false, req.Sensitive); len(members) > 0 {
		if path, cohort, nsInherited, ok := cohortDestination(
			store,
			members,
			req,
			"same type, any namespace",
			true,
		); ok {
			return path, cohort, nsInherited, true
		}
	}
	return "", "", false, false
}

// cohortMembers collects every existing document of the given type (optionally
// pinned to namespace) whose sensitivity matches the resource being placed. A
// document's sensitivity is read off the analyzer's own encrypted-document
// classification (CauseEncrypted) rather than a separately threaded policy, so a
// sensitive resource can never infer from a plaintext sibling or vice versa (the
// design doc's "sensitive stays hard-split — with no config").
func cohortMembers(
	store *ManifestStore,
	group, version, resource, namespace string,
	matchNamespace, sensitive bool,
) []*DocumentModel {
	var out []*DocumentModel
	for rid, dm := range store.ByResourceIdentity {
		if rid.Group != group || rid.Version != version || rid.Resource != resource {
			continue
		}
		if matchNamespace && rid.Namespace != namespace {
			continue
		}
		if isSensitiveDocument(dm) != sensitive {
			continue
		}
		out = append(out, dm)
	}
	return out
}

func isSensitiveDocument(dm *DocumentModel) bool {
	return dm.Cause.Kind == CauseEncrypted
}

// fileIsAppendSafe reports whether every document already in fm is cleanly
// editable or an ordinary encrypted document — never a document tolerated despite
// an unsupported construct (CauseNonEditable: an anchor, alias, or other disallowed
// pattern), which does not claim its identity and so cannot be vouched for. Such a
// file is excluded from both bundle and singleton-style candidacy (cohortDestination)
// and from the append decision (finishPlacement): a genuinely new resource must
// never be joined to a file the writer cannot fully account for.
func fileIsAppendSafe(fm *FileModel) bool {
	if fm == nil {
		return false
	}
	for _, d := range fm.Documents {
		if d.Cause.Kind == CauseNonEditable {
			return false
		}
	}
	return true
}

// fileHoldsEncryptedDocument reports whether fm already contains at least one
// encrypted (sensitive) document. finishPlacement uses it to refuse appending a
// plaintext resource into an encrypted file — the write-time half of the
// "sensitivity is a write-safety classifier, not a placement namespace" contract
// (Option B2 of the design doc).
func fileHoldsEncryptedDocument(fm *FileModel) bool {
	if fm == nil {
		return false
	}
	for _, d := range fm.Documents {
		if d.Cause.Kind == CauseEncrypted {
			return true
		}
	}
	return false
}

// cohortDestination decides, for one matched cohort, whether the repository's
// established pattern is "one resource per file" or "resources of this cohort share
// a file" (a bundle), and resolves the concrete destination:
//
//   - every file holding >1 document is a candidate bundle, keyed by path, weighted
//     by how many cohort members it holds;
//   - every file holding exactly one document is "singleton style," aggregated into
//     one virtual candidate regardless of how many separate files/directories it
//     spans, weighted by its total member count;
//   - the candidate with the most members wins; ties (including "no bundle beats
//     the singleton style") favour singleton style, the more conservative choice,
//     since it never grows an existing bundle the repository's siblings do not
//     clearly favour. Among multiple bundle files tied for the lead, the
//     lexicographically smallest file path wins; the singleton style's directory is
//     the lexicographically smallest directory among its members.
//
// This is deterministic and independent of map/walk iteration order (P1 of the
// design doc): the result depends only on the (path -> member count) shape of the
// pre-plan snapshot, never on the order LocateNew is called for other resources in
// the same batch.
//
// namespaceAgnostic is true only for step 2 (any namespace). It disqualifies a
// candidate that has not demonstrated it is independent of namespace: a bundle file
// must already hold members from more than one distinct namespace, and singleton
// style must have every member in exactly one directory (P4 — see resolveInferred).
// A step-1 candidate (namespaceAgnostic false) is never disqualified this way,
// because every member there already shares the new resource's own namespace.
func cohortDestination(
	store *ManifestStore,
	members []*DocumentModel,
	req PlacementRequest,
	step string,
	namespaceAgnostic bool,
) (string, string, bool, bool) {
	docLoc := store.DocumentLocations()
	perFile := map[string][]*DocumentModel{}
	for _, m := range members {
		if p := docLoc[m].FilePath; p != "" {
			perFile[p] = append(perFile[p], m)
		}
	}
	if len(perFile) == 0 {
		return "", "", false, false
	}

	singletonDirs, bestPath, bestCount, dirReps, bundleReps := classifyCohortLocations(
		store,
		perFile,
		namespaceAgnostic,
	)
	if bestPath == "" && len(singletonDirs) == 0 {
		return "", "", false, false
	}

	cohort := fmt.Sprintf("%d sibling(s) via %s", len(members), step)
	if bestCount > len(singletonDirs) {
		nsInherited := bundleReps[bestPath] != nil && bundleReps[bestPath].NamespaceInheritedFromContext()
		return bestPath, cohort, nsInherited, true
	}

	sort.Strings(singletonDirs)
	winDir := singletonDirs[0]
	name := req.Identifier.Name + ".yaml"
	if req.Sensitive {
		name = req.Identifier.Name + ".sops.yaml"
	}
	nsInherited := dirReps[winDir] != nil && dirReps[winDir].NamespaceInheritedFromContext()
	return cleanJoin(winDir, name), cohort, nsInherited, true
}

// classifyCohortLocations partitions a cohort's members by where they live:
// every file holding more than one document is a bundle candidate (keyed by path,
// weighted by member count); every file holding exactly one document contributes to
// the singleton-style candidate. A tainted file (fileIsAppendSafe false) is
// excluded from both. namespaceAgnostic applies the P4 safety rule (see
// resolveInferred): a bundle must already span more than one namespace, and
// singleton style must resolve to a single shared directory, or the candidate is
// dropped. It returns the eligible singleton directories, the winning bundle
// (path/count), and one representative document per singleton directory / bundle
// path — used to decide whether the destination's namespace is inherited from
// build context (see PlacementResult.NamespaceInherited) — determined
// independently of map iteration order by scanning candidate paths in sorted order.
func classifyCohortLocations(
	store *ManifestStore,
	perFile map[string][]*DocumentModel,
	namespaceAgnostic bool,
) ([]string, string, int, map[string]*DocumentModel, map[string]*DocumentModel) {
	var singletonDirs []string
	dirReps := map[string]*DocumentModel{}
	bundleReps := map[string]*DocumentModel{}
	bundleCounts := map[string]int{}
	for p, ms := range perFile {
		fm := store.FilesByPath[p]
		if !fileIsAppendSafe(fm) {
			continue // a tainted file is never a placement destination
		}
		if len(fm.Documents) > 1 {
			if namespaceAgnostic && !spansMultipleNamespaces(ms) {
				continue // unproven: looks like a per-namespace-segmented bundle (P4)
			}
			bundleCounts[p] = len(ms)
			bundleReps[p] = ms[0]
			continue
		}
		dir := slashDir(p)
		singletonDirs = append(singletonDirs, dir)
		if _, seen := dirReps[dir]; !seen {
			dirReps[dir] = ms[0]
		}
	}
	if namespaceAgnostic && !allSameDir(singletonDirs) {
		singletonDirs = nil // unproven: directories look namespace-segmented (P4)
	}

	bundlePaths := make([]string, 0, len(bundleCounts))
	for p := range bundleCounts {
		bundlePaths = append(bundlePaths, p)
	}
	sort.Strings(bundlePaths)
	bestPath, bestCount := "", 0
	for _, p := range bundlePaths {
		if bundleCounts[p] > bestCount {
			bestCount, bestPath = bundleCounts[p], p
		}
	}
	return singletonDirs, bestPath, bestCount, dirReps, bundleReps
}

// spansMultipleNamespaces reports whether ms (all documents sharing one file)
// carry more than one distinct namespace, proving the file is namespace-agnostic
// rather than one namespace's dedicated bundle.
func spansMultipleNamespaces(ms []*DocumentModel) bool {
	seen := map[string]struct{}{}
	for _, m := range ms {
		if m.ResourceIdentity == nil {
			continue
		}
		seen[m.ResourceIdentity.Namespace] = struct{}{}
		if len(seen) > 1 {
			return true
		}
	}
	return false
}

// allSameDir reports whether every directory in dirs is identical (trivially true
// for zero or one element).
func allSameDir(dirs []string) bool {
	for i := 1; i < len(dirs); i++ {
		if dirs[i] != dirs[0] {
			return false
		}
	}
	return true
}
