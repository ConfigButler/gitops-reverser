// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"sort"
	"strings"
)

// Actor names who can solve a refusal. It is empty unless the refusal is Solvable —
// naming someone for a refusal they cannot act on is worse than naming nobody.
//
// It exists because some refusals are solvable ONLY by the person who owns the GitTarget,
// who is usually not the person reading the message. Rendering an out-of-scope refusal as
// "fix your repository" to a repository author who cannot is the failure this half
// prevents.
//
// # Which scan can report which value
//
// CLUSTER-AWARENESS is the gate, and it is structural rather than incidental. A
// STRUCTURE-ONLY scan — one whose [typeset.Lookup] is not ready, which is every
// ScanFolder and ScanRepo — reports only [ActorUnknown] or [ActorRepositoryAuthor]. It
// cannot reach [ActorPlatformOperator] through either of that value's two acceptance
// sites: [IssueOutOfScope] needs a declared AcceptancePolicy.InScope, and
// [IssueUnresolvedKRM] needs MappingNotFollowable, which a not-ready registry never
// produces because it resolves every document to MappingNoSource instead.
//
// So a consumer of a structure-only report has a platform-operator branch that never
// fires, and that is by design: a scan that cannot see the cluster cannot know a CRD is
// missing or a GitTarget's scope is narrow. Only a cluster-aware scan and the live write
// path name the platform operator. TestStructureOnlyScanNeverNamesThePlatformOperator
// pins it.
type Actor string

const (
	// ActorUnknown is the zero value: nobody can solve this refusal, or the check did not
	// say who.
	ActorUnknown Actor = ""
	// ActorRepositoryAuthor is the person who owns the files in the repository.
	ActorRepositoryAuthor Actor = "repository-author"
	// ActorPlatformOperator is the person who owns the GitTarget — its scope, its path,
	// and the CRDs installed in the cluster it mirrors.
	ActorPlatformOperator Actor = "platform-operator"
)

// Classification is the pair a raise site attaches to a refusal: can it be solved, and by
// whom. Passing them together keeps a solvable refusal from losing the actor half on its
// way out of the check.
type Classification struct {
	Solvable bool
	Actor    Actor
}

// classified stamps a computed classification onto an issue. Raise sites with one fixed
// answer set Solvable and Actor in the literal instead; this exists for the sites whose
// answer depends on which branch fired.
func (i AcceptanceIssue) classified(c Classification) AcceptanceIssue {
	i.Solvable, i.Actor = c.Solvable, c.Actor
	return i
}

// kustomizeFeatureClassification maps every unsupported kustomization construct to its own
// answer. This is the map the ask exists for: `unsupported-kustomize` is one code with two
// very different answers, so classifying it per code would tell most of the people who hit
// it something false.
//
// The split is whether a person can do something about it today:
//
//   - solvable by the author — the file itself is broken, or reaches outside the tree. No
//     support boundary is in play; one commit clears it.
//   - not solvable — content synthesised at build time, fields rewritten after the source
//     is read, plugin code, or context this release does not fetch. Nothing the author or
//     the platform operator does makes the folder adoptable while it declares them.
//
// Every unmodelled kustomization field key must appear here.
// TestEveryUnsupportedKustomizeFeatureIsClassified walks kustomize's own Kustomization
// struct and fails when a field outside supportedKustomizationFields is missing, so a
// kustomize bump cannot quietly add a construct that refuses a folder with no answer.
func kustomizeFeatureClassification() map[string]Classification {
	return map[string]Classification{
		// The author's own file, solvable where it stands.
		featureUnparseable:       {Solvable: true, Actor: ActorRepositoryAuthor},
		featureMalformedImages:   {Solvable: true, Actor: ActorRepositoryAuthor},
		featureMalformedReplicas: {Solvable: true, Actor: ActorRepositoryAuthor},
		featurePatchOutsideTree:  {Solvable: true, Actor: ActorRepositoryAuthor},
		featureRenderFailed:      {Solvable: true, Actor: ActorRepositoryAuthor},

		// Read-only context this release does not fetch, and merge semantics it does not
		// model. Nobody can make the folder adoptable while it declares them.
		featureRemoteBase:             {Solvable: false},
		"helmCharts":                  {Solvable: false},
		"helmGlobals":                 {Solvable: false},
		"helmChartInflationGenerator": {Solvable: false},
		"configurations":              {Solvable: false},
		"crds":                        {Solvable: false},
		"openapi":                     {Solvable: false},
		// Deprecated spellings FixKustomization folds before this map is consulted. Seeing
		// one here means kustomize stopped folding it, which is ours to fix, not the
		// author's — but it is still not solvable from the repository today.
		"bases":     {Solvable: false},
		"imageTags": {Solvable: false},

		// Build-time synthesis, post-read rewriting, and plugin code: no source document
		// to route an edit back into.
		featurePatchInline:      {Solvable: false},
		featurePatchJSON6902:    {Solvable: false},
		"components":            {Solvable: false},
		"configMapGenerator":    {Solvable: false},
		"secretGenerator":       {Solvable: false},
		"generators":            {Solvable: false},
		"transformers":          {Solvable: false},
		"replacements":          {Solvable: false},
		"vars":                  {Solvable: false},
		"validators":            {Solvable: false},
		"namePrefix":            {Solvable: false},
		"nameSuffix":            {Solvable: false},
		"patchesStrategicMerge": {Solvable: false},
		"patchesJson6902":       {Solvable: false},
	}
}

// classifyKustomizeFeatures reduces the unsupported constructs one kustomization declares
// to the single answer the folder deserves: solvable only when EVERY construct is. A
// folder holding both a typo and a configMapGenerator is not adoptable once the typo is
// fixed, so telling its author to go fix something would be a confident wrong sentence.
//
// A feature set with nothing recognised in it reports not solvable, which is the
// conservative claim: it sends nobody off to fix something they cannot. The classification
// tests keep that case unreachable.
func classifyKustomizeFeatures(features []string) Classification {
	classes := kustomizeFeatureClassification()
	out := Classification{}
	for _, f := range features {
		c, known := classes[f]
		if !known {
			return Classification{}
		}
		if !c.Solvable {
			return Classification{}
		}
		out = c
	}
	return out
}

// unsupportedKustomizeDetail describes an unsupported kustomization with a stem that AGREES
// with the classification beside it.
//
// It takes the classification rather than recomputing one because the two sentences a reader
// gets — "solvable: true" and the prose — came from one input and must not drift. They did
// drift: one stem served both branches, so a folder whose only fault was a typo in its
// kustomization.yaml was told it "uses unsupported feature(s): unparseable", which is the
// opposite of what Solvable said about it, and it is the sentence a consumer shipped to real
// users as "this can never be synced".
//
// The not-solvable stem is unchanged, deliberately: it is the one that was true, it is what
// the corpus baseline records for a dozen fixtures, and leaving it alone keeps the diff of
// this change to exactly the refusals that were lying.
func unsupportedKustomizeDetail(class Classification, features []string, decodeErr string) string {
	if len(features) == 0 {
		return "kustomization uses an unsupported feature the operator cannot map back to editable source"
	}
	stem := "kustomization uses unsupported feature(s): "
	if class.Solvable {
		// Every solvable construct is a fault in the file or a path reaching out of the
		// tree, so "fixed in the repository" holds for all five without naming a person
		// the classification may not have named.
		stem = "kustomization has a fault that can be fixed in the repository: "
	}
	detail := stem + strings.Join(features, ", ")
	if decodeErr != "" {
		// "unparseable" on its own says nothing a user can act on. kustomize's decoder
		// knows exactly what is wrong — that a resources: is a string, or that the file
		// is a Flux Kustomization CR rather than a build file — so quote it.
		detail += " (" + decodeErr + ")"
	}
	return detail
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
