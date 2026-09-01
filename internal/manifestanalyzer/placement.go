// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// PlacementPolicy is a resolved GitTarget placement declaration (Option B2 of
// docs/layout/new-file-placement-rules.md): a single
// exact-type map plus a fallback default template, consulted for every resource
// regardless of sensitivity. It mirrors api/v1alpha3.GitTargetPlacementSpec
// field-for-field but is defined locally so this analyzer package stays free of any
// Kubernetes API type dependency; the git package converts the CRD spec into this
// shape.
//
// There is no sensitive/normal split here: sensitivity is a write-safety property
// (encrypt the content, keep the path identity-complete, never append or
// co-mingle) enforced after resolution — in finishPlacement (sensitive never
// appends) and in the writer (encrypt by classification) — not a second map to
// configure.
//
// A nil *PlacementPolicy, or one with no matching ByType entry and no Default,
// falls through to the kustomize-root fallback and then the canonical path.
type PlacementPolicy struct {
	ByType  map[string]string
	Default string
}

// PlacementRequest describes a resource with no existing document in Git — the
// only case placement runs for (an existing document is always updated in place at
// its current location; see docs/layout/new-file-placement-rules.md,
// "Existing manifests are still match-first").
type PlacementRequest struct {
	Identifier types.ResourceIdentifier
	Kind       string
	Sensitive  bool
	// WriteScope is the write jail relative to the scanned (render) root, set only when
	// render-root scoping re-rooted the scan past spec.path into a base an overlay reads.
	// Placement is documented as relative to spec.path, so a resolved path that would land
	// outside the jail (a declared or canonical path resolved against the render anchor) is
	// rebased under WriteScope rather than escaping it. Empty for a self-contained subtree,
	// where the scan root IS spec.path and every resolved path is already in scope.
	WriteScope string
}

// PlacementSource names which mechanism produced a PlacementResult's Path. It is
// the "why here" answer for one new document, and it is reported three ways: the
// write path's log line, the placements_total metric's `source` label, and the
// scan/dry-run trace. The values are a public observability contract — they are
// metric label values — so they are lower_snake_case and are not renamed lightly.
//
// There are exactly three, and the list is closed by construction: a declaration,
// one structural fact about the folder, and the built-in path. Nothing here reads
// the repository's *layout* to guess an intent — that was Option C's sibling-cohort
// ladder, and it is gone (see the deletion argument in
// docs/design/open-asks-priority.md).
type PlacementSource string

const (
	// PlacementSourceDeclared is Option B: an explicit placement.byType/default
	// template matched.
	PlacementSourceDeclared PlacementSource = "declared"
	// PlacementSourceKustomizeRoot is the structural fallback: no declared template
	// matched, and the whole writable subtree is governed by exactly one supported
	// kustomization, so the new document goes beside it and into its resources: list
	// (see resolveKustomizeRoot). It is a fact about reachability, not a guess about
	// convention: a file that root cannot reach would never render at all.
	PlacementSourceKustomizeRoot PlacementSource = "kustomize_root"
	// PlacementSourceCanonical is the built-in, versionless
	// {namespaceOrCluster}/{group}/{resource}/{name}.yaml fallback: no declared
	// template, and no single kustomize root to hang the file off. For a repository
	// with a hand-authored layout this is the signal that a placement.byType or
	// placement.default line is missing — which is why it is counted per
	// (GitTarget, type) rather than only logged. See recordPlacement in internal/git.
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
	// Kustomization is set when Path's directory carries a supported
	// kustomization.yaml whose resources: list does not already name Path — the
	// writer must add it as part of the same commit so kustomize picks the file
	// up ("add to the right kustomize file").
	Kustomization *KustomizationInfo
	// NamespaceInherited is true when Path's destination infers its namespace
	// from build context (a kustomization.yaml's namespace: transformer set to this
	// resource's own namespace) rather than from metadata.namespace in the file —
	// mirroring DocumentModel.NamespaceInheritedFromContext for a document that does
	// not exist yet. The writer must keep metadata.namespace out of the written
	// bytes, exactly as it already does for an in-place edit of an existing
	// document in the same context.
	NamespaceInherited bool
}

// PlacementRefusalReason names WHY a placement was refused, from a closed set. It is a
// metric label value (placement_refusals_total{reason}) as well as a log field, so the
// strings are lower_snake_case and are part of the observability contract.
//
// A refusal is a resource the operator did NOT mirror. It has to be countable per
// (GitTarget, type): before this it left a log line and, on the resync path, a single
// integer in a summary — neither of which a dashboard or an alert can reach, so a
// misconfigured template that silently skipped one Secret on every reconcile was
// invisible unless somebody read the logs.
type PlacementRefusalReason string

const (
	// PlacementRefusedInvalidPath is a resolved path that failed the write-jail gate:
	// empty, absolute, unclean, escaping via "..", or not a YAML file name. In practice
	// a declared template whose literal text is wrong.
	PlacementRefusedInvalidPath PlacementRefusalReason = "invalid_path"
	// PlacementRefusedSensitiveAppend is a sensitive resource whose resolved path already
	// holds a document. Sensitive documents are never appended, so the write is skipped
	// rather than co-mingled — usually a declared template that is not identity-complete.
	PlacementRefusedSensitiveAppend PlacementRefusalReason = "sensitive_append"
	// PlacementRefusedPlaintextOntoEncrypted is a plaintext resource routed onto a file
	// that already holds an encrypted document: appending would produce a
	// partially-encrypted file and overwriting would destroy the encrypted document.
	PlacementRefusedPlaintextOntoEncrypted PlacementRefusalReason = "plaintext_onto_encrypted"
	// PlacementRefusedMixedSensitivityNewFile is two resources of different sensitivity
	// resolving to the SAME brand-new path within one batch. LocateNew cannot see this
	// (it resolves every resource against the pre-batch snapshot), so the writer refuses
	// it; the value is defined here to keep one closed label domain for every refusal.
	PlacementRefusedMixedSensitivityNewFile PlacementRefusalReason = "mixed_sensitivity_new_file"
	// PlacementRefusedMultiDocumentTarget is a resolved path that holds a multi-document
	// file the writer cannot append to (one of its documents is not cleanly editable), so
	// the write would have to overwrite it and drop the siblings. Raised by the writer.
	PlacementRefusedMultiDocumentTarget PlacementRefusalReason = "multi_document_target"
)

// PlacementRefusedError is a placement that resolved but cannot be honoured safely. The
// caller must skip creating that resource and surface the refusal rather than writing.
// It carries a bounded Reason so the write path can count refusals by cause without
// matching on message text.
type PlacementRefusedError struct {
	Reason   PlacementRefusalReason
	Resource string
	Path     string
	detail   string
	cause    error
}

func (e *PlacementRefusedError) Error() string { return e.detail }

// Unwrap exposes the underlying validation error for an invalid-path refusal, so
// errors.Is/As still reach it.
func (e *PlacementRefusedError) Unwrap() error { return e.cause }

// LocateNew resolves the placement of a resource with no existing document, per
// docs/layout/new-file-placement-rules.md: a declared template (Option B)
// wins when present; otherwise the folder's one supported kustomize root, if it has
// exactly one; otherwise the canonical path.
//
// There is no step that reads the layout of the *other* documents of this type.
// Sibling-cohort inference (Option C) was removed: it let a human's edit to the
// repository change where the operator writes, with no Kubernetes object changing
// and nothing in status recording the move, and its central namespace-agnosticism
// guard had already failed once by cascading. The argument, and what replaced it
// (a declared byType line, plus the placements_total metric that says which
// (GitTarget, type) needs one), is in docs/design/open-asks-priority.md.
//
// store is still the pre-plan snapshot for the whole batch and must never be mutated
// mid-batch: the remaining store reads — does the resolved path already hold an
// append-safe file, does its directory carry a kustomization — must answer the same
// way for every resource in one batch, so a batch of several new creates resolves
// order-independently regardless of event order (P2 of the design doc).
//
// An error is returned only when the resolved placement cannot be honoured safely
// — currently, a sensitive resource whose resolved path already exists (sensitive
// documents are never appended; see "Sensitive placement and uniqueness" in the
// design doc). The caller must skip creating that resource and surface the error as
// a diagnostic rather than writing into a shared or multi-document sensitive file.
func LocateNew(store *ManifestStore, policy *PlacementPolicy, req PlacementRequest) (PlacementResult, error) {
	vars := placementVars(req)

	if path, ok, err := resolveDeclared(policy, req, vars); err == nil && ok {
		return finishPlacement(store, req, path, PlacementSourceDeclared)
	}

	if path, ok := resolveKustomizeRoot(store, req); ok {
		return finishPlacement(store, req, path, PlacementSourceKustomizeRoot)
	}

	return finishPlacement(store, req, canonicalPath(req), PlacementSourceCanonical)
}

// resolveKustomizeRoot is the one non-declared, non-canonical placement, and it is a
// structural fact rather than a reading of the repository's conventions. The canonical
// path is a {namespaceOrCluster}/{group}/{resource}/{name}.yaml tree a kustomization's
// resources: graph can never reach, so a new document in an otherwise kustomize-managed
// folder would land outside every render — not merely oddly placed, but never applied.
// When the whole writable subtree is governed by exactly one supported kustomization
// (today's single-context baseline), the new resource belongs beside that
// kustomization's other files, and finishPlacement adds the resources: entry.
//
// The destination follows from there being ONE root, not from picking a cohort: more
// than one supported kustomization under the scanned root is ambiguous and declines
// rather than guessing. That is why this survived the Option C deletion — deleting it
// would reintroduce the unreachable-file bug it was added to fix.
//
// The "exactly one writable supported kustomization" predicate lives in writableRenderRoots
// (layout.go), because status.placement reports the rung this function will take and the two
// must not be able to drift: a LayoutResolved that says SingleKustomization while placement
// declines here would be worse than no report at all.
func resolveKustomizeRoot(store *ManifestStore, req PlacementRequest) (string, bool) {
	roots := writableRenderRoots(store, req.WriteScope)
	if len(roots) != 1 {
		return "", false
	}
	only := store.Kustomizations[roots[0]]
	name := req.Identifier.Name + ".yaml"
	if req.Sensitive {
		name = req.Identifier.Name + ".sops.yaml"
	}
	return cleanJoin(slashDir(only.Path), name), true
}

// finishPlacement fills in the parts of a PlacementResult that depend only on the
// resolved path (whether it already exists, and whether its directory needs a
// kustomize resources: entry), and enforces the "sensitive never appends" rule.
// rebaseIntoWriteScope pulls a resolved placement path back under the write jail when
// render-root scoping anchored the scan past spec.path, so placement stays relative to
// spec.path as documented. A no-op for a self-contained subtree (scope == "") and for a path
// already in scope.
func rebaseIntoWriteScope(scope, resolvedPath string) string {
	if scope == "" || pathWithin(resolvedPath, scope) {
		return resolvedPath
	}
	return cleanJoin(scope, resolvedPath)
}

func finishPlacement(
	store *ManifestStore,
	req PlacementRequest,
	resolvedPath string,
	source PlacementSource,
) (PlacementResult, error) {
	// Render-root scoping re-roots the scan at the common ancestor of spec.path and the bases
	// it reads, so a resolved path is anchored there, not at spec.path. Placement is documented
	// as relative to spec.path, so rebase a path that would land outside the write jail back
	// under it before it is validated, checked for append, or matched to a kustomization.
	resolvedPath = rebaseIntoWriteScope(req.WriteScope, resolvedPath)
	// This is the one gate every resolution path — declared, the kustomize-root
	// fallback, and canonical alike — funnels through before a
	// byte is ever written, so a rendered path can never escape the GitTarget's
	// spec.path regardless of which mechanism produced it. See "Path validation"
	// in the design doc: non-empty, a clean relative path, no "..", and a YAML
	// suffix.
	if err := ValidateResolvedPlacementPath(resolvedPath); err != nil {
		return PlacementResult{}, &PlacementRefusedError{
			Reason:   PlacementRefusedInvalidPath,
			Resource: req.Identifier.String(),
			Path:     resolvedPath,
			detail: fmt.Sprintf(
				"placement for resource %s resolved to an invalid path: %v", req.Identifier.String(), err,
			),
			cause: err,
		}
	}
	res := PlacementResult{Path: resolvedPath, Source: source}
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
		return PlacementResult{}, &PlacementRefusedError{
			Reason:   PlacementRefusedSensitiveAppend,
			Resource: req.Identifier.String(),
			Path:     resolvedPath,
			detail: fmt.Sprintf(
				"placement for sensitive resource %s resolved to %q, which already holds a document; "+
					"sensitive resources are never appended to an existing file",
				req.Identifier.String(), resolvedPath,
			),
		}
	}
	// A plaintext resource must never join a file that already holds an encrypted
	// document: appending would sit its cleartext beside SOPS-encrypted data (a
	// partially-encrypted file), and falling through to writeWholeFile would
	// instead overwrite — destroy — the encrypted document. Under Option B2 the one
	// declared map is consulted for sensitive and normal resources alike, so this
	// runtime guard (not a separate sensitive placement block) is what keeps the two
	// classes from co-mingling for every sensitive type, core or operator-configured.
	if res.Append && !req.Sensitive && fileHoldsEncryptedDocument(fm) {
		return PlacementResult{}, &PlacementRefusedError{
			Reason:   PlacementRefusedPlaintextOntoEncrypted,
			Resource: req.Identifier.String(),
			Path:     resolvedPath,
			detail: fmt.Sprintf(
				"placement for resource %s resolved to %q, which already holds an encrypted document; "+
					"a plaintext resource is never appended to an encrypted file",
				req.Identifier.String(), resolvedPath,
			),
		}
	}
	if k := governingKustomization(store, req.WriteScope, resolvedPath); k != nil && !k.Unsupported {
		if !kustomizationListsResource(k, resolvedPath) {
			res.Kustomization = k
		}
		res.NamespaceInherited = namespaceIsInheritedFromContext(k, req)
	}
	return res, nil
}

// namespaceIsInheritedFromContext reports whether a new document at a path this
// kustomization governs must OMIT metadata.namespace, because the build context already
// supplies it. Two conditions, and the second one is the safety half:
//
//   - the kustomization sets a namespace: transformer at all, and
//   - it sets it to THIS resource's own namespace.
//
// The second condition is what keeps the write honest. Omitting metadata.namespace hands
// the namespace to kustomize, so if the transformer named a DIFFERENT namespace the
// document would render as another object entirely — the mirror would claim to hold a
// resource it does not. Writing the namespace explicitly in that case is not a
// convention break; it is the only truthful bytes available, and the render oracle then
// reports the folder as unable to express this object rather than silently mis-rendering
// it. (kustomize's namespace transformer overrides an explicit metadata.namespace, so the
// explicit line is redundant when the two agree and load-bearing when they do not.)
//
// A cluster-scoped resource has no namespace, so it never inherits one.
//
// This applies to every resolved path, not just the kustomize-root fallback: a DECLARED
// template pointing into a governed directory has exactly the same obligation, and before
// this it silently wrote a namespace: line the folder's own documents omit.
func namespaceIsInheritedFromContext(k *KustomizationInfo, req PlacementRequest) bool {
	if k.Namespace == "" || req.Identifier.Namespace == "" {
		return false
	}
	return k.Namespace == req.Identifier.Namespace
}

// governingKustomization returns the kustomization whose resources: list a new file at
// resolvedPath must join to render: the NEAREST one at or above the file's own directory,
// bounded by the write jail. Without this an overlay's new object would be committed to a
// file no kustomization includes, so it would never render and the oracle (armed only for
// governed writes) would not catch it — a silent divergence.
//
// The walk is what makes a DECLARED path into a subdirectory behave like every other path
// (#295). Before it, the lookup was the file's own directory plus a special case for the
// write scope's root, so `byType: {v1/configmaps: "configmaps/{name}.yaml"}` in a kustomize
// folder committed a document nothing renders — and the two cases differed by render-root
// scoping, which the user cannot see. It also silently skipped
// namespaceIsInheritedFromContext, writing a namespace: line the folder's own documents omit.
//
// The jail is the bound, and it is load-bearing: writeScope is where the target may write, so
// a kustomization ABOVE it is a read-only ancestor (a base pulled into the scan by render-root
// scoping) whose resources: list is not ours to edit. With no scope the whole scanned subtree
// is the target's own, so the walk may reach its root.
func governingKustomization(store *ManifestStore, writeScope, resolvedPath string) *KustomizationInfo {
	jail := path.Clean(writeScope)
	if writeScope == "" {
		jail = "."
	}
	for dir := slashDir(resolvedPath); ; {
		if k := store.Kustomizations[dir]; k != nil {
			return k
		}
		if dir == jail || dir == "." {
			return nil
		}
		parent := slashDir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
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
// single byte is written, so a bad declared template (Option B) can never
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
		// The namespace-position segment uses "_cluster", an illegal Kubernetes
		// namespace name (DNS-1123 forbids "_"), so it can never collide with a real
		// namespace — unlike a bare "cluster", which is a legal namespace name. This
		// mirrors ResourceIdentifier.ToGitPath's built-in canonical scope segment.
		// {scope} above stays the readable "cluster"/"namespaced" descriptor.
		nsOrCluster = "_cluster"
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
//
// The type variables are {groupPath} and {resource}, and deliberately NOT {version}
// (#295): the built-in canonical path is versionless because two served versions of one
// group/resource are the SAME object, so a version segment separates no identities — it
// splits one. Requiring it also judged the canonical shape we would default to as not
// identity-complete, which is what made a spec-level default fail our own validation gate.
func IdentityCompletePlacementTemplate(tmpl string, narrowedToOneType bool) bool {
	hasName := strings.Contains(tmpl, "{name}")
	hasScope := strings.Contains(tmpl, "{namespace}") || strings.Contains(tmpl, "{namespaceOrCluster}")
	if !hasName || !hasScope {
		return false
	}
	if narrowedToOneType {
		return true
	}
	return strings.Contains(tmpl, "{groupPath}") && strings.Contains(tmpl, "{resource}")
}

// --- Write-safety helpers for an already-occupied destination ------------------

// fileIsAppendSafe reports whether every document already in fm is cleanly
// editable or an ordinary encrypted document — never a document tolerated despite
// an unsupported construct (CauseNonEditable: an anchor, alias, or other disallowed
// pattern), which does not claim its identity and so cannot be vouched for. Such a
// file is excluded from the append decision (finishPlacement): a genuinely new
// resource must never be joined to a file the writer cannot fully account for. The
// caller falls back to writeWholeFile, whose own multi-document guard refuses rather
// than overwrites.
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
