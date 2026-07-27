// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import "sort"

// Solvability answers one question about a refusal: can it be solved? It is set by the
// check that raised the refusal, because only that check knows — the same code answers
// differently depending on which branch emitted it.
//
// It is a statement about THIS RELEASE, never a promise about the future. "Nobody can
// solve this today" is a fact we can stand behind; "we may support it later" is a roadmap,
// and a roadmap in a machine-readable field ages into a lie in one direction or the other.
// A refusal a later release learns to accept simply reports differently on the next scan,
// because a consumer reads this value rather than a table it cached.
//
// Consumers MUST treat an unrecognised or absent value as SolvabilityUnknown and say
// nothing about whether the refusal can be solved.
//
// See docs/design/analyzer-consumer-contract-asks.md (Ask 1).
type Solvability string

const (
	// SolvabilityUnknown is the zero value: this refusal was not classified, so say
	// nothing about whether it can be solved.
	SolvabilityUnknown Solvability = ""
	// SolvabilityYes means someone can solve it with the current release, by changing the
	// repository or the GitTarget. Actor says who.
	SolvabilityYes Solvability = "yes"
	// SolvabilityNo means nobody can solve it with the current release: the construct is
	// outside the adoption model, or the support is simply not there. Either way the
	// folder cannot be adopted as it stands, and no repository or platform change alters
	// that. It carries no Actor, because there is nobody to name.
	SolvabilityNo Solvability = "no"
)

// Actor names who can solve a refusal. It is empty unless Solvability is SolvabilityYes:
// naming someone for a refusal they cannot act on is worse than naming nobody.
//
// It exists because some refusals are solvable ONLY by the person who owns the GitTarget,
// who is usually not the person reading the message. Rendering an out-of-scope refusal as
// "fix your repository" to a repository author who cannot is the failure this half
// prevents.
type Actor string

const (
	// ActorUnknown is the zero value: nobody can solve this refusal today, or it was not
	// classified.
	ActorUnknown Actor = ""
	// ActorRepositoryAuthor is the person who owns the files in the repository.
	ActorRepositoryAuthor Actor = "repository-author"
	// ActorPlatformOperator is the person who owns the GitTarget — its scope, its path,
	// and the CRDs installed in the cluster it mirrors.
	ActorPlatformOperator Actor = "platform-operator"
)

// Classification is the pair a raise site attaches to a refusal. Passing them together
// keeps a solvable refusal from losing the actor half on its way out of the check.
type Classification struct {
	Solvability Solvability
	Actor       Actor
}

// classified stamps a computed classification onto an issue. Raise sites with one fixed
// answer set Solvability and Actor in the literal instead; this exists for the sites whose
// answer depends on which branch fired.
func (i AcceptanceIssue) classified(c Classification) AcceptanceIssue {
	i.Solvability, i.Actor = c.Solvability, c.Actor
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
// struct and fails when a field outside supportedKustomizationFields is unclassified, so a
// kustomize bump cannot quietly reintroduce SolvabilityUnknown.
func kustomizeFeatureClassification() map[string]Classification {
	return map[string]Classification{
		// The author's own file, solvable where it stands.
		featureUnparseable:       {Solvability: SolvabilityYes, Actor: ActorRepositoryAuthor},
		featureMalformedImages:   {Solvability: SolvabilityYes, Actor: ActorRepositoryAuthor},
		featureMalformedReplicas: {Solvability: SolvabilityYes, Actor: ActorRepositoryAuthor},
		featurePatchOutsideTree:  {Solvability: SolvabilityYes, Actor: ActorRepositoryAuthor},
		featureRenderFailed:      {Solvability: SolvabilityYes, Actor: ActorRepositoryAuthor},

		// Read-only context this release does not fetch, and merge semantics it does not
		// model. Nobody can make the folder adoptable while it declares them.
		featureRemoteBase:             {Solvability: SolvabilityNo},
		"helmCharts":                  {Solvability: SolvabilityNo},
		"helmGlobals":                 {Solvability: SolvabilityNo},
		"helmChartInflationGenerator": {Solvability: SolvabilityNo},
		"configurations":              {Solvability: SolvabilityNo},
		"crds":                        {Solvability: SolvabilityNo},
		"openapi":                     {Solvability: SolvabilityNo},
		// Deprecated spellings FixKustomization folds before this map is consulted. Seeing
		// one here means kustomize stopped folding it, which is ours to fix, not the
		// author's — but it is still not solvable from the repository today.
		"bases":     {Solvability: SolvabilityNo},
		"imageTags": {Solvability: SolvabilityNo},

		// Build-time synthesis, post-read rewriting, and plugin code: no source document
		// to route an edit back into.
		featurePatchInline:      {Solvability: SolvabilityNo},
		featurePatchJSON6902:    {Solvability: SolvabilityNo},
		"components":            {Solvability: SolvabilityNo},
		"configMapGenerator":    {Solvability: SolvabilityNo},
		"secretGenerator":       {Solvability: SolvabilityNo},
		"generators":            {Solvability: SolvabilityNo},
		"transformers":          {Solvability: SolvabilityNo},
		"replacements":          {Solvability: SolvabilityNo},
		"vars":                  {Solvability: SolvabilityNo},
		"validators":            {Solvability: SolvabilityNo},
		"namePrefix":            {Solvability: SolvabilityNo},
		"nameSuffix":            {Solvability: SolvabilityNo},
		"patchesStrategicMerge": {Solvability: SolvabilityNo},
		"patchesJson6902":       {Solvability: SolvabilityNo},
	}
}

// classifyKustomizeFeatures reduces the unsupported constructs one kustomization declares
// to the single answer the folder deserves. The LEAST solvable one wins: a folder holding
// both a typo and a configMapGenerator is not adoptable once the typo is fixed, so telling
// its author to go fix something would be a confident wrong sentence.
//
// An empty or wholly unrecognised feature set degrades to SolvabilityUnknown — silence —
// rather than to a guess.
func classifyKustomizeFeatures(features []string) Classification {
	classes := kustomizeFeatureClassification()
	out := Classification{}
	for _, f := range features {
		c, known := classes[f]
		if !known {
			continue
		}
		if lessSolvable(c.Solvability, out.Solvability) {
			out = c
		}
	}
	return out
}

// How solvable each answer is, most solvable first. Unknown ranks below a real answer, so
// silence never wins over something a check actually decided.
const (
	rankUnknown = iota
	rankYes
	rankNo
)

// lessSolvable reports whether a is a less solvable answer than b — the reduction rule for
// a refusal raised by several constructs at once.
func lessSolvable(a, b Solvability) bool {
	return solvabilityRank(a) > solvabilityRank(b)
}

// solvabilityRank orders the answers for "least solvable wins".
func solvabilityRank(s Solvability) int {
	switch s {
	case SolvabilityNo:
		return rankNo
	case SolvabilityYes:
		return rankYes
	case SolvabilityUnknown:
		return rankUnknown
	}
	return rankUnknown
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
