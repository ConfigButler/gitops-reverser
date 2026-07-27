// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	kustypes "sigs.k8s.io/kustomize/api/types"

	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// This file is the test the ask is actually about. A permanence that is merely written
// down drifts exactly like the doc comments that caused the original bug: the axis went
// degenerate when a refusal was retired, and nothing said so. Two things are pinned here:
//
//   - every IssueKind constant is classified, discovered from the SOURCE rather than from
//     a list someone remembers to update, so a new kind fails this test; and
//   - every EMISSION PATH is classified, because three kinds answer differently depending
//     on which branch raised them. A per-constant test passes while a new branch quietly
//     emits PermanenceUnknown, which reproduces the original bug one level down.
//
// See docs/design/analyzer-consumer-contract-asks.md (Ask 1).

// classificationByKind is the settled table, in the form this package can check. A kind
// whose answer depends on the branch that raised it lists every branch it can produce, so
// this file never claims a single answer where the code has two.
var classificationByKind = map[IssueKind][]Classification{
	IssueInvalidYAML:            {{Permanence: PermanenceFixable, Actor: ActorAuthor}},
	IssueDuplicate:              {{Permanence: PermanenceFixable, Actor: ActorAuthor}},
	IssueImpureManagedFile:      {{Permanence: PermanenceFixable, Actor: ActorAuthor}},
	IssueMixedFile:              {{Permanence: PermanenceFixable, Actor: ActorAuthor}},
	IssueIgnoreShadowsManaged:   {{Permanence: PermanenceFixable, Actor: ActorAuthor}},
	IssueNonKRM:                 {{Permanence: PermanenceFixable, Actor: ActorAuthor}},
	IssueForeignFile:            {{Permanence: PermanenceFixable, Actor: ActorAuthor}},
	IssueForeignSymlink:         {{Permanence: PermanenceFixable, Actor: ActorAuthor}},
	IssueForeignSubmodule:       {{Permanence: PermanenceFixable, Actor: ActorAuthor}},
	IssueOutOfScope:             {{Permanence: PermanenceFixable, Actor: ActorPlatform}},
	IssueWriteEscapesScope:      {{Permanence: PermanenceFixable, Actor: ActorPlatform}},
	IssueRenderDoesNotMatchLive: {{Permanence: PermanenceFixable, Actor: ActorPlatform}},
	IssueWriteFanIn:             {{Permanence: PermanencePending}},
	IssueUnplaceableEdit:        {{Permanence: PermanencePermanent}},
	IssueRenderRefused:          {{Permanence: PermanencePermanent}},
	// One code, three answers — the case that proves the whole ask.
	IssueUnsupportedKustomize: {
		{Permanence: PermanenceFixable, Actor: ActorAuthor},
		{Permanence: PermanencePending},
		{Permanence: PermanencePermanent},
	},
	// A kind nothing serves is one CRD install away; a kind that is served but ambiguous,
	// denied, or missing a verb is not.
	IssueUnresolvedKRM: {
		{Permanence: PermanenceFixable, Actor: ActorPlatform},
		{Permanence: PermanencePending},
	},
}

// TestEveryIssueKindIsClassified reads the IssueKind constants out of this package's own
// source, so adding a kind and forgetting to classify it fails here rather than reaching a
// consumer as silence. It is the guard that keeps the table above honest.
func TestEveryIssueKindIsClassified(t *testing.T) {
	kinds := issueKindConstants(t)
	if len(kinds) < 15 {
		t.Fatalf("found only %d IssueKind constants (%v); the source scan is broken, not the code",
			len(kinds), kinds)
	}
	for _, kind := range kinds {
		classes, ok := classificationByKind[IssueKind(kind)]
		if !ok {
			t.Errorf("IssueKind %q has no permanence classification: a consumer receiving it can only say "+
				"\"this folder cannot be picked\". Classify it at its raise site and add it here.", kind)
			continue
		}
		for _, c := range classes {
			assertCoherent(t, kind, c)
		}
	}
}

// assertCoherent holds the two invariants every classification must satisfy: it says
// something, and it names an actor exactly when someone can act.
func assertCoherent(t *testing.T, what string, c Classification) {
	t.Helper()
	if c.Permanence == PermanenceUnknown {
		t.Errorf("%q is classified PermanenceUnknown; refusals ship classified", what)
	}
	if c.Permanence == PermanenceFixable && c.Actor == ActorUnknown {
		t.Errorf("%q is fixable but names no actor, so it renders as \"fix your repository\" to "+
			"someone who may not be able to", what)
	}
	if c.Permanence != PermanenceFixable && c.Actor != ActorUnknown {
		t.Errorf("%q names an actor for a refusal nobody can act on", what)
	}
}

// issueKindConstants parses the package's non-test sources and returns the value of every
// constant declared with the IssueKind type. Reflection cannot enumerate constants, and a
// hand-kept list is the thing that drifted in the first place.
func issueKindConstants(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out = append(out, issueKindsInFile(file)...)
	}
	return out
}

// issueKindsInFile collects the string value of every `X IssueKind = "..."` const spec.
func issueKindsInFile(file *ast.File) []string {
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if ok && gen.Tok == token.CONST {
			out = append(out, issueKindsInDecl(gen)...)
		}
	}
	return out
}

// issueKindsInDecl collects the IssueKind-typed constants of one const block.
func issueKindsInDecl(gen *ast.GenDecl) []string {
	var out []string
	for _, spec := range gen.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		if ident, ok := vs.Type.(*ast.Ident); !ok || ident.Name != "IssueKind" {
			continue
		}
		out = append(out, stringLiterals(vs.Values)...)
	}
	return out
}

// stringLiterals unquotes every string-literal expression in values.
func stringLiterals(values []ast.Expr) []string {
	var out []string
	for _, value := range values {
		lit, ok := value.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		if s, err := strconv.Unquote(lit.Value); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// wantIssue is one refusal a case expects, and the classification a consumer must see on
// it. A slice rather than a map keyed by kind: the point is what one emission path
// produces, never a claim about the whole enum.
type wantIssue struct {
	kind  IssueKind
	class Classification
}

// TestAcceptanceEmissionPathsAreClassified drives the real gate over folders that raise
// each structural refusal and asserts what a consumer would actually receive. It is the
// half a per-constant test cannot do: it fails when a NEW branch emits an existing kind
// without classifying it.
func TestAcceptanceEmissionPathsAreClassified(t *testing.T) {
	cases := map[string]struct {
		fsys   fstest.MapFS
		mapper typeset.Lookup
		policy AcceptancePolicy
		want   []wantIssue
	}{
		"invalid yaml": {
			fsys: fstest.MapFS{"broken.yaml": {Data: []byte(brokenYAML)}},
			want: []wantIssue{{
				kind:  IssueInvalidYAML,
				class: Classification{Permanence: PermanenceFixable, Actor: ActorAuthor},
			}},
		},
		"non-krm yaml": {
			fsys: fstest.MapFS{"values.yaml": {Data: []byte(plainYAML)}},
			want: []wantIssue{{
				kind:  IssueNonKRM,
				class: Classification{Permanence: PermanenceFixable, Actor: ActorAuthor},
			}},
		},
		"duplicate identity": {
			fsys: fstest.MapFS{
				"a.yaml": {Data: []byte(configMapCYAML)},
				"b.yaml": {Data: []byte(configMapCYAML)},
			},
			want: []wantIssue{{
				kind:  IssueDuplicate,
				class: Classification{Permanence: PermanenceFixable, Actor: ActorAuthor},
			}},
		},
		"impure managed file": {
			fsys: fstest.MapFS{"mixed.yaml": {Data: []byte(deployYAML + "---\n" + plainYAML)}},
			want: []wantIssue{{
				kind:  IssueImpureManagedFile,
				class: Classification{Permanence: PermanenceFixable, Actor: ActorAuthor},
			}},
		},
		"managed resource in a build directive": {
			fsys:   fstest.MapFS{"kustomization.yaml": {Data: []byte(kustomizationY + "---\n" + deployYAML)}},
			policy: AcceptancePolicy{Allowlist: DefaultAllowlist()},
			want: []wantIssue{{
				kind:  IssueMixedFile,
				class: Classification{Permanence: PermanenceFixable, Actor: ActorAuthor},
			}},
		},
		"foreign file": {
			fsys: fstest.MapFS{
				"deploy.yaml": {Data: []byte(deployYAML)},
				"notes.txt":   {Data: []byte("scratch\n")},
			},
			want: []wantIssue{{
				kind:  IssueForeignFile,
				class: Classification{Permanence: PermanenceFixable, Actor: ActorAuthor},
			}},
		},
		"out of scope": {
			fsys:   fstest.MapFS{"deploy.yaml": {Data: []byte(deployYAML)}},
			mapper: typeset.NewSnapshotRegistry(sampleClusterSnapshot()),
			policy: AcceptancePolicy{
				InScope: func(ri types.ResourceIdentifier) bool { return ri.Namespace == "kube-system" },
			},
			want: []wantIssue{{
				kind:  IssueOutOfScope,
				class: Classification{Permanence: PermanenceFixable, Actor: ActorPlatform},
			}},
		},
		// The two unresolved-krm branches, which is the split the raise site exists to
		// record: a Widget nothing serves is a CRD the platform operator can install; a
		// Secret the policy denies is not something anyone clears from here.
		"unresolved krm, kind not served": {
			fsys:   fstest.MapFS{"w.yaml": {Data: []byte(widgetYAMLDoc)}},
			mapper: typeset.NewSnapshotRegistry(typeset.Snapshot{Generation: 1}),
			want: []wantIssue{{
				kind:  IssueUnresolvedKRM,
				class: Classification{Permanence: PermanenceFixable, Actor: ActorPlatform},
			}},
		},
		"unresolved krm, kind served but not followable": {
			fsys:   fstest.MapFS{"secret.yaml": {Data: []byte(plainSecretYAML)}},
			mapper: typeset.NewSnapshotRegistry(sampleClusterSnapshot()),
			want: []wantIssue{{
				kind:  IssueUnresolvedKRM,
				class: Classification{Permanence: PermanencePending},
			}},
		},
		// The three unsupported-kustomize branches, through the real gate.
		"unsupported kustomize, the author's own file": {
			fsys: fstest.MapFS{
				"kustomization.yaml": {Data: []byte("apiVersion: kustomize.config.k8s.io/v1beta1\n" +
					"kind: Kustomization\nresources: [\n")},
			},
			policy: AcceptancePolicy{Allowlist: DefaultAllowlist()},
			want: []wantIssue{{
				kind:  IssueUnsupportedKustomize,
				class: Classification{Permanence: PermanenceFixable, Actor: ActorAuthor},
			}},
		},
		"unsupported kustomize, not supported yet": {
			fsys: fstest.MapFS{
				"kustomization.yaml": {Data: []byte("apiVersion: kustomize.config.k8s.io/v1beta1\n" +
					"kind: Kustomization\nhelmCharts:\n  - name: podinfo\n    repo: https://example.com\n")},
			},
			policy: AcceptancePolicy{Allowlist: DefaultAllowlist()},
			want: []wantIssue{{
				kind:  IssueUnsupportedKustomize,
				class: Classification{Permanence: PermanencePending},
			}},
		},
		"unsupported kustomize, the support boundary": {
			fsys: fstest.MapFS{
				"kustomization.yaml": {Data: []byte("apiVersion: kustomize.config.k8s.io/v1beta1\n" +
					"kind: Kustomization\nconfigMapGenerator:\n  - name: settings\n    literals:\n      - a=b\n")},
			},
			policy: AcceptancePolicy{Allowlist: DefaultAllowlist()},
			want: []wantIssue{{
				kind:  IssueUnsupportedKustomize,
				class: Classification{Permanence: PermanencePermanent},
			}},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := buildStoreFS(context.Background(), tc.fsys, tc.mapper, tc.policy.Allowlist)
			acc := Accept(store, tc.policy)
			if acc.Accepted {
				t.Fatalf("expected a refusal to classify, got an accepted folder")
			}
			for _, want := range tc.want {
				assertIssueClassified(t, acc, want)
			}
		})
	}
}

// assertIssueClassified asserts the gate raised the expected kind, and that a consumer
// reading it is told what it can do about it.
func assertIssueClassified(t *testing.T, acc Acceptance, want wantIssue) {
	t.Helper()
	for _, issue := range acc.Issues {
		if issue.Kind != want.kind {
			continue
		}
		if got := (Classification{Permanence: issue.Permanence, Actor: issue.Actor}); got != want.class {
			t.Errorf("%s at %s: classification = %+v, want %+v", issue.Kind, issue.Path, got, want.class)
		}
		return
	}
	t.Fatalf("expected a %s refusal, got %+v", want.kind, acc.Issues)
}

// TestForeignEntryRefusalsAreClassified covers the symlink and submodule raise sites,
// which no in-memory filesystem can produce. They are the rule the whole table follows:
// permanence classifies the FOLDER's prospects, not the rule's. We will never follow a
// symlink, yet the folder in front of the reader is one `git rm` from being adoptable.
func TestForeignEntryRefusalsAreClassified(t *testing.T) {
	store := &ManifestStore{Foreign: []ForeignEntry{
		{Kind: ForeignSymlink, Path: "link.yaml"},
		{Kind: ForeignSubmodule, Path: "vendor"},
		{Kind: ForeignFile, Path: "notes.txt"},
	}}
	issues := foreignContentRefusals(store)
	if len(issues) != 3 {
		t.Fatalf("want three foreign refusals, got %+v", issues)
	}
	want := Classification{Permanence: PermanenceFixable, Actor: ActorAuthor}
	for _, issue := range issues {
		if got := (Classification{Permanence: issue.Permanence, Actor: issue.Actor}); got != want {
			t.Errorf("%s at %s: classification = %+v, want %+v", issue.Kind, issue.Path, got, want)
		}
	}
}

// TestIgnoreShadowsManagedRefusalIsClassified covers the .gittargetignore parse-time
// raise site.
func TestIgnoreShadowsManagedRefusalIsClassified(t *testing.T) {
	_, issues := LoadGitTargetIgnore([]byte("*\n"))
	if len(issues) != 1 || issues[0].Kind != IssueIgnoreShadowsManaged {
		t.Fatalf("want one ignore-shadows-managed refusal, got %+v", issues)
	}
	want := Classification{Permanence: PermanenceFixable, Actor: ActorAuthor}
	if got := (Classification{Permanence: issues[0].Permanence, Actor: issues[0].Actor}); got != want {
		t.Errorf("classification = %+v, want %+v", got, want)
	}
}

// TestEveryUnsupportedKustomizeFeatureIsClassified walks kustomize's OWN type: every
// field outside the modelled subset can refuse a folder by name, so every one of them
// must carry an answer. A kustomize bump that adds a field fails here instead of shipping
// a refusal that says nothing.
func TestEveryUnsupportedKustomizeFeatureIsClassified(t *testing.T) {
	modelled := supportedKustomizationFields()
	classes := kustomizeFeatureClassification()
	kt := reflect.TypeOf(kustypes.Kustomization{})
	for i := range kt.NumField() {
		field := kt.Field(i)
		if _, ok := modelled[field.Name]; ok {
			continue
		}
		key := kustomizationFieldKey(field)
		if _, ok := classes[key]; !ok {
			t.Errorf("unmodelled kustomization field %q (%s) has no permanence: it would refuse a folder "+
				"with no answer to \"can this ever be picked\".", key, field.Name)
		}
	}
	for _, f := range []string{
		featureUnparseable, featureRemoteBase, featureMalformedImages, featureMalformedReplicas,
		featurePatchInline, featurePatchJSON6902, featurePatchOutsideTree, featureRenderFailed,
	} {
		if _, ok := classes[f]; !ok {
			t.Errorf("feature %q has no permanence classification", f)
		}
	}
	for f, c := range classes {
		assertCoherent(t, f, c)
	}
}

// TestClassifyKustomizeFeatures_MostSevereWins pins the reduction rule. A folder holding
// both a typo and a generator is not fixed by fixing the typo, so telling its author to go
// fix something would be a confident wrong sentence.
func TestClassifyKustomizeFeatures_MostSevereWins(t *testing.T) {
	fixable := Classification{Permanence: PermanenceFixable, Actor: ActorAuthor}
	pending := Classification{Permanence: PermanencePending}
	permanent := Classification{Permanence: PermanencePermanent}

	cases := map[string]struct {
		features []string
		want     Classification
	}{
		"nothing at all":       {nil, Classification{}},
		"unrecognised only":    {[]string{"someFutureField"}, Classification{}},
		"the author's file":    {[]string{featureUnparseable}, fixable},
		"not yet":              {[]string{featureRemoteBase}, pending},
		"the boundary":         {[]string{"configMapGenerator"}, permanent},
		"fixable and pending":  {[]string{featureUnparseable, featureRemoteBase}, pending},
		"pending and boundary": {[]string{featureRemoteBase, "vars"}, permanent},
		"fixable and unknown":  {[]string{"someFutureField", featureUnparseable}, fixable},
		"boundary over all":    {[]string{featureUnparseable, featureRemoteBase, "replacements"}, permanent},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := classifyKustomizeFeatures(tc.features); got != tc.want {
				t.Errorf("classifyKustomizeFeatures(%v) = %+v, want %+v", tc.features, got, tc.want)
			}
		})
	}
}

// TestIssuesToReasons_CarriesClassification pins the projection a consumer of ScanRepo
// actually reads. issuesToReasons is the single choke point through which every issue
// becomes a refusal reason, so a check that classifies itself must arrive intact.
func TestIssuesToReasons_CarriesClassification(t *testing.T) {
	reasons := issuesToReasons([]AcceptanceIssue{{
		Kind:       IssueOutOfScope,
		Path:       "apps/web/deploy.yaml",
		Message:    "out of scope",
		Permanence: PermanenceFixable,
		Actor:      ActorPlatform,
	}})
	if len(reasons) != 1 {
		t.Fatalf("want one reason, got %+v", reasons)
	}
	if reasons[0].Permanence != PermanenceFixable || reasons[0].Actor != ActorPlatform {
		t.Errorf("reason = %+v, want the issue's own classification", reasons[0])
	}
}

// refusedStructuralReasonIn returns the first refused-structural reason in a report.
func refusedStructuralReasonIn(rep RepoReport) (RefusalReason, bool) {
	for _, cand := range rep.Candidates {
		for _, reason := range cand.RefusalReasons {
			if reason.Code == ReasonRefusedStructural {
				return reason, true
			}
		}
	}
	return RefusalReason{}, false
}

// TestRefusedStructuralIsClassified pins the one refusal reason that is not an IssueKind.
// It reads as the support boundary, and stays permanent whenever the constructs behind it
// are unknown — but a root refused for a construct we could support later says so, because
// the gate says so for the very same folder when a NESTED kustomization declares it. Two
// answers about one directory is the bug this whole document is about.
func TestRefusedStructuralIsClassified(t *testing.T) {
	const kustHeader = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deploy.yaml\n"

	cases := map[string]struct {
		kustomization string
		want          Classification
	}{
		"a generator is the boundary": {
			kustomization: kustHeader + "configMapGenerator:\n  - name: settings\n    literals:\n      - a=b\n",
			want:          Classification{Permanence: PermanencePermanent},
		},
		"helm inflation is not supported yet": {
			kustomization: kustHeader + "helmCharts:\n  - name: podinfo\n    repo: https://example.com\n",
			want:          Classification{Permanence: PermanencePending},
		},
		"an unparseable build file is the author's to fix": {
			kustomization: "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [\n",
			want:          Classification{Permanence: PermanenceFixable, Actor: ActorAuthor},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fsys := fstest.MapFS{
				"app/kustomization.yaml": {Data: []byte(tc.kustomization)},
				"app/deploy.yaml":        {Data: []byte(deployYAML)},
			}
			rep := scanRepoFS(context.Background(), fsys)

			reason, ok := refusedStructuralReasonIn(rep)
			if !ok {
				t.Fatalf("expected a refused-structural candidate, got %+v", rep.Candidates)
			}
			if got := (Classification{Permanence: reason.Permanence, Actor: reason.Actor}); got != tc.want {
				t.Errorf("refused-structural reason = %+v, want %+v", reason, tc.want)
			}
		})
	}
}
