/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package manifestanalyzer

import (
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/mapping"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// Plan is the first-class, cross-layer contract described in
// docs/design/manifest/current-manifest-support-review.md ("Writer Model: Plan,
// Apply, Dirty Flush"). It is a pure function of (ManifestStore, desired set,
// policy): the same value the live writer applies, scan mode renders, the CLI
// prints, and GitTarget status summarizes. M3 builds the model and its
// computation; applying it to a worktree is M7.
//
// It carries enough detail to render text/JSON/status without recomputing any
// decision: each action names its kind, the document it concerns, and a reason.
type Plan struct {
	// Actions are the decided changes, in a deterministic order (by file path, then
	// document index, then identity), so output is stable regardless of map
	// iteration order. A resource already in sync produces NO action — the plan
	// lists only what would change.
	Actions []PlanAction
	// Diagnostics are planning-level problems (e.g. a touched file whose bytes were
	// not provided for hydration). Store-level diagnostics stay on the ManifestStore.
	Diagnostics []manifestedit.Diagnostic
}

// PlanActionKind enumerates what a single action does. The seven kinds are the
// full vocabulary the materialized model speaks; which milestone *emits* each is
// noted below, because M3 (this milestone) computes the plan from a whole desired
// set, while a few kinds only arise once the apply/event layers land.
type PlanActionKind string

const (
	// PlanCreate places a desired resource that has no managed document in Git yet.
	// The target path is a placement decision made at apply time (M7), so a create
	// action carries the Desired object but a zero Ref.
	PlanCreate PlanActionKind = "create"
	// PlanPatch edits an existing document field-by-field (Decide said the Git
	// document differs from desired and a mapping-root patch is possible).
	PlanPatch PlanActionKind = "patch"
	// PlanReplace re-renders an existing document canonically (Decide could not
	// patch field-by-field, e.g. a non-mapping root).
	PlanReplace PlanActionKind = "replace"
	// PlanDeleteDocument removes one document from a multi-document file. It is part
	// of the apply vocabulary; the M3 planner does not emit it directly — a managed
	// drop is expressed as PlanDropOrphan, and whether removing it empties the file
	// is realized mechanically at apply time (M7). Reserved here so the kind set is
	// complete and stable.
	PlanDeleteDocument PlanActionKind = "delete-document"
	// PlanDeleteFile removes a whole file (its last managed document was dropped).
	// Like PlanDeleteDocument it is realized at apply time (M7), not emitted by the
	// M3 planner. Reserved here for completeness.
	PlanDeleteFile PlanActionKind = "delete-file"
	// PlanDropOrphan deletes a watched resource the API no longer has — the managed
	// drop. It is emitted only for a document whose GVK the mapper resolved to a
	// served, policy-allowed resource (mapping.MappingResolved) that has no desired
	// counterpart. Duplicate identities and unwatched API-backed KRM produce NO plan
	// action: they are acceptance facts (M4), not planning outcomes. Allowlisted
	// non-API KRM produces none either (it never resolves to a served resource).
	PlanDropOrphan PlanActionKind = "drop-orphan"
	// PlanSkip marks a document that exists but cannot be edited in place
	// (encrypted, a disallowed construct, or a soft Decide skip). It is reported,
	// never silently acted on.
	PlanSkip PlanActionKind = "skip"
)

// PlanAction is one decided change over a single document or desired resource.
type PlanAction struct {
	// Kind is what the action does.
	Kind PlanActionKind
	// Ref is the Git document the action concerns. It is the zero value for
	// PlanCreate, which has no existing location.
	Ref RecordRef
	// Identity is the manifest identity (apiVersion + kind + namespace + name)
	// involved, always set.
	Identity manifestedit.Identity
	// Resource is the resolved API-side identity (GVR + namespace + name). For a
	// desired-side action (create / patch / replace / skip) it is the cluster-truth
	// identity carried on the DesiredResource, so a create carries everything
	// ResourceIdentifier.ToGitPath needs to place a new file at apply time (M7)
	// without re-resolving the mapping. For a Git-only managed drop it is the
	// store-resolved identity. It is the zero value only when neither is known (a
	// skip over a document a structure-only store never resolved).
	Resource types.ResourceIdentifier
	// Desired is the clean object Git should contain, set for create/patch/replace
	// and nil for removals and skips.
	Desired *unstructured.Unstructured
	// Reason is a human-readable explanation, carried so renderers and status need
	// no recomputation.
	Reason string
}

// Counts returns the number of actions per kind, for a bounded status summary.
func (p Plan) Counts() map[PlanActionKind]int {
	out := make(map[PlanActionKind]int)
	for _, a := range p.Actions {
		out[a.Kind]++
	}
	return out
}

// DesiredResource is one resource in the COMPLETE desired snapshot the planner
// compares Git against: a resource the cluster currently has, paired with the
// API-side identity the controller already resolved from the GVR it watched (or the
// CLI from the mapper). Carrying the ResourceIdentifier is what lets a create place
// its new file (ResourceIdentifier.ToGitPath) at apply time (M7) without re-resolving
// the mapping.
//
// This is a full-snapshot input — the "Resync" path of the design's "Two Paths, One
// Plan Type" (docs/design/manifest/reconcile-via-watchlist-mark-and-sweep.md). It is
// NOT a per-event PendingChange: BuildPlan mark-and-sweeps every watched document
// absent from this set as a managed drop, so the set must be the whole desired state
// (scan mode / resync), never a partial batch. Steady-state, per-event planning that
// targets a single identity and emits an explicit delete-document — without sweeping
// — is the separate pending-change path (M7, on M6's delete-identity resolution).
//
// Object must be non-nil: every entry in a desired snapshot is a resource that
// exists. A nil Object is a malformed entry, ignored — deliberately NOT a delete
// tombstone, because in a sweeping planner a lone tombstone is indistinguishable from
// "every other document is now an orphan".
type DesiredResource struct {
	Resource types.ResourceIdentifier
	Object   *unstructured.Unstructured
}

// Policy is the injected planning policy. The planner stays a pure function and
// pulls every cluster-shaped or rendering-shaped decision out into this struct, so
// the production wiring (manifestreport.Project / EditOptions) lives at the call
// sites and tests can substitute their own.
type Policy struct {
	// Project maps a live API object to the clean desired state Git should contain.
	// A nil Project is treated as an identity passthrough (the object compared as-is).
	Project func(*unstructured.Unstructured) *unstructured.Unstructured
	// EditOptions are the manifestedit options (canonical renderer, list-match) used
	// when Decide must compare and choose patch vs. whole-replace.
	EditOptions manifestedit.EditOptions
}

// BuildPlan computes the Plan from the byte-free ManifestStore, the file bytes
// that back it (hydration source for the patch/no-op decision), the COMPLETE desired
// snapshot, and the policy. It graduates manifestreport.BuildReport's read-only
// create/update/delete/skip comparison into the materialized model's plan.
//
// This is the full-snapshot "Resync" planner (scan mode, CLI, initial reconcile /
// resync): it mark-and-sweeps — every watched document with no entry in desired is a
// managed drop — so desired MUST be the whole desired state, never a partial batch.
// The steady-state path (one plan action per live event, where a DELETED event is an
// explicit delete-document and nothing re-sweeps) is M7, not this function.
//
// The store is expected to have been built with the same mapper whose watched set
// produced desired; under a structure-only store (no resolved mappings) no managed
// drop is ever emitted, preserving the no-cluster promise even if a desired set is
// passed by mistake.
func BuildPlan(
	store *ManifestStore,
	files []manifestedit.FileContent,
	desired []DesiredResource,
	policy Policy,
) Plan {
	project := policy.Project
	if project == nil {
		project = func(obj *unstructured.Unstructured) *unstructured.Unstructured { return obj }
	}
	b := &planBuilder{
		store:         store,
		project:       project,
		opts:          policy.EditOptions,
		contentByPath: indexContent(files),
		docLoc:        documentLocations(store),
		nonClaiming:   nonClaimingIdentities(store),
		collided:      collidedIdentities(store),
		matched:       map[*DocumentModel]bool{},
	}

	// Desired side: create / patch / replace / skip for every cluster object, and
	// mark the documents it matched so the Git-only sweep below does not drop them.
	for _, dr := range desired {
		if dr.Object == nil {
			// A malformed snapshot entry, ignored. It is NOT a delete tombstone: this
			// planner sweeps, so treating a lone nil as "delete" would be
			// indistinguishable from "every other document is orphaned". Per-event
			// delete intents are the separate steady-state path (M7).
			continue
		}
		b.planDesired(dr)
	}

	// Git-only side: documents with no desired counterpart — managed drops for
	// watched resources the API no longer has, skips for non-editable constructs,
	// and nothing at all for duplicates and unwatched API-backed KRM (acceptance
	// facts, not plan actions).
	for _, path := range sortedKeys(store.FilesByPath) {
		for _, dm := range store.FilesByPath[path].Documents {
			b.planGitOnly(dm)
		}
	}

	sortActions(b.actions)
	return Plan{Actions: b.actions, Diagnostics: b.diags}
}

// planBuilder accumulates a plan's actions and diagnostics while BuildPlan walks
// the desired set and the store. It keeps the resolved inputs in one place so the
// per-document classifiers stay small, and keeps Plan itself a pure value type.
type planBuilder struct {
	store         *ManifestStore
	project       func(*unstructured.Unstructured) *unstructured.Unstructured
	opts          manifestedit.EditOptions
	contentByPath map[string][]byte
	docLoc        map[*DocumentModel]RecordRef
	nonClaiming   map[manifestedit.Identity]bool
	collided      map[manifestedit.Identity]bool

	matched map[*DocumentModel]bool
	actions []PlanAction
	diags   []manifestedit.Diagnostic
}

// planDesired classifies one desired resource against the store and appends its
// action (or, for a no-op, nothing). dr.Resource is the cluster-truth identity, so
// every desired-side action carries it — a create included.
func (b *planBuilder) planDesired(dr DesiredResource) {
	obj := dr.Object
	id := identityOfObject(obj)
	if b.collided[id] {
		// A duplicate-identity collision refuses the whole GitTarget at acceptance
		// (M4): neither the winner nor the losers of a collided identity produce a
		// plan action. Suppress it here so an arbitrary copy is never edited.
		return
	}
	dm := b.store.ByManifestIdentity[id]
	if dm == nil {
		// No document claims this identity. If a non-claiming construct document
		// already holds it, Git has it but cannot edit it: defer to the skip the
		// Git-only sweep emits, rather than report a contradictory create. Only a
		// truly absent resource is a create.
		if b.nonClaiming[id] {
			return
		}
		b.actions = append(b.actions, PlanAction{
			Kind:     PlanCreate,
			Identity: id,
			Resource: dr.Resource,
			Desired:  b.project(obj),
			Reason:   "desired resource has no managed document in Git; placement is decided at apply time",
		})
		return
	}

	b.matched[dm] = true
	ref := b.docLoc[dm]
	if !dm.Editable {
		// Claims its identity but is not patchable in place (encrypted, or a
		// disallowed construct that still carries an identity): reported, never edited.
		b.actions = append(b.actions, PlanAction{
			Kind: PlanSkip, Ref: ref, Identity: id, Resource: dr.Resource,
			Reason: skipReason(dm.Cause),
		})
		return
	}

	content, ok := b.contentByPath[ref.FilePath]
	if !ok {
		b.diags = append(b.diags, manifestedit.Diagnostic{
			Level: manifestedit.DiagWarning, Path: ref.FilePath, DocumentIndex: ref.DocumentIndex,
			Message: "no file content provided for hydration; cannot plan this document",
		})
		b.actions = append(b.actions, PlanAction{
			Kind: PlanSkip, Ref: ref, Identity: id, Resource: dr.Resource,
			Reason: "file content unavailable for planning",
		})
		return
	}

	gitDoc, _ := manifestedit.NewDocumentAt(ref.FilePath, content, ref.DocumentIndex)
	decision := manifestedit.Decide(manifestedit.Comparison{
		Git: gitDoc, Desired: b.project(obj), Options: b.opts,
	})
	if kind, action := actionFromDecision(decision.Action); action {
		b.actions = append(b.actions, PlanAction{
			Kind: kind, Ref: ref, Identity: id, Resource: dr.Resource,
			Desired: b.project(obj), Reason: decision.Reason,
		})
	}
}

// planGitOnly classifies one Git document that no desired object matched.
func (b *planBuilder) planGitOnly(dm *DocumentModel) {
	if b.matched[dm] {
		return
	}
	if b.collided[dm.ManifestIdentity] {
		// Duplicate-identity collision: an acceptance fact (M4 refuses the folder),
		// never a plan action — for the first-occurrence winner or its losers.
		return
	}
	ref := b.docLoc[dm]
	if !dm.claimsIdentity() {
		// A disallowed construct that does not claim an identity: surfaced as a skip
		// so an operator sees a document Git holds but the editor refuses.
		b.actions = append(b.actions, PlanAction{
			Kind: PlanSkip, Ref: ref, Identity: dm.ManifestIdentity, Resource: resourceOf(dm),
			Reason: skipReason(dm.Cause),
		})
		return
	}
	// A claiming document with no desired counterpart. Only a watched resource — one
	// the mapper resolved to a served, policy-allowed GVR — is dropped. Unwatched
	// API-backed KRM, unserved KRM, and structure-only documents produce no action:
	// they are refused at acceptance, never pruned.
	if dm.Mapping == mapping.MappingResolved {
		b.actions = append(b.actions, PlanAction{
			Kind: PlanDropOrphan, Ref: ref, Identity: dm.ManifestIdentity, Resource: resourceOf(dm),
			Reason: "watched resource absent from the cluster: managed drop",
		})
	}
}

// actionFromDecision maps a manifestedit decision intent to a plan action kind. The
// boolean is false for ActionNoChange, which produces no plan action: an in-sync
// document is not a change. ActionDelete cannot arise here (desired is non-nil), so
// it falls through to a defensive skip.
func actionFromDecision(a manifestedit.DecisionAction) (PlanActionKind, bool) {
	switch a {
	case manifestedit.ActionNoChange:
		return "", false
	case manifestedit.ActionPatch:
		return PlanPatch, true
	case manifestedit.ActionReplace:
		return PlanReplace, true
	case manifestedit.ActionSkip:
		return PlanSkip, true
	case manifestedit.ActionDelete:
		return PlanSkip, true
	default:
		return PlanSkip, true
	}
}

// documentLocations indexes every managed document to its (file path, document
// index) reference. DocumentModel deliberately stores neither, so the planner
// derives both top-down here and hands manifestedit the position at hydration time.
func documentLocations(store *ManifestStore) map[*DocumentModel]RecordRef {
	out := map[*DocumentModel]RecordRef{}
	for path, fm := range store.FilesByPath {
		for _, dm := range fm.Documents {
			out[dm] = RecordRef{FilePath: path, DocumentIndex: dm.index}
		}
	}
	return out
}

// nonClaimingIdentities collects the manifest identities held only by documents
// that do not claim their identity (disallowed constructs). The desired side uses
// it to defer to the Git-only skip instead of reporting a contradictory create.
func nonClaimingIdentities(store *ManifestStore) map[manifestedit.Identity]bool {
	out := map[manifestedit.Identity]bool{}
	for _, fm := range store.FilesByPath {
		for _, dm := range fm.Documents {
			if !dm.claimsIdentity() {
				out[dm.ManifestIdentity] = true
			}
		}
	}
	return out
}

// collidedIdentities collects every manifest identity involved in a duplicate
// collision — the identities for which IsDuplicate flags at least one loser, which
// by definition also covers the first-occurrence winner that shares the identity.
// A duplicate collision refuses the whole GitTarget at acceptance (M4), so the
// planner emits no action for either copy; this set is how it suppresses both.
func collidedIdentities(store *ManifestStore) map[manifestedit.Identity]bool {
	out := map[manifestedit.Identity]bool{}
	for _, fm := range store.FilesByPath {
		for _, dm := range fm.Documents {
			if store.IsDuplicate(dm) {
				out[dm.ManifestIdentity] = true
			}
		}
	}
	return out
}

// resourceOf returns the resolved resource identity of a document, or the zero
// value when the mapper left it unresolved.
func resourceOf(dm *DocumentModel) types.ResourceIdentifier {
	if dm != nil && dm.ResourceIdentity != nil {
		return *dm.ResourceIdentity
	}
	return types.ResourceIdentifier{}
}

// skipReason renders a display reason for a skipped document from its structured
// cause, never from a diagnostic message string.
func skipReason(c DocumentCause) string {
	switch c.Kind {
	case CauseEncrypted:
		return "encrypted document: cannot patch in place"
	case CauseNonEditable:
		if c.Detail != "" {
			return "not editable: " + c.Detail
		}
		return "not editable"
	case CauseNone:
		return "document cannot be edited in place"
	default:
		return "document cannot be edited in place"
	}
}

// identityOfObject reads the manifest identity from a live API object, matching how
// manifestedit derives identity from YAML.
func identityOfObject(obj *unstructured.Unstructured) manifestedit.Identity {
	return manifestedit.Identity{
		APIVersion: obj.GetAPIVersion(),
		Kind:       obj.GetKind(),
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
	}
}

// indexContent maps each file path to its raw bytes for document hydration.
func indexContent(files []manifestedit.FileContent) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for _, f := range files {
		out[f.Path] = f.Content
	}
	return out
}

// sortedKeys returns the file paths of the store's managed files in sorted order,
// so the Git-only sweep is deterministic.
func sortedKeys(m map[string]*FileModel) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortActions orders actions deterministically by file path, then document index,
// then identity. Creates (zero Ref) group first, ordered by identity.
func sortActions(actions []PlanAction) {
	sort.SliceStable(actions, func(i, j int) bool {
		a, b := actions[i], actions[j]
		if a.Ref.FilePath != b.Ref.FilePath {
			return a.Ref.FilePath < b.Ref.FilePath
		}
		if a.Ref.DocumentIndex != b.Ref.DocumentIndex {
			return a.Ref.DocumentIndex < b.Ref.DocumentIndex
		}
		return identityString(a.Identity) < identityString(b.Identity)
	})
}

// identityString renders a manifest identity for stable sorting.
func identityString(id manifestedit.Identity) string {
	return id.APIVersion + "/" + id.Kind + "/" + id.Namespace + "/" + id.Name
}
