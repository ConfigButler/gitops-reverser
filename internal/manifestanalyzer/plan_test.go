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
	"context"
	"testing"
	"testing/fstest"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/mapping"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// planFS is a duplicate-free tree (deploy.yaml, cm.yaml with two ConfigMaps, and an
// encrypted Secret). The sample tree's dup.yaml would collide the Deployment
// identity, which acceptance refuses — so the core plan cases use this clean tree
// and the duplicate behavior gets its own focused test.
func planFS() fstest.MapFS {
	return fstest.MapFS{
		"deploy.yaml":      {Data: []byte(deployYAML)},
		"cm.yaml":          {Data: []byte(configMapsYAML)},
		"secret.sops.yaml": {Data: []byte(sopsSecretYAML)},
	}
}

// planFiles returns planFS as the manifestedit.FileContent slice BuildPlan hydrates
// from, so a plan and the store it plans over read the exact same bytes.
func planFiles() []manifestedit.FileContent {
	fsys := planFS()
	var files []manifestedit.FileContent
	for _, p := range []string{"deploy.yaml", "cm.yaml", "secret.sops.yaml"} {
		files = append(files, manifestedit.FileContent{Path: p, Content: fsys[p].Data})
	}
	return files
}

// planStore builds the clean tree against the ready static snapshot (the Deployment
// and ConfigMaps are watched/resolved; the Secret is served but disallowed).
func planStore(t *testing.T) *ManifestStore {
	t.Helper()
	mapper := mapping.NewStaticSnapshotMapper(sampleClusterSnapshot())
	return BuildStore(context.Background(), planFS(), mapper)
}

// obj builds an unstructured live object with the given identity and an optional
// spec/data payload merged in.
func obj(apiVersion, kind, namespace, name string, extra map[string]interface{}) *unstructured.Unstructured {
	o := map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
	}
	for k, v := range extra {
		o[k] = v
	}
	return &unstructured.Unstructured{Object: o}
}

func deployWeb(replicas int64) *unstructured.Unstructured {
	return obj("apps/v1", "Deployment", "default", "web",
		map[string]interface{}{"spec": map[string]interface{}{"replicas": replicas}})
}

func configMap(name string) *unstructured.Unstructured {
	return obj("v1", "ConfigMap", "default", name, nil)
}

// desiredDeployWeb / desiredConfigMap / desiredSecret pair a live object with the
// resolved API identity the controller already knows from the GVR it watched.
func desiredDeployWeb(replicas int64) DesiredResource {
	return DesiredResource{
		Resource: types.NewResourceIdentifier("apps", "v1", "deployments", "default", "web"),
		Object:   deployWeb(replicas),
	}
}

func desiredConfigMap(name string) DesiredResource {
	return DesiredResource{
		Resource: types.NewResourceIdentifier("", "v1", "configmaps", "default", name),
		Object:   configMap(name),
	}
}

func desiredSecret() DesiredResource {
	return DesiredResource{
		Resource: types.NewResourceIdentifier("", "v1", "secrets", "default", "db"),
		Object:   obj("v1", "Secret", "default", "db", nil),
	}
}

// inSync is the desired set that matches planFS byte-for-semantics, so adding one
// more resource isolates a single action.
func inSync() []DesiredResource {
	return []DesiredResource{desiredDeployWeb(1), desiredConfigMap("a"), desiredConfigMap("b")}
}

// findAction returns the single action targeting the given file (the files queried
// here hold one document each), or fails.
func findAction(t *testing.T, plan Plan, path string) PlanAction {
	t.Helper()
	for _, a := range plan.Actions {
		if a.Ref.FilePath == path {
			return a
		}
	}
	t.Fatalf("no action at %s; actions=%+v", path, plan.Actions)
	return PlanAction{}
}

// TestBuildPlan_Patch: a desired Deployment that differs from Git is a patch
// carrying the resolved resource identity and the desired object.
func TestBuildPlan_Patch(t *testing.T) {
	store := planStore(t)
	// ConfigMaps in sync, Deployment differs.
	desired := []DesiredResource{desiredConfigMap("a"), desiredConfigMap("b"), desiredDeployWeb(3)}
	plan := BuildPlan(store, planFiles(), desired, Policy{})

	if len(plan.Actions) != 1 {
		t.Fatalf("want exactly one patch, got %+v", plan.Actions)
	}
	patch := findAction(t, plan, "deploy.yaml")
	if patch.Kind != PlanPatch {
		t.Errorf("deploy.yaml#0 kind = %q, want patch", patch.Kind)
	}
	wantRI := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "web")
	if patch.Resource != wantRI {
		t.Errorf("patch Resource = %+v, want %+v", patch.Resource, wantRI)
	}
	if patch.Desired == nil {
		t.Errorf("patch action should carry the desired object")
	}
}

// TestBuildPlan_Create: a desired resource with no document in Git is a create that
// carries the resolved resource identity (so M7 can place the new file via
// ResourceIdentifier.ToGitPath without re-resolving the mapping), while the
// resources already in sync produce no action.
func TestBuildPlan_Create(t *testing.T) {
	store := planStore(t)
	desired := append(inSync(), desiredConfigMap("c")) // c is brand new
	plan := BuildPlan(store, planFiles(), desired, Policy{})

	if len(plan.Actions) != 1 {
		t.Fatalf("actions = %+v, want exactly one create", plan.Actions)
	}
	create := plan.Actions[0]
	if create.Kind != PlanCreate {
		t.Fatalf("kind = %q, want create", create.Kind)
	}
	if create.Identity.Name != "c" || create.Ref != (RecordRef{}) {
		t.Errorf("create = %+v, want identity c with a zero Ref", create)
	}
	wantRI := types.NewResourceIdentifier("", "v1", "configmaps", "default", "c")
	if create.Resource != wantRI {
		t.Errorf("create Resource = %+v, want %+v (placement needs the GVR)", create.Resource, wantRI)
	}
	if create.Desired == nil {
		t.Errorf("create action should carry the desired object")
	}
	if got := create.Resource.ToGitPath(); got != "v1/configmaps/default/c.yaml" {
		t.Errorf("create placement = %q, want v1/configmaps/default/c.yaml", got)
	}
}

// TestBuildPlan_NoOp: every desired resource matching Git yields no actions at all.
func TestBuildPlan_NoOp(t *testing.T) {
	store := planStore(t)
	plan := BuildPlan(store, planFiles(), inSync(), Policy{})
	if len(plan.Actions) != 0 {
		t.Fatalf("in-sync plan should have no actions, got %+v", plan.Actions)
	}
}

// TestBuildPlan_DropOrphans: with an empty desired set every watched, resolved
// document is a managed drop, while the disallowed Secret is left untouched.
func TestBuildPlan_DropOrphans(t *testing.T) {
	store := planStore(t)
	plan := BuildPlan(store, planFiles(), nil, Policy{})

	counts := plan.Counts()
	if counts[PlanDropOrphan] != 3 || len(plan.Actions) != 3 {
		t.Fatalf("counts=%v actions=%d, want exactly 3 drop-orphan", counts, len(plan.Actions))
	}
	for _, a := range plan.Actions {
		if a.Ref.FilePath == "secret.sops.yaml" {
			t.Errorf("disallowed Secret must not be dropped: %+v", a)
		}
	}
	drop := findAction(t, plan, "deploy.yaml")
	wantRI := types.NewResourceIdentifier("apps", "v1", "deployments", "default", "web")
	if drop.Kind != PlanDropOrphan || drop.Resource != wantRI {
		t.Errorf("deploy.yaml#0 = %+v, want drop-orphan with %+v", drop, wantRI)
	}
}

// TestBuildPlan_SkipEncrypted: a desired object that matches an encrypted document
// is a skip — encrypted documents are authoritative but never patched in place.
func TestBuildPlan_SkipEncrypted(t *testing.T) {
	store := planStore(t)
	desired := append(inSync(), desiredSecret())
	plan := BuildPlan(store, planFiles(), desired, Policy{})

	skip := findAction(t, plan, "secret.sops.yaml")
	if skip.Kind != PlanSkip {
		t.Fatalf("secret.sops.yaml#0 kind = %q, want skip", skip.Kind)
	}
	if skip.Resource != (types.NewResourceIdentifier("", "v1", "secrets", "default", "db")) {
		t.Errorf("skip should still carry the desired resource identity, got %+v", skip.Resource)
	}
	if skip.Desired != nil {
		t.Errorf("skip action should not carry a desired object")
	}
}

// TestBuildPlan_DuplicateSuppressed proves Finding 2: a duplicate-identity collision
// produces NO plan action for either copy — not the loser and not the
// first-occurrence winner — because acceptance (M4) refuses the whole folder.
func TestBuildPlan_DuplicateSuppressed(t *testing.T) {
	fsys := fstest.MapFS{
		"deploy.yaml": {Data: []byte(deployYAML)},
		"dup.yaml":    {Data: []byte(deployYAML)}, // same Deployment/default/web identity
	}
	files := []manifestedit.FileContent{
		{Path: "deploy.yaml", Content: []byte(deployYAML)},
		{Path: "dup.yaml", Content: []byte(deployYAML)},
	}
	store := BuildStore(context.Background(), fsys, mapping.NewStaticSnapshotMapper(sampleClusterSnapshot()))

	// A desired update to the collided identity is suppressed: the winner is not
	// patched.
	if plan := BuildPlan(store, files, []DesiredResource{desiredDeployWeb(3)}, Policy{}); len(plan.Actions) != 0 {
		t.Errorf("collided identity should produce no action on update, got %+v", plan.Actions)
	}

	// An empty desired set does not drop the collided identity either.
	if plan := BuildPlan(store, files, nil, Policy{}); len(plan.Actions) != 0 {
		t.Errorf("collided identity should produce no drop, got %+v", plan.Actions)
	}
}

// TestBuildPlan_StructureOnlyNeverDrops proves the no-cluster promise: a
// structure-only store resolves no mappings, so even an empty desired set drops
// nothing — yet a desired object that differs from Git is still a patch, because
// manifest identity (and Decide) need no cluster.
func TestBuildPlan_StructureOnlyNeverDrops(t *testing.T) {
	store := BuildStore(context.Background(), planFS(), nil)

	if plan := BuildPlan(store, planFiles(), nil, Policy{}); len(plan.Actions) != 0 {
		t.Fatalf("structure-only plan should never drop, got %+v", plan.Actions)
	}

	plan := BuildPlan(store, planFiles(), []DesiredResource{desiredDeployWeb(5)}, Policy{})
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != PlanPatch {
		t.Fatalf("structure-only differing object should patch, got %+v", plan.Actions)
	}
}

// TestBuildPlan_NonEditableConstructSkips: a document using a disallowed construct
// (a YAML anchor/alias) does not claim its identity, so a matching desired object is
// not a create and the document surfaces as a single skip, never a drop.
func TestBuildPlan_NonEditableConstructSkips(t *testing.T) {
	const anchoredYAML = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n" +
		"  name: anchored\n  namespace: default\ndata: &d\n  k: v\nextra: *d\n"
	fsys := fstest.MapFS{"anchored.yaml": {Data: []byte(anchoredYAML)}}
	files := []manifestedit.FileContent{{Path: "anchored.yaml", Content: []byte(anchoredYAML)}}
	store := BuildStore(context.Background(), fsys, mapping.NewStaticSnapshotMapper(sampleClusterSnapshot()))

	dm := store.FilesByPath["anchored.yaml"].Documents[0]
	if dm.claimsIdentity() {
		t.Fatalf("anchor/alias document should not claim its identity")
	}

	plan := BuildPlan(store, files, []DesiredResource{desiredConfigMap("anchored")}, Policy{})
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != PlanSkip {
		t.Fatalf("non-editable construct should yield one skip, got %+v", plan.Actions)
	}
}

// TestBuildPlan_ProjectPolicy proves the injected projection is applied: a Project
// that rewrites the desired object so it matches Git collapses a would-be patch into
// a no-op.
func TestBuildPlan_ProjectPolicy(t *testing.T) {
	store := planStore(t)
	policy := Policy{Project: func(_ *unstructured.Unstructured) *unstructured.Unstructured {
		return deployWeb(1) // normalize back to the Git value
	}}
	// Live says 9, the projection normalizes back to 1.
	desired := []DesiredResource{desiredConfigMap("a"), desiredConfigMap("b"), desiredDeployWeb(9)}
	plan := BuildPlan(store, planFiles(), desired, policy)

	for _, a := range plan.Actions {
		if a.Ref.FilePath == "deploy.yaml" {
			t.Fatalf("projection should have collapsed deploy.yaml to a no-op, got %+v", a)
		}
	}
}

// TestBuildPlan_MissingHydration: a managed document whose file bytes were not
// supplied cannot be planned, so it becomes a skip plus a diagnostic rather than a
// silent or wrong edit.
func TestBuildPlan_MissingHydration(t *testing.T) {
	store := planStore(t)
	var files []manifestedit.FileContent
	for _, f := range planFiles() {
		if f.Path != "deploy.yaml" {
			files = append(files, f)
		}
	}
	desired := []DesiredResource{desiredConfigMap("a"), desiredConfigMap("b"), desiredDeployWeb(3)}
	plan := BuildPlan(store, files, desired, Policy{})

	skip := findAction(t, plan, "deploy.yaml")
	if skip.Kind != PlanSkip {
		t.Errorf("un-hydrated document should skip, got %q", skip.Kind)
	}
	if len(plan.Diagnostics) == 0 {
		t.Errorf("missing hydration should emit a diagnostic")
	}
}

// TestBuildPlan_TwoCreatesSortByIdentity covers the deterministic ordering of
// actions that share a zero Ref: two creates are ordered by manifest identity.
func TestBuildPlan_TwoCreatesSortByIdentity(t *testing.T) {
	store := planStore(t)
	desired := append(inSync(), desiredConfigMap("z"), desiredConfigMap("m"))
	plan := BuildPlan(store, planFiles(), desired, Policy{})

	if len(plan.Actions) != 2 {
		t.Fatalf("want two creates, got %+v", plan.Actions)
	}
	if plan.Actions[0].Identity.Name != "m" || plan.Actions[1].Identity.Name != "z" {
		t.Errorf("creates not sorted by identity: %q then %q",
			plan.Actions[0].Identity.Name, plan.Actions[1].Identity.Name)
	}
}

// TestBuildPlan_NilObjectGhostInert: a nil-Object entry for a resource Git does not
// have is inert — it neither creates nor drops — but it is still diagnosed as a
// malformed snapshot entry.
func TestBuildPlan_NilObjectGhostInert(t *testing.T) {
	store := planStore(t)
	desired := append(inSync(),
		DesiredResource{Resource: types.NewResourceIdentifier("", "v1", "configmaps", "default", "ghost")})
	plan := BuildPlan(store, planFiles(), desired, Policy{})

	if len(plan.Actions) != 0 {
		t.Fatalf("a nil-Object entry must produce no action, got %+v", plan.Actions)
	}
	if len(plan.Diagnostics) != 1 {
		t.Errorf("a nil-Object entry should be diagnosed, got %+v", plan.Diagnostics)
	}
}

// TestBuildPlan_NilObjectProtectsExistingResource is the key safety case: a nil
// Object for a resource that DOES exist in Git must NOT become a managed drop via the
// sweep. The matching document is protected and the malformed entry is diagnosed.
func TestBuildPlan_NilObjectProtectsExistingResource(t *testing.T) {
	store := planStore(t)
	// The Deployment exists in deploy.yaml and resolves; its desired entry is nil.
	desired := []DesiredResource{
		desiredConfigMap("a"), desiredConfigMap("b"),
		{Resource: types.NewResourceIdentifier("apps", "v1", "deployments", "default", "web")},
	}
	plan := BuildPlan(store, planFiles(), desired, Policy{})

	for _, a := range plan.Actions {
		if a.Kind == PlanDropOrphan {
			t.Errorf("a nil object for an existing resource must never drop it: %+v", a)
		}
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("the nil-protected resync should have no actions, got %+v", plan.Actions)
	}
	if len(plan.Diagnostics) != 1 || plan.Diagnostics[0].Path != "deploy.yaml" {
		t.Errorf("want one diagnostic naming deploy.yaml, got %+v", plan.Diagnostics)
	}
}

// TestBuildPlan_ImpureFileTrueIndices proves plan references carry the TRUE file
// index even in a non-contiguous (impure) managed file the acceptance gate would
// refuse: deploy@0, an empty document@1, ConfigMap@2. An empty desired set drops both
// managed documents; the drop references must be #0 and #2, not the loop indices #0
// and #1.
func TestBuildPlan_ImpureFileTrueIndices(t *testing.T) {
	impure := deployYAML + "---\n# comment\n---\n" + configMapCYAML
	fsys := fstest.MapFS{"app.yaml": {Data: []byte(impure)}}
	files := []manifestedit.FileContent{{Path: "app.yaml", Content: []byte(impure)}}
	store := BuildStore(context.Background(), fsys, mapping.NewStaticSnapshotMapper(sampleClusterSnapshot()))

	plan := BuildPlan(store, files, nil, Policy{})

	idxByKind := map[string]int{}
	for _, a := range plan.Actions {
		if a.Kind != PlanDropOrphan {
			t.Fatalf("want only managed drops, got %+v", a)
		}
		idxByKind[a.Identity.Kind] = a.Ref.DocumentIndex
	}
	if idxByKind["Deployment"] != 0 || idxByKind["ConfigMap"] != 2 {
		t.Errorf("drop refs should be true file indices (Deployment#0, ConfigMap#2), got %+v", idxByKind)
	}
}

// TestActionFromDecision maps every manifestedit decision intent to a plan kind: a
// no-change produces no action, and the editing intents map to their kinds.
func TestActionFromDecision(t *testing.T) {
	cases := []struct {
		in      manifestedit.DecisionAction
		want    PlanActionKind
		emitted bool
	}{
		{manifestedit.ActionNoChange, "", false},
		{manifestedit.ActionPatch, PlanPatch, true},
		{manifestedit.ActionReplace, PlanReplace, true},
		{manifestedit.ActionSkip, PlanSkip, true},
		{manifestedit.ActionDelete, PlanSkip, true},
		{manifestedit.DecisionAction("bogus"), PlanSkip, true},
	}
	for _, c := range cases {
		got, emitted := actionFromDecision(c.in)
		if emitted != c.emitted || got != c.want {
			t.Errorf("actionFromDecision(%q) = (%q,%v), want (%q,%v)", c.in, got, emitted, c.want, c.emitted)
		}
	}
}

// TestSkipReason renders a display reason from each structured cause, never from a
// message string.
func TestSkipReason(t *testing.T) {
	cases := []struct {
		cause DocumentCause
		want  string
	}{
		{DocumentCause{Kind: CauseEncrypted}, "encrypted document: cannot patch in place"},
		{DocumentCause{Kind: CauseNonEditable, Detail: "anchor"}, "not editable: anchor"},
		{DocumentCause{Kind: CauseNonEditable}, "not editable"},
		{DocumentCause{Kind: CauseNone}, "document cannot be edited in place"},
	}
	for _, c := range cases {
		if got := skipReason(c.cause); got != c.want {
			t.Errorf("skipReason(%+v) = %q, want %q", c.cause, got, c.want)
		}
	}
}
