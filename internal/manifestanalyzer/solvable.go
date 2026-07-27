// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import "sort"

// Actor names who can solve a refusal. It is empty unless the refusal is Solvable —
// naming someone for a refusal they cannot act on is worse than naming nobody.
//
// It exists because some refusals are solvable ONLY by the person who owns the GitTarget,
// who is usually not the person reading the message. Rendering an out-of-scope refusal as
// "fix your repository" to a repository author who cannot is the failure this half
// prevents.
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
