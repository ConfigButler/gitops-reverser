// SPDX-License-Identifier: Apache-2.0

package manifestedit

import (
	"strings"
	"testing"
)

const kustomizationFixture = `# pin the app image here
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: app
resources:
  - deployment.yaml
images:
  - name: ghcr.io/example/podinfo # tracked by the product
    newTag: "6.4.0" # deployed version
replicas:
  - name: web
    count: 3
`

func imagesTagEdit(value string) KustomizationEdit {
	return KustomizationEdit{
		Section: KustomizationSectionImages, EntryIndex: 0,
		EntryName: "ghcr.io/example/podinfo", Field: "newTag", Value: value,
	}
}

func TestPatchKustomization_UpdatesEntryPreservingHandAuthoring(t *testing.T) {
	res, diags := PatchKustomization("kustomization.yaml", []byte(kustomizationFixture),
		[]KustomizationEdit{imagesTagEdit("6.5.0")})
	if res.Mode != EditPatched {
		t.Fatalf("Mode = %q, want patched (diags %+v)", res.Mode, diags)
	}
	got := string(res.Content)
	for _, want := range []string{
		"# pin the app image here",
		"# tracked by the product",
		"# deployed version",
		`newTag: "6.4.0"`,
	} {
		if want == `newTag: "6.4.0"` {
			if strings.Contains(got, want) {
				t.Errorf("old value must be gone:\n%s", got)
			}
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("hand-authored content %q must survive:\n%s", want, got)
		}
	}
	if !strings.Contains(got, `newTag: "6.5.0"`) {
		t.Errorf("new value must keep its quoting style:\n%s", got)
	}
	// Unrelated sections stay put.
	if !strings.Contains(got, "namespace: app") || !strings.Contains(got, "count: 3") {
		t.Errorf("unrelated fields must be untouched:\n%s", got)
	}
}

func TestPatchKustomization_QuotesValuesThatWouldChangeType(t *testing.T) {
	content := "images:\n- name: app\n  newTag: stable\n"
	res, diags := PatchKustomization("kustomization.yaml", []byte(content), []KustomizationEdit{{
		Section: KustomizationSectionImages, EntryIndex: 0,
		EntryName: "app", Field: "newTag", Value: "1.29",
	}})
	if res.Mode != EditPatched {
		t.Fatalf("Mode = %q, want patched (diags %+v)", res.Mode, diags)
	}
	if !strings.Contains(string(res.Content), `newTag: "1.29"`) {
		t.Errorf("a float-looking tag must stay a string:\n%s", res.Content)
	}
}

func TestPatchKustomization_UpdatesReplicaCountAsInteger(t *testing.T) {
	res, diags := PatchKustomization("kustomization.yaml", []byte(kustomizationFixture),
		[]KustomizationEdit{{
			Section: KustomizationSectionReplicas, EntryIndex: 0,
			EntryName: "web", Field: "count", Value: "5",
		}})
	if res.Mode != EditPatched {
		t.Fatalf("Mode = %q, want patched (diags %+v)", res.Mode, diags)
	}
	if !strings.Contains(string(res.Content), "count: 5") {
		t.Errorf("count must be a plain integer:\n%s", res.Content)
	}
}

func TestPatchKustomization_SameValueIsNoChange(t *testing.T) {
	res, diags := PatchKustomization("kustomization.yaml", []byte(kustomizationFixture),
		[]KustomizationEdit{imagesTagEdit("6.4.0")})
	if res.Mode != EditNoChange {
		t.Fatalf("Mode = %q, want no-change (diags %+v)", res.Mode, diags)
	}
	if string(res.Content) != kustomizationFixture {
		t.Errorf("no-change must return the original bytes")
	}
}

func TestDocumentBody(t *testing.T) {
	content := []byte("a: 1\n---\n# second\nb: 2\n")
	body, ok := DocumentBody(content, 1)
	if !ok || string(body) != "# second\nb: 2\n" {
		t.Errorf("DocumentBody(1) = %q, %v; want the exact second-document bytes", body, ok)
	}
	if _, ok := DocumentBody(content, 2); ok {
		t.Errorf("an out-of-range index must report ok=false")
	}
	if _, ok := DocumentBody(content, -1); ok {
		t.Errorf("a negative index must report ok=false")
	}
}

// All-or-nothing: any edit that cannot land skips the whole call, byte-for-byte.
func TestPatchKustomization_RefusalsLeaveContentUntouched(t *testing.T) {
	cases := []struct {
		name    string
		content string
		edit    KustomizationEdit
	}{
		{"entry name mismatch at pinned index", kustomizationFixture, KustomizationEdit{
			Section: KustomizationSectionImages, EntryIndex: 0,
			EntryName: "someone/else", Field: "newTag", Value: "1",
		}},
		{"index out of range", kustomizationFixture, KustomizationEdit{
			Section: KustomizationSectionImages, EntryIndex: 7,
			EntryName: "ghcr.io/example/podinfo", Field: "newTag", Value: "1",
		}},
		{"field not declared on entry", kustomizationFixture, KustomizationEdit{
			Section: KustomizationSectionImages, EntryIndex: 0,
			EntryName: "ghcr.io/example/podinfo", Field: "digest", Value: "sha256:abc",
		}},
		{"missing section", "namespace: app\n", imagesTagEdit("1")},
		{"multi-document file", kustomizationFixture + "---\nnamespace: other\n", imagesTagEdit("1")},
		{"unparseable", "images: [::\n", imagesTagEdit("1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, diags := PatchKustomization("kustomization.yaml", []byte(tc.content),
				[]KustomizationEdit{tc.edit})
			if res.Mode != EditSkipped {
				t.Fatalf("Mode = %q, want skipped", res.Mode)
			}
			if string(res.Content) != tc.content {
				t.Errorf("a refused edit must leave the bytes untouched")
			}
			if len(diags) == 0 {
				t.Errorf("a refusal must carry a diagnostic")
			}
		})
	}
}

func TestAppendKustomizationResource_AddsEntryPreservingHandAuthoring(t *testing.T) {
	res, diags := AppendKustomizationResource("kustomization.yaml", []byte(kustomizationFixture), "debug-toolbox.yaml")
	if res.Mode != EditPatched {
		t.Fatalf("Mode = %q, want patched (diags %+v)", res.Mode, diags)
	}
	got := string(res.Content)
	for _, want := range []string{
		"# pin the app image here",
		"resources:\n  - deployment.yaml\n  - debug-toolbox.yaml\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
	// Unrelated sections stay put.
	if !strings.Contains(got, "namespace: app") || !strings.Contains(got, "count: 3") {
		t.Errorf("unrelated fields must be untouched:\n%s", got)
	}
}

func TestAppendKustomizationResource_IdempotentWhenAlreadyListed(t *testing.T) {
	res, _ := AppendKustomizationResource("kustomization.yaml", []byte(kustomizationFixture), "deployment.yaml")
	if res.Mode != EditNoChange {
		t.Fatalf("Mode = %q, want no-change for an entry that is already listed", res.Mode)
	}
	if string(res.Content) != kustomizationFixture {
		t.Errorf("a no-op must leave the bytes byte-identical")
	}
}

func TestAppendKustomizationResource_RefusalsLeaveContentUntouched(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"no resources sequence", "namespace: app\nimages:\n  - name: x\n    newTag: \"1\"\n"},
		{"resources is not a sequence", "resources: not-a-list\n"},
		{"multi-document file", kustomizationFixture + "---\nnamespace: other\n"},
		{"unparseable", "resources: [::\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, diags := AppendKustomizationResource("kustomization.yaml", []byte(tc.content), "new.yaml")
			if res.Mode != EditSkipped {
				t.Fatalf("Mode = %q, want skipped", res.Mode)
			}
			if string(res.Content) != tc.content {
				t.Errorf("a refused append must leave the bytes untouched")
			}
			if len(diags) == 0 {
				t.Errorf("a refusal must carry a diagnostic")
			}
		})
	}
}
