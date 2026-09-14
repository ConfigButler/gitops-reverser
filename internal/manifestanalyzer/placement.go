// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// PlacementPolicy is a resolved GitTarget placement declaration: an exact-type map plus a fallback
// default template. It mirrors api/v1alpha3.GitTargetPlacementSpec but is defined locally so this
// package stays free of Kubernetes API types.
//
// No sensitive/normal split: sensitivity is a write-safety property enforced after resolution, not
// a second map to configure. A nil policy falls through to the kustomize-root fallback, then
// canonical. See docs/layout/new-file-placement-rules.md.
type PlacementPolicy struct {
	ByType  map[string]string
	Default string
	// UseKustomize is spec.placement.useKustomize: create a kustomization.yaml at the write jail's
	// root when nothing governs a new document's path, and register the document in it. It is read
	// by the writer rather than by LocateNew, because creating a file is not a path decision.
	UseKustomize bool
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
	// Labels is metadata.labels as the writer will commit them — the SANITIZED object's labels,
	// not the raw cluster object's, so one internal/sanitize strips (kustomize.toolkit.fluxcd.io/*,
	// kro.run/*, applyset.kubernetes.io/*) is already gone by the time it gets here. That is why
	// the GitTarget's Validated gate refuses a template reading one of those: it could never
	// discriminate by it. Nil for a resource carrying no labels.
	//
	// A key the resource does not set — or sets to the empty string, which Kubernetes allows —
	// renders the "{label:key|fallback}" fallback the template declared, and the built-in
	// placementUnlabeledSentinel when it declared none. Either way the resource is placed: the
	// DEFAULT never renders an empty segment, because that would fold every unlabeled resource
	// of the type one directory up. A template can still ask for exactly that fold, by declaring
	// an empty fallback ("{label:key|}").
	Labels map[string]string
}

// PlacementSource names which mechanism produced a Path: the "why here" answer for one document.
// The values are a public observability contract (metric label values), so lower_snake_case and
// not renamed lightly.
//
// Exactly four, closed by construction: two kinds of declaration, one structural fact about the
// folder, and the built-in path. Nothing reads the repository's layout to guess intent.
//
// The two declared values are separate because a catch-all quietly swallowing a type you meant to
// name explicitly otherwise looks identical to a rule working as intended, and telling those apart
// is the whole job of a metric that answers "is a placement rule missing?".
type PlacementSource string

const (
	// PlacementSourceByType is an explicit placement.byType entry for this exact type.
	PlacementSourceByType PlacementSource = "by_type"
	// PlacementSourceDefault is the declared catch-all, placement.default: no byType entry named
	// this type, so the target's own fallback template answered instead. Distinct from
	// PlacementSourceCanonical, which is the absence of any declaration at all.
	PlacementSourceDefault PlacementSource = "default"
	// PlacementSourceKustomizeRoot is the structural fallback: no declared template
	// matched, and the whole writable subtree is governed by exactly one supported
	// kustomization, so the new document goes beside it and into its resources: list
	// (see resolveKustomizeRoot). It is a fact about reachability, not a guess about
	// convention: a file that root cannot reach would never render at all.
	PlacementSourceKustomizeRoot PlacementSource = "kustomize_root"
	// PlacementSourceCanonical is the built-in, versionless
	// {namespace}/{group}/{resource}/{name}.yaml fallback: no declared
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

// PlacementRefusalReason names WHY a placement was refused, from a closed set. A metric label
// value as well as a log field, so lower_snake_case and part of the observability contract.
//
// A refusal is a resource the operator did NOT mirror, so it must be countable per
// (GitTarget, type): a misconfigured template silently skipping one Secret every reconcile is
// otherwise invisible unless somebody reads the logs.
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

// LocateNew resolves the placement of a resource with no existing document: a declared template
// wins; otherwise the folder's one supported kustomize root; otherwise the canonical path.
// Nothing reads the layout of the OTHER documents of this type.
//
// store is the pre-plan snapshot and must never be mutated mid-batch: its reads must answer the
// same way for every resource in one batch, so several new creates resolve order-independently.
//
// An error means the placement cannot be honoured safely (a sensitive resource whose path already
// exists; sensitive documents are never appended). The caller must skip that resource rather than
// write into a shared file. See docs/layout/new-file-placement-rules.md.
func LocateNew(store *ManifestStore, policy *PlacementPolicy, req PlacementRequest) (PlacementResult, error) {
	vars := placementVars(req)

	// A declared template can only fail to render because its own TEXT is wrong — an unknown
	// variable. The Validated gate rejects those before any write, so reaching one here means
	// the spec is ahead of the gate; fall through below rather than erroring mid-batch.
	// "{label:key}" never fails to render: a resource that does not carry the label gets the
	// "{label:key|fallback}" value, or the built-in unlabeled sentinel if the template named
	// none (see placementUnlabeledSentinel).
	if path, source, ok, err := resolveDeclared(policy, req, vars); err == nil && ok {
		return finishPlacement(store, req, path, source)
	}

	if path, ok := resolveKustomizeRoot(store, req); ok {
		return finishPlacement(store, req, path, PlacementSourceKustomizeRoot)
	}

	if path, ok := resolveDeclaredKustomizeFolder(store, policy, req); ok {
		return finishPlacement(store, req, path, PlacementSourceKustomizeRoot)
	}

	return finishPlacement(store, req, canonicalPath(req), PlacementSourceCanonical)
}

// resolveKustomizeRoot is a structural fact, not a reading of conventions. The canonical path is a
// tree no resources: graph can reach, so a new document in a kustomize-managed folder would land
// outside every render and never be applied. With exactly one supported kustomization the resource
// belongs beside its other files.
//
// The destination follows from there being ONE root, never from picking a cohort: more than one
// declines rather than guessing. The predicate lives in writableRenderRoots (layout.go) because
// status.placement reports the rung this function takes and the two must not drift.
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

// resolveDeclaredKustomizeFolder stands in for the rung above when the folder has no root YET:
// useKustomize makes the writer create one in the same commit, so a new document belongs beside it.
// Without this the first document would land at the canonical path, outside the root just created.
//
// It reports the same PlacementSource because it IS that rung, and the label set is a public
// contract. It runs only when there is NO writable root: one is the rung above, several is refused
// before placement is asked.
func resolveDeclaredKustomizeFolder(
	store *ManifestStore,
	policy *PlacementPolicy,
	req PlacementRequest,
) (string, bool) {
	if policy == nil || !policy.UseKustomize {
		return "", false
	}
	if len(writableRenderRoots(store, req.WriteScope)) != 0 {
		return "", false
	}
	name := req.Identifier.Name + ".yaml"
	if req.Sensitive {
		name = req.Identifier.Name + ".sops.yaml"
	}
	return cleanJoin(req.WriteScope, name), true
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
	// Only safe to append when every document already there is cleanly editable. A file tolerating
	// a non-editable construct may hold a document that looks like a match but does not claim its
	// identity, and the writer cannot vouch for it. Fall back to writeWholeFile and its guard.
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

// namespaceIsInheritedFromContext reports whether a new document must OMIT metadata.namespace
// because the build context supplies it. Two conditions: the kustomization sets a namespace:
// transformer, AND it sets it to THIS resource's own namespace.
//
// The second is the safety half. Omitting hands the namespace to kustomize, so a transformer
// naming a DIFFERENT namespace would render the document as another object entirely. Writing it
// explicitly there is the only truthful bytes available, and the oracle then reports the folder as
// unable to express the object rather than mis-rendering it.
//
// Cluster-scoped resources never inherit one. This applies to DECLARED paths too, not just the
// kustomize-root fallback.
func namespaceIsInheritedFromContext(k *KustomizationInfo, req PlacementRequest) bool {
	if k.Namespace == "" || req.Identifier.Namespace == "" {
		return false
	}
	return k.Namespace == req.Identifier.Namespace
}

// governingKustomization returns the kustomization a new file must join to render: the NEAREST one
// at or above its directory, bounded by the write jail. Without the walk, a declared path into a
// subdirectory commits a document nothing renders, and the oracle (armed only for governed writes)
// does not catch it.
//
// The jail bound is load-bearing: a kustomization ABOVE the write scope is a read-only ancestor
// whose resources: list is not ours to edit. With no scope the walk may reach the subtree root.
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

// GoverningKustomization is governingKustomization for the writer, which has to ask a question
// LocateNew's result cannot answer: PlacementResult.Kustomization is set only when a governing root
// exists AND does not already list the path, so a nil there means either "no root" or "already
// listed". Creating a root is only correct for the first of those.
func GoverningKustomization(store *ManifestStore, writeScope, resolvedPath string) *KustomizationInfo {
	return governingKustomization(store, writeScope, resolvedPath)
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

// ValidateResolvedPlacementPath checks a fully-substituted placement path: non-empty, clean and
// relative, under spec.path, no backslashes, a real file name, a YAML suffix. Run on every path
// before a byte is written, because sanitizePlacementSegment defends each variable's VALUE but the
// template's own literal text is author-supplied and otherwise unconstrained.
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

// resolveDeclared renders the declared template that answers for this type, and reports WHICH
// declaration answered: an exact byType entry, or the target's catch-all default.
func resolveDeclared(
	policy *PlacementPolicy,
	req PlacementRequest,
	vars map[string]string,
) (string, PlacementSource, bool, error) {
	if policy == nil {
		return "", "", false, nil
	}
	key := PlacementTypeKey(req.Identifier.Group, req.Identifier.Version, req.Identifier.Resource)
	var tmpl string
	var source PlacementSource
	switch {
	case strings.TrimSpace(policy.ByType[key]) != "":
		tmpl, source = policy.ByType[key], PlacementSourceByType
	case strings.TrimSpace(policy.Default) != "":
		tmpl, source = policy.Default, PlacementSourceDefault
	default:
		return "", "", false, nil
	}
	rendered, err := RenderPlacementTemplate(tmpl, vars)
	if err != nil {
		return "", "", false, err
	}
	return rendered, source, true, nil
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

// placementPlaceholderPattern matches anything "{...}"-shaped rather than only a legal variable
// name, so a placeholder this package does not understand is REPORTED instead of left in the path
// as literal text. That widening is what makes "{label:key}" safe to add: a label key may contain
// "/", so an unrecognized label placeholder pasted through verbatim would split into directories.
var placementPlaceholderPattern = regexp.MustCompile(`\{[^{}]*\}`)

// strayPlacementBracesError reports a template carrying a brace that is not part of a complete
// placeholder: "{namespace}/{label:team/{name}.yaml", or a nested "{label:{name}}".
//
// placementPlaceholderPattern only finds COMPLETE "{...}" runs, so without this check an unclosed
// one is not a placeholder at all — it is literal text, and it renders into the path verbatim.
// That is the exact failure the widened pattern was introduced to prevent, one level up: the
// example above resolves to "app/{label:team/cache.yaml", a clean relative .yaml path that every
// later gate accepts, so the writer would create a directory literally named "{label:team".
// A brace in a path is never what an author meant, so the template is refused instead.
func strayPlacementBracesError(tmpl string) error {
	if residue := placementPlaceholderPattern.ReplaceAllString(tmpl, ""); strings.ContainsAny(residue, "{}") {
		return fmt.Errorf(
			"placement template %q has a brace that does not open or close a complete variable: "+
				"every %q and %q must belong to one, such as {name}",
			tmpl, "{", "}",
		)
	}
	return nil
}

// placementLabelPrefix introduces the one variable that takes an argument:
// "{label:app.kubernetes.io/instance}" renders that label's value on the placed resource.
const placementLabelPrefix = "label:"

// placementFallbackSeparator introduces the optional value a variable renders when the resource
// supplies none: "{label:team|unassigned}", "{namespace|_global}". "|" is legal in neither a
// label key nor a namespace name, so it cannot be mistaken for part of either, and a "|" with
// nothing after it is a declared EMPTY fallback rather than a typo (see validPlacementFallback).
const placementFallbackSeparator = "|"

// placementNamespaceVariable is the namespace-position variable. It is named as a constant
// because three things must agree on it: the sentinel it falls back to, the fallback rule that
// keeps that position collision-proof, and the identity-completeness check that looks for it.
const placementNamespaceVariable = "namespace"

// placementUnlabeledSentinel is what "{label:key}" renders for a resource that does not carry
// the label (or carries it empty) when the template names no explicit "{label:key|fallback}".
//
// It plays the same role here that "_cluster" plays for a cluster-scoped {namespace}: a value no real
// label could ever hold — a label value must be alphanumeric at both ends, and this starts with
// "_" — so it can never collide with one, and it is a single documented constant rather than a
// per-deployment invisible default. A template that wants its own bucket name still writes
// "{label:key|fallback}"; this is only what renders when it doesn't, so every resource is still
// placed and the mirror never has a silent, label-shaped gap in it.
const placementUnlabeledSentinel = "_unlabeled"

// placementVariable is a parsed placeholder: the variable's name ("namespace", "label:team") and
// the fallback the template declared for it, if any.
//
// Two variables can be ABSENT for a resource that is otherwise perfectly placeable — {namespace}
// on a cluster-scoped resource, {label:key} on one that does not carry the label — and both
// answer that the same way: render the fallback the template declared, or a built-in sentinel
// when it declared none, so the resource is still placed and the mirror never has a
// variable-shaped gap in it. That shared shape is why one type parses both; the difference
// between them is not the grammar but what a fallback in an IDENTITY position is allowed to say,
// which is validNamespaceFallback's job.
type placementVariable struct {
	name        string
	fallback    string
	hasFallback bool
}

// parsePlacementVariable splits a placeholder's inner name into the variable's name and its
// optional fallback. It says nothing about whether either is legal — placementVariableFault does.
func parsePlacementVariable(name string) placementVariable {
	if base, fallback, found := strings.Cut(name, placementFallbackSeparator); found {
		return placementVariable{name: base, fallback: fallback, hasFallback: true}
	}
	return placementVariable{name: name}
}

// labelKey is the label this variable reads, and false when it is not a label variable.
func (v placementVariable) labelKey() (string, bool) {
	return strings.CutPrefix(v.name, placementLabelPrefix)
}

// absentSentinel is what the variable renders for a resource that supplies no value, when the
// template declares no fallback of its own. The second result is false for a variable that has
// no absent state — {name}, {resource} and the rest always have a value — and such a variable
// therefore takes no fallback either: the two questions have one answer, so they share one
// function rather than drifting apart in two lists.
func (v placementVariable) absentSentinel() (string, bool) {
	if _, isLabel := v.labelKey(); isLabel {
		return placementUnlabeledSentinel, true
	}
	if v.name == placementNamespaceVariable {
		return types.ClusterScopeSegment, true
	}
	return "", false
}

// renderable reports whether the variable NAME is one this package substitutes, ignoring any
// fallback. A label variable is renderable when its key is one Kubernetes would accept.
func (v placementVariable) renderable() bool {
	if key, isLabel := v.labelKey(); isLabel {
		return len(validation.IsQualifiedName(key)) == 0
	}
	switch v.name {
	case "group", "groupPath", "version", "apiVersion", "resource",
		"kind", "scope", placementNamespaceVariable, "name", "sensitiveSuffix":
		return true
	default:
		return false
	}
}

// Why a {namespace} fallback may name a real namespace, where an earlier cut of this refused one:
//
// That refusal assumed "{namespace|team-a}" could fold two distinct objects onto one path — a
// cluster-scoped resource landing where a namespaced resource of the same type and name in
// "team-a" lands. It cannot. Scope is a property of the TYPE, not the object, so two resources
// rendering the same {groupPath}/{resource} are the same type and are therefore both
// cluster-scoped or both namespaced, while the fallback fires only for the former. An
// identity-complete template always carries those type variables, or is a ByType entry narrowed
// to one type, so the two resources that collision needs cannot both exist.
//
// What is left is a template that deliberately omits the type variables — a bundling path such as
// "{namespace|team-a}/all.yaml" — where a cluster-scoped resource joins the bundle namespace
// "team-a" writes. That is not a silent fold: bundling is a declared, supported layout, every
// document keeps its own identity inside the file, and the write-time co-mingle guards still
// refuse to put a sensitive document in a shared file. validateSecretSafety refuses a bundling
// default outright unless Secrets have an identity-complete route of their own.
//
// So a namespace fallback is fenced exactly as a label fallback is, by validPlacementFallback,
// which protects the PATH. Whether a bucket shares a folder with a real namespace is the author's
// call, the same call bundling has always been.

// placementFallbackPattern is the charset a declared "{label:key|fallback}" bucket may use: the
// label-VALUE charset, minus the label rule that both ends be alphanumeric.
//
// Dropping that end rule is the point rather than an oversight. A fallback that is itself a legal
// label value shares its bucket with resources genuinely labeled it — "{label:team|unassigned}"
// puts the unlabeled ones exactly where team=unassigned goes, which is sometimes wanted and
// sometimes a surprise. Allowing a leading "_" lets a template say "{label:team|_none}" and get a
// bucket no real value can reach, the same collision-proofing placementUnlabeledSentinel and
// "_cluster" are built on. What the charset keeps is the half that protects the path: no "/", no
// "\", no "%", no space, no control character, so a fallback can no more invent a directory or
// escape the write jail than a real label value can.
var placementFallbackPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// placementFallbackMaxLength is the length limit Kubernetes puts on a label VALUE. A fallback
// stands in for one, so it is held to the same ceiling rather than being allowed to grow into a
// path segment no label could have produced.
const placementFallbackMaxLength = 63

// validPlacementFallback reports whether a declared fallback is safe as one rendered path segment.
//
// The EMPTY fallback ("{label:key|}") is deliberately accepted, and renders nothing:
// collapseEmptyPathSegments then drops the segment, so an unlabeled resource lands one directory
// up instead of in a bucket of its own. That is the same fold placementUnlabeledSentinel exists to
// prevent — but the sentinel protects the DEFAULT, the case where nobody chose. A template that
// spells out an empty fallback has chosen, in text a reviewer can see in the GitTarget, so it is
// honoured rather than second-guessed. The fence that actually matters still holds either way:
// "." and ".." are refused outright and the charset refuses a separator, so the worst an author
// can do to themselves here is a flatter tree.
func validPlacementFallback(v string) bool {
	if v == "" {
		return true
	}
	if len(v) > placementFallbackMaxLength || v == "." || v == ".." {
		return false
	}
	return placementFallbackPattern.MatchString(v)
}

// isKnownPlacementVariable reports whether name is a variable RenderPlacementTemplate accepts,
// fallback included. Keep in sync with placementVars.
func isKnownPlacementVariable(name string) bool {
	return placementVariableFault(parsePlacementVariable(name)) == faultNone
}

// A placement variable is rejected for one of four reasons, and they are an enum rather than a
// bool so the error can say which: "unknown variable" is the wrong sentence for a fallback that
// is merely too long, and worse for one that would silently fold two resources onto a path.
type placementVariableFaultKind int

const (
	faultNone placementVariableFaultKind = iota
	faultUnknownName
	faultFallbackNotSupported
	faultFallbackUnsafe
)

// placementVariableFault reports why a parsed variable cannot be used, or faultNone.
func placementVariableFault(v placementVariable) placementVariableFaultKind {
	if !v.renderable() {
		return faultUnknownName
	}
	if !v.hasFallback {
		return faultNone
	}
	if _, canBeAbsent := v.absentSentinel(); !canBeAbsent {
		return faultFallbackNotSupported
	}
	if !validPlacementFallback(v.fallback) {
		return faultFallbackUnsafe
	}
	return faultNone
}

// placementVariableGuidance is the sentence that tells an author what is actually wrong with one
// placeholder, and "" when "unknown variable" already says it. A removed name, or a fallback the
// author clearly meant, reported as "unknown" reads like a typo and sends its reader hunting for
// a misspelling that is not there, so each is named and answered instead.
func placementVariableGuidance(placeholder string) string {
	name := strings.Trim(placeholder, "{}")
	if name == "namespaceOrCluster" {
		return "{namespaceOrCluster} was removed: {namespace} now renders \"" +
			types.ClusterScopeSegment + "\" for a cluster-scoped resource, so it is the only " +
			"namespace-position variable"
	}
	v := parsePlacementVariable(name)
	switch placementVariableFault(v) {
	case faultFallbackNotSupported:
		return fmt.Sprintf(
			"{%s} always has a value, so it takes no %q fallback: only {namespace}, which is absent "+
				"for a cluster-scoped resource, and {label:key}, which is absent for a resource that "+
				"does not carry the label, can fall back",
			v.name, placementFallbackSeparator,
		)
	case faultFallbackUnsafe:
		return fmt.Sprintf(
			"{%s} fallback %q is not usable as one path segment: at most %d characters of letters, "+
				"digits, \".\", \"_\" and \"-\", and neither \".\" nor \"..\"",
			v.name, v.fallback, placementFallbackMaxLength,
		)
	case faultNone, faultUnknownName:
		return ""
	}
	return ""
}

// unknownPlacementVariablesError names every placeholder in tmpl that is not a variable this
// package renders, shared by the render path and the static Validated gate so the two report a
// typo identically. A placeholder that names a REMOVED variable is reported with its
// replacement instead of being lumped in with the typos.
func unknownPlacementVariablesError(tmpl string, unknown []string) error {
	for _, placeholder := range unknown {
		if guidance := placementVariableGuidance(placeholder); guidance != "" {
			return fmt.Errorf("placement template %q: %s", tmpl, guidance)
		}
	}
	return fmt.Errorf(
		"placement template %q references unknown variable(s): %s",
		tmpl,
		strings.Join(unknown, ", "),
	)
}

func placementVars(req PlacementRequest) map[string]string {
	id := req.Identifier
	scope := "namespaced"
	if id.IsClusterScoped() {
		// {scope} is the readable "cluster"/"namespaced" descriptor. The namespace-position
		// VALUE is {namespace}, which renders types.ClusterScopeSegment here — the same word
		// the canonical path and a commit message use, so all three name a scope identically.
		scope = "cluster"
	}
	apiVersion := id.Version
	if id.Group != "" {
		apiVersion = id.Group + "/" + id.Version
	}
	sensitiveSuffix := ".yaml"
	if req.Sensitive {
		sensitiveSuffix = ".sops.yaml"
	}
	vars := map[string]string{
		"group":      id.Group,
		"groupPath":  id.Group,
		"version":    id.Version,
		"apiVersion": apiVersion,
		"resource":   id.Resource,
		"kind":       req.Kind,
		"scope":      scope,
		// The RAW namespace, empty for a cluster-scoped resource, not NamespaceOrCluster(): the
		// empty value is what tells the renderer this variable is absent, so {namespace} takes
		// the same declared-fallback-then-sentinel path {label:key} does instead of having
		// types.ClusterScopeSegment baked in here where no fallback could override it.
		placementNamespaceVariable: id.Namespace,
		"name":                     id.Name,
		"sensitiveSuffix":          sensitiveSuffix,
	}
	// Labels join the same map under their full placeholder name. The "label:" prefix is not a
	// legal bare variable, so a label literally named "namespace" cannot shadow the built-in one.
	for key, value := range req.Labels {
		vars[placementLabelPrefix+key] = value
	}
	return vars
}

// RenderPlacementTemplate expands a brace-variable path template ("{namespace}/
// secret-{name}.sops.yaml") against vars, then collapses empty path segments left
// behind by an omitted variable (e.g. "{groupPath}" for a core resource) so
// "{groupPath}/{version}/..." renders "v1/..." rather than "/v1/...". It returns an
// error naming any "{...}"-shaped placeholder that is not a known variable, and any brace that
// belongs to no complete placeholder at all, so a typo in a declared template is caught rather
// than silently left in the path as literal text.
func RenderPlacementTemplate(tmpl string, vars map[string]string) (string, error) {
	// Before substituting: a stray brace is not a variable this function can fail to resolve, it
	// is literal text that would be written into the repository as a path segment.
	if err := strayPlacementBracesError(tmpl); err != nil {
		return "", err
	}
	var unknown []string
	rendered := placementPlaceholderPattern.ReplaceAllStringFunc(tmpl, func(match string) string {
		v := parsePlacementVariable(strings.Trim(match, "{}"))
		if placementVariableFault(v) != faultNone {
			unknown = append(unknown, match)
			return match
		}
		// Labels join vars under their full "label:key" name, so one lookup serves every
		// variable and a label can never shadow a built-in one.
		//
		// Present-but-empty counts as absent. Kubernetes accepts an empty label value, and
		// rendering one leaves a segment collapseEmptyPathSegments then drops — folding every
		// resource that happens to carry the label empty onto one path, which is exactly the
		// silent identity fold sanitizePlacementSegment exists to prevent.
		if value := vars[v.name]; value != "" {
			return sanitizePlacementSegment(value)
		}
		// The declared fallback wins over the sentinel, including when it is empty: that is the
		// one case where this DOES render an empty segment, because the template asked for it in
		// writing (see validPlacementFallback).
		if v.hasFallback {
			return v.fallback
		}
		// A variable with no absent state renders "" here only because the resource genuinely has
		// no value for it — {groupPath} on a core resource — which collapseEmptyPathSegments then
		// drops, as the canonical path intends.
		sentinel, _ := v.absentSentinel()
		return sentinel
	})
	if len(unknown) > 0 {
		return "", unknownPlacementVariablesError(tmpl, unknown)
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
//
// It scans placeholders rather than rendering the template, because "{label:key}" has no static
// value to render: whether a resource sets that label, and so whether it renders the value or
// falls back to a fallback/sentinel, is a per-object fact answered at write time. What IS static,
// and checked here, is that the key itself is one Kubernetes would accept.
func ValidPlacementTemplateSyntax(tmpl string) error {
	if err := strayPlacementBracesError(tmpl); err != nil {
		return err
	}
	var unknown []string
	for _, match := range placementPlaceholderPattern.FindAllString(tmpl, -1) {
		if !isKnownPlacementVariable(strings.Trim(match, "{}")) {
			unknown = append(unknown, match)
		}
	}
	if len(unknown) > 0 {
		return unknownPlacementVariablesError(tmpl, unknown)
	}
	return nil
}

// PlacementTemplateLabelKeys lists the label keys tmpl reads, in template order, so a caller with
// knowledge this package does not have — which labels never survive to the write path — can reject
// a template that could never discriminate by the label it names: every resource of the type would
// render the same fallback/sentinel value, silently collapsing what looked like a per-label split
// into one bucket. Malformed label placeholders are left out; ValidPlacementTemplateSyntax reports
// those.
func PlacementTemplateLabelKeys(tmpl string) []string {
	var keys []string
	for _, match := range placementPlaceholderPattern.FindAllString(tmpl, -1) {
		v := parsePlacementVariable(strings.Trim(match, "{}"))
		if placementVariableFault(v) != faultNone {
			continue
		}
		if key, isLabel := v.labelKey(); isLabel {
			keys = append(keys, key)
		}
	}
	return keys
}

// ValidPlacementTemplatePath statically rejects a template whose own literal text could render
// outside spec.path or with the wrong file name: "..", a leading "/", a "\" separator, or a
// non-YAML suffix. Runs at the Validated gate, before any scan, so a bad template fails fast and
// visibly instead of skipping resource by resource.
//
// A "{label:key}" placeholder may itself contain the "/" of a prefixed label key, so the segment
// walk below sees it split in two. That is harmless: neither half can be ".." (the key is a
// qualified name), and the suffix check reads the whole string.
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

// templateReadsPlacementVariable reports whether tmpl uses the named variable, in either its bare
// form or with a declared fallback ("{namespace}" or "{namespace|_global}"). A caller asking
// "does this template carry the namespace?" means both.
func templateReadsPlacementVariable(tmpl, name string) bool {
	for _, match := range placementPlaceholderPattern.FindAllString(tmpl, -1) {
		v := parsePlacementVariable(strings.Trim(match, "{}"))
		if v.name == name && placementVariableFault(v) == faultNone {
			return true
		}
	}
	return false
}

// IdentityCompletePlacementTemplate reports whether tmpl renders a distinct path for every
// distinct resource identity, which every accepted sensitive template must. narrowedToOneType is
// true for a ByType entry; a Default template must carry the type variables itself.
//
// Those are {groupPath} and {resource}, deliberately NOT {version}: two served versions of one
// group/resource are the SAME object, so a version segment splits one identity rather than
// separating two.
//
// {label:key} deliberately counts for nothing here. A label is not identity — two Secrets in one
// namespace can carry the same one — so it can only ever ADD discrimination to a path that is
// already complete, never supply the part that is missing.
//
// A {namespace} carrying a fallback still counts, which is why the scope check parses rather than
// matching the literal "{namespace}". "{namespace|_global}" discriminates exactly as well as the
// bare form: the fallback fires only for cluster-scoped resources, and this template carries the
// type variables (or is narrowed to one type), so the resources that would have to collide are the
// same type and therefore the same scope. The other three variables take no fallback at all
// (placementVariableFault), so a literal match is exact for them.
func IdentityCompletePlacementTemplate(tmpl string, narrowedToOneType bool) bool {
	hasName := strings.Contains(tmpl, "{name}")
	hasScope := templateReadsPlacementVariable(tmpl, placementNamespaceVariable)
	if !hasName || !hasScope {
		return false
	}
	if narrowedToOneType {
		return true
	}
	return strings.Contains(tmpl, "{groupPath}") && strings.Contains(tmpl, "{resource}")
}

// --- Write-safety helpers for an already-occupied destination ------------------

// fileIsAppendSafe reports whether every document in fm is cleanly editable or ordinarily
// encrypted, never one tolerated despite an unsupported construct: such a document does not claim
// its identity and cannot be vouched for. A new resource must never join a file the writer cannot
// fully account for.
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
