// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

func deploymentObj(image string, replicas *int64) map[string]interface{} {
	spec := map[string]interface{}{
		"template": map[string]interface{}{
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{"name": "app", "image": image},
				},
			},
		},
	}
	if replicas != nil {
		spec["replicas"] = *replicas
	}
	return map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": "web"},
		"spec":       spec,
	}
}

func desiredOf(obj map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: obj}
	// Live objects carry int64 replicas; normalize the fixture.
	if v, ok, _ := unstructured.NestedFieldNoCopy(obj, "spec", "replicas"); ok {
		if n, isInt := v.(int); isInt {
			_ = unstructured.SetNestedField(obj, int64(n), "spec", "replicas")
		}
	}
	return u
}

func desiredImage(t *testing.T, u *unstructured.Unstructured) string {
	t.Helper()
	slots := collectContainerSlots(u.Object)
	if len(slots) != 1 {
		t.Fatalf("want exactly one container slot, got %d", len(slots))
	}
	return slots[0].image
}

func imgEntry(name string, set map[string]string) ImageOverride {
	e := ImageOverride{Source: "kustomization.yaml", Name: name}
	if v, ok := set["newName"]; ok {
		e.NewName, e.HasNewName = v, true
	}
	if v, ok := set["newTag"]; ok {
		e.NewTag, e.HasNewTag = v, true
	}
	if v, ok := set["digest"]; ok {
		e.Digest, e.HasDigest = v, true
	}
	return e
}

// TestSplitDesired_TagRoutedToEntry pins the core edit-through behavior: a live tag change
// produced by a newTag entry lands on the entry and the file keeps its bytes.
func TestSplitDesired_TagRoutedToEntry(t *testing.T) {
	git := deploymentObj("ghcr.io/example/app:1.0.0", nil)
	desired := desiredOf(deploymentObj("ghcr.io/example/app:2.0.0", nil))
	ov := &KustomizeOverrides{Images: []ImageOverride{
		imgEntry("ghcr.io/example/app", map[string]string{"newTag": "1.5.0"}),
	}}

	out, edits := SplitDesiredForOverrides(git, desired, ov)
	if got := desiredImage(t, out); got != "ghcr.io/example/app:1.0.0" {
		t.Errorf("file image = %q, want the source form restored", got)
	}
	if len(edits) != 1 {
		t.Fatalf("want one entry edit, got %+v", edits)
	}
	e := edits[0]
	if e.KustomizationPath != "kustomization.yaml" ||
		e.Edit != (manifestedit.KustomizationEdit{
			Section: manifestedit.KustomizationSectionImages, EntryIndex: 0,
			EntryName: "ghcr.io/example/app", Field: "newTag", Value: "2.0.0",
		}) {
		t.Errorf("unexpected edit %+v", e)
	}
}

// TestSplitDesired_LiveMatchesRenderIsNoOp pins the write-through fix: live equal
// to the rendered value must restore the source form and route nothing, so the
// source file's "stale" tag is never overwritten.
func TestSplitDesired_LiveMatchesRenderIsNoOp(t *testing.T) {
	git := deploymentObj("ghcr.io/example/app:1.0.0", nil)
	desired := desiredOf(deploymentObj("ghcr.io/example/app:1.5.0", nil))
	ov := &KustomizeOverrides{Images: []ImageOverride{
		imgEntry("ghcr.io/example/app", map[string]string{"newTag": "1.5.0"}),
	}}

	out, edits := SplitDesiredForOverrides(git, desired, ov)
	if got := desiredImage(t, out); got != "ghcr.io/example/app:1.0.0" {
		t.Errorf("file image = %q, want source form", got)
	}
	if len(edits) != 0 {
		t.Errorf("want no edits, got %+v", edits)
	}
}

// TestSplitDesired_UngovernedComponentPatchesFile: the matching entry declares
// only newName, so a tag change is file-supplied and flows into the source image
// while the name stays in its source form.
func TestSplitDesired_UngovernedComponentPatchesFile(t *testing.T) {
	git := deploymentObj("old/app:1.0.0", nil)
	desired := desiredOf(deploymentObj("new/app:2.0.0", nil))
	ov := &KustomizeOverrides{Images: []ImageOverride{
		imgEntry("old/app", map[string]string{"newName": "new/app"}),
	}}

	out, edits := SplitDesiredForOverrides(git, desired, ov)
	if got := desiredImage(t, out); got != "old/app:2.0.0" {
		t.Errorf("file image = %q, want source name with the live tag", got)
	}
	if len(edits) != 0 {
		t.Errorf("tag is file-supplied here; want no edits, got %+v", edits)
	}
}

// TestSplitDesired_NameChangeRoutedToNewName: a live name change whose supplier
// is a newName entry updates the entry.
func TestSplitDesired_NameChangeRoutedToNewName(t *testing.T) {
	git := deploymentObj("old/app:1.0.0", nil)
	desired := desiredOf(deploymentObj("mirror/app:1.0.0", nil))
	ov := &KustomizeOverrides{Images: []ImageOverride{
		imgEntry("old/app", map[string]string{"newName": "new/app"}),
	}}

	out, edits := SplitDesiredForOverrides(git, desired, ov)
	if got := desiredImage(t, out); got != "old/app:1.0.0" {
		t.Errorf("file image = %q, want untouched source", got)
	}
	if len(edits) != 1 || edits[0].Edit.Field != "newName" || edits[0].Edit.Value != "mirror/app" {
		t.Fatalf("want one newName edit to mirror/app, got %+v", edits)
	}
}

// TestSplitDesired_DigestRoutedToEntry: a live digest change whose supplier is a
// digest entry updates the entry; tag and name keep their source form.
func TestSplitDesired_DigestRoutedToEntry(t *testing.T) {
	git := deploymentObj("app:1.0.0", nil)
	desired := desiredOf(deploymentObj("app:1.0.0@sha256:ccc", nil))
	ov := &KustomizeOverrides{Images: []ImageOverride{
		imgEntry("app", map[string]string{"digest": "sha256:bbb"}),
	}}

	out, edits := SplitDesiredForOverrides(git, desired, ov)
	if got := desiredImage(t, out); got != "app:1.0.0" {
		t.Errorf("file image = %q, want untouched source (no digest)", got)
	}
	if len(edits) != 1 ||
		edits[0].Edit != (manifestedit.KustomizationEdit{
			Section: manifestedit.KustomizationSectionImages, EntryIndex: 0,
			EntryName: "app", Field: "digest", Value: "sha256:ccc",
		}) {
		t.Fatalf("want one digest edit to sha256:ccc, got %+v", edits)
	}
}

// TestSplitDesired_RemovalUnroutable: live drops the digest an entry supplies;
// nothing can express that on the entry, so the whole object writes through.
func TestSplitDesired_RemovalUnroutable(t *testing.T) {
	git := deploymentObj("app:1.0.0", nil)
	desired := desiredOf(deploymentObj("app:1.0.0", nil))
	ov := &KustomizeOverrides{Images: []ImageOverride{
		imgEntry("app", map[string]string{"digest": "sha256:abc"}),
	}}

	out, edits := SplitDesiredForOverrides(git, desired, ov)
	if got := desiredImage(t, out); got != "app:1.0.0" {
		t.Errorf("file image = %q, want the live value written through", got)
	}
	if len(edits) != 0 {
		t.Errorf("want no edits on write-through, got %+v", edits)
	}
}

// TestSplitDesired_ConflictingContainersAbandonRouting: two containers governed
// by one entry cannot pin two different tags on it.
func TestSplitDesired_ConflictingContainersAbandonRouting(t *testing.T) {
	containers := func(tagA, tagB string) map[string]interface{} {
		return map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]interface{}{"name": "web"},
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{"name": "a", "image": "app:" + tagA},
							map[string]interface{}{"name": "b", "image": "app:" + tagB},
						},
					},
				},
			},
		}
	}
	git := containers("1.0.0", "1.0.0")
	desired := desiredOf(containers("2.0.0", "3.0.0"))
	ov := &KustomizeOverrides{Images: []ImageOverride{
		imgEntry("app", map[string]string{"newTag": "1.5.0"}),
	}}

	out, edits := SplitDesiredForOverrides(git, desired, ov)
	if len(edits) != 0 {
		t.Fatalf("conflicting demands must abandon routing, got %+v", edits)
	}
	slots := collectContainerSlots(out.Object)
	if slots[0].image != "app:2.0.0" || slots[1].image != "app:3.0.0" {
		t.Errorf("write-through must keep the live values, got %q / %q", slots[0].image, slots[1].image)
	}
}

// TestSplitDesired_ChainedEntriesCompose: a base renames the image, the parent
// pins the tag of the renamed image; a live tag change lands on the parent entry.
func TestSplitDesired_ChainedEntriesCompose(t *testing.T) {
	git := deploymentObj("app:1.0.0", nil)
	desired := desiredOf(deploymentObj("mirror/app:9.9.9", nil))
	base := imgEntry("app", map[string]string{"newName": "mirror/app"})
	base.Source = "base/kustomization.yaml"
	parent := imgEntry("mirror/app", map[string]string{"newTag": "2.0.0"})
	ov := &KustomizeOverrides{Images: []ImageOverride{base, parent}}

	out, edits := SplitDesiredForOverrides(git, desired, ov)
	if got := desiredImage(t, out); got != "app:1.0.0" {
		t.Errorf("file image = %q, want untouched source", got)
	}
	if len(edits) != 1 || edits[0].KustomizationPath != "kustomization.yaml" ||
		edits[0].Edit.Field != "newTag" || edits[0].Edit.Value != "9.9.9" {
		t.Fatalf("want one newTag edit on the parent entry, got %+v", edits)
	}
}

// TestSplitDesired_ReplicasRoutedToEntry: a pinned count absorbs the live scale;
// the file's replicas field is restored to its source form (absent here).
func TestSplitDesired_ReplicasRoutedToEntry(t *testing.T) {
	five := int64(5)
	git := deploymentObj("app:1.0.0", nil) // source has no spec.replicas
	desired := desiredOf(deploymentObj("app:1.0.0", &five))
	ov := &KustomizeOverrides{Replicas: []ReplicaOverride{
		{Source: "kustomization.yaml", Index: 0, Name: "web", Count: 3},
	}}

	out, edits := SplitDesiredForOverrides(git, desired, ov)
	if _, has, _ := unstructured.NestedInt64(out.Object, "spec", "replicas"); has {
		t.Errorf("source has no replicas field, so the desired-for-file must not either")
	}
	if len(edits) != 1 ||
		edits[0].Edit != (manifestedit.KustomizationEdit{
			Section: manifestedit.KustomizationSectionReplicas, EntryIndex: 0,
			EntryName: "web", Field: "count", Value: "5",
		}) {
		t.Fatalf("want one count edit to 5, got %+v", edits)
	}
}

// TestSplitDesired_ReplicasMatchRestoresSource: live equals the pinned count; the
// source's own (stale) value is restored and nothing is routed.
func TestSplitDesired_ReplicasMatchRestoresSource(t *testing.T) {
	one, three := int64(1), int64(3)
	git := deploymentObj("app:1.0.0", &one)
	desired := desiredOf(deploymentObj("app:1.0.0", &three))
	ov := &KustomizeOverrides{Replicas: []ReplicaOverride{
		{Source: "kustomization.yaml", Index: 0, Name: "web", Count: 3},
	}}

	out, edits := SplitDesiredForOverrides(git, desired, ov)
	if got, _, _ := unstructured.NestedInt64(out.Object, "spec", "replicas"); got != 1 {
		t.Errorf("desired-for-file replicas = %d, want the source's 1", got)
	}
	if len(edits) != 0 {
		t.Errorf("want no edits, got %+v", edits)
	}
}

// TestSplitDesired_ReplicasIgnoresOtherKinds: the replica transformer only
// touches Deployment/ReplicaSet/StatefulSet.
func TestSplitDesired_ReplicasIgnoresOtherKinds(t *testing.T) {
	git := map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "web"},
	}
	desired := desiredOf(map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "web"},
		"spec":     map[string]interface{}{"replicas": int64(5)},
	})
	ov := &KustomizeOverrides{Replicas: []ReplicaOverride{
		{Source: "kustomization.yaml", Index: 0, Name: "web", Count: 3},
	}}

	out, edits := SplitDesiredForOverrides(git, desired, ov)
	if got, _, _ := unstructured.NestedInt64(out.Object, "spec", "replicas"); got != 5 {
		t.Errorf("non-workload kinds are untouched, got replicas %d", got)
	}
	if len(edits) != 0 {
		t.Errorf("want no edits, got %+v", edits)
	}
}
