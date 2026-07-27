// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import "sort"

// Permanence says whether a refusal can ever stop being one. It is set by the check that
// raised the refusal, because only that check knows: the same code can be a wait, a
// redesign, or a one-commit fix depending on which branch emitted it.
//
// Consumers MUST treat an unrecognised or absent value as PermanenceUnknown and say
// nothing about the future.
//
// The taxonomy is deliberately three-valued plus a zero value. Collapsing "fixable" and
// "pending" tells a user to go fix something no user can fix; dropping the zero value
// makes an unclassified path emit a confident wrong answer instead of silence. See
// docs/design/analyzer-consumer-contract-asks.md (Ask 1).
type Permanence string

const (
	// PermanenceUnknown is the zero value: this refusal was not classified, so say
	// nothing about whether it can stop being one.
	PermanenceUnknown Permanence = ""
	// PermanenceFixable means a change to the repository or the GitTarget clears it
	// today. Actor names who can make that change.
	PermanenceFixable Permanence = "fixable"
	// PermanencePending means the operator does not support this yet and a future
	// release may accept it. Nobody on either side can clear it today.
	PermanencePending Permanence = "pending-upstream"
	// PermanencePermanent means the support boundary: never a "not yet".
	PermanencePermanent Permanence = "permanent"
)

// Actor names who can act on a fixable refusal. It is empty when the refusal is not
// fixable, or when nobody can act.
type Actor string

const (
	// ActorUnknown is the zero value: unclassified, or nobody can act.
	ActorUnknown Actor = ""
	// ActorAuthor is the person who owns the files in the repository.
	ActorAuthor Actor = "repository-author"
	// ActorPlatform is the person who owns the GitTarget — its scope, its path, and the
	// CRDs installed in the cluster it mirrors.
	ActorPlatform Actor = "platform-operator"
)

// Classification is the pair a raise site attaches to a refusal. Passing them together
// keeps a fixable refusal from losing the actor half on its way out of the check.
type Classification struct {
	Permanence Permanence
	Actor      Actor
}

// classified stamps a computed classification onto an issue. Raise sites with one fixed
// answer set Permanence and Actor in the literal instead; this exists for the sites whose
// answer depends on which branch fired.
func (i AcceptanceIssue) classified(c Classification) AcceptanceIssue {
	i.Permanence, i.Actor = c.Permanence, c.Actor
	return i
}

// kustomizeFeatureClassification maps every unsupported kustomization construct to its
// own permanence. This is the map the ask exists for: `unsupported-kustomize` is one code
// with three different answers, so classifying it per code would be a confident wrong
// sentence for two thirds of the constructs that raise it.
//
// The three groups:
//
//   - author-fixable — the file itself is broken or reaches outside the tree. Nothing
//     about the support boundary is in play; one commit clears it.
//   - pending-upstream — a construct the operator could model later. A remote base and a
//     Helm chart are read-only context whose adoption is the same shape as the
//     render-root scoping that already shipped, so calling them permanent would repeat
//     the mistake that retired ReasonOverlayFanOutUnsupported.
//   - permanent — the constructs whose output the writer cannot map back to a source
//     document at all: content synthesised at build time, fields rewritten after the
//     source is read, or arbitrary plugin code. This is the same boundary
//     LayoutRefusedStructural names.
//
// Every unmodelled kustomization field key must appear here.
// TestEveryUnsupportedKustomizeFeatureIsClassified walks kustomize's own Kustomization
// struct and fails when a field outside supportedKustomizationFields is unclassified, so
// a kustomize bump cannot quietly reintroduce PermanenceUnknown.
func kustomizeFeatureClassification() map[string]Classification {
	return map[string]Classification{
		// The author's own file, fixable where it stands.
		featureUnparseable:       {Permanence: PermanenceFixable, Actor: ActorAuthor},
		featureMalformedImages:   {Permanence: PermanenceFixable, Actor: ActorAuthor},
		featureMalformedReplicas: {Permanence: PermanenceFixable, Actor: ActorAuthor},
		featurePatchOutsideTree:  {Permanence: PermanenceFixable, Actor: ActorAuthor},
		featureRenderFailed:      {Permanence: PermanenceFixable, Actor: ActorAuthor},

		// Read-only context, or merge semantics, we could model later. A remote base and an
		// inflated chart are context the operator does not fetch yet, not content it can
		// never own — calling them permanent would repeat the mistake that retired
		// ReasonOverlayFanOutUnsupported.
		featureRemoteBase:             {Permanence: PermanencePending},
		"helmCharts":                  {Permanence: PermanencePending},
		"helmGlobals":                 {Permanence: PermanencePending},
		"helmChartInflationGenerator": {Permanence: PermanencePending},
		"configurations":              {Permanence: PermanencePending},
		"crds":                        {Permanence: PermanencePending},
		"openapi":                     {Permanence: PermanencePending},
		// Deprecated spellings FixKustomization folds before this map is consulted. Seeing
		// one here means kustomize stopped folding it, which is ours to fix, not the
		// author's.
		"bases":     {Permanence: PermanencePending},
		"imageTags": {Permanence: PermanencePending},

		// Build-time synthesis, post-read rewriting, and plugin code: no source document to
		// route an edit back into.
		featurePatchInline:      {Permanence: PermanencePermanent},
		featurePatchJSON6902:    {Permanence: PermanencePermanent},
		"components":            {Permanence: PermanencePermanent},
		"configMapGenerator":    {Permanence: PermanencePermanent},
		"secretGenerator":       {Permanence: PermanencePermanent},
		"generators":            {Permanence: PermanencePermanent},
		"transformers":          {Permanence: PermanencePermanent},
		"replacements":          {Permanence: PermanencePermanent},
		"vars":                  {Permanence: PermanencePermanent},
		"validators":            {Permanence: PermanencePermanent},
		"namePrefix":            {Permanence: PermanencePermanent},
		"nameSuffix":            {Permanence: PermanencePermanent},
		"patchesStrategicMerge": {Permanence: PermanencePermanent},
		"patchesJson6902":       {Permanence: PermanencePermanent},
	}
}

// classifyKustomizeFeatures reduces the unsupported constructs one kustomization declares
// to the single classification the folder's prospects deserve. The most severe answer
// wins: a folder holding both a typo and a configMapGenerator is not fixed by fixing the
// typo, so telling its author to go fix something would be wrong.
//
// An empty or wholly unrecognised feature set degrades to PermanenceUnknown — silence —
// rather than to a guess.
func classifyKustomizeFeatures(features []string) Classification {
	classes := kustomizeFeatureClassification()
	out := Classification{}
	for _, f := range features {
		c, known := classes[f]
		if !known {
			continue
		}
		if permanenceSeverity(c.Permanence) > permanenceSeverity(out.Permanence) {
			out = c
		}
	}
	return out
}

// The order the taxonomy is reduced in when one kustomization declares several
// unsupported constructs: the most severe answer is the honest one, because fixing the
// least severe leaves the folder exactly as unadoptable as it was.
const (
	severityUnknown = iota
	severityFixable
	severityPending
	severityPermanent
)

// permanenceSeverity orders the taxonomy for "most severe wins".
func permanenceSeverity(p Permanence) int {
	switch p {
	case PermanencePermanent:
		return severityPermanent
	case PermanencePending:
		return severityPending
	case PermanenceFixable:
		return severityFixable
	case PermanenceUnknown:
		return severityUnknown
	}
	return severityUnknown
}

// retainedUnsupportedFeatures names the constructs behind one unsupported retention: the
// parsed feature set, plus featureRenderFailed when kustomize could not build the root at
// all. It returns nothing for a supported retention, so the field is empty exactly when
// Unsupported is false.
func retainedUnsupportedFeatures(unsupported bool, doc *kustomizationDoc, buildFailed bool) []string {
	if !unsupported {
		return nil
	}
	var out []string
	if doc != nil {
		out = append(out, doc.features...)
	}
	if buildFailed {
		out = append(out, featureRenderFailed)
	}
	sort.Strings(out)
	return out
}
