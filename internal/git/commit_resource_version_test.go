// SPDX-License-Identifier: Apache-2.0

package git

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// observedEvent is a live event as the watch seam builds one: the object sanitized, the observed
// counters carried BESIDE it. generation 0 stands in for a kind that has none.
func observedEvent(name, operation, resourceVersion string, generation int64) Event {
	event := Event{
		Identifier:      types.NewResourceIdentifier("apps", "v1", "deployments", "prod", name),
		Operation:       operation,
		ResourceVersion: resourceVersion,
		Generation:      generation,
	}
	if operation != "DELETE" {
		event.Object = &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": name, "namespace": "prod"},
		}}
	}
	return event
}

func TestBuildLiveCommitMessageData_CarriesResourceVersionPerResource(t *testing.T) {
	data := buildLiveCommitMessageData("someone", "target", "", []Event{
		observedEvent("api", "CREATE", "20001", 3),
		// A DELETE carries no object, and so no Kind and no labels — but it DOES carry a
		// version: the watch Deleted frame delivers the final object, so the last version that
		// existed is knowable even though the object is not.
		observedEvent("web", "DELETE", "20003", 4),
		// A reconcile-sourced event observed nothing, so it names no version.
		{Identifier: types.NewResourceIdentifier("", "v1", "configmaps", "prod", "cm"), Operation: "RECONCILE"},
	})

	want := []string{"20001", "20003", ""}
	for i, w := range want {
		if got := data.Resources[i].ResourceVersion; got != w {
			t.Errorf("Resources[%d].ResourceVersion = %q, want %q", i, got, w)
		}
	}
	wantGenerations := []int64{3, 4, 0}
	for i, w := range wantGenerations {
		if got := data.Resources[i].Generation; got != w {
			t.Errorf("Resources[%d].Generation = %d, want %d", i, got, w)
		}
	}
	if kind := data.Resources[1].Kind; kind != "" {
		t.Errorf("a DELETE must still carry no Kind, got %q", kind)
	}
}

// A resource re-edited inside one window collapses to its last routed event, so the version the
// message names is the state the commit actually writes — not the one that opened the window.
//
// It goes through openWindow, which is where coalescing actually happens: add() is
// last-write-wins per windowPathKey and orderedEvents() yields one event per path.
// buildLiveCommitMessageData does NOT deduplicate — it appends one ResourceRef per event it is
// handed — so feeding it both events directly and reading the last element would assert nothing
// but slice indexing, and would keep passing if coalescing stopped selecting the final event.
// Hence the length assertion first: one entry is the property, the counters on it are the payload.
func TestOpenWindow_ResourceVersionIsTheLastObserved(t *testing.T) {
	first := observedEvent("api", "CREATE", "20001", 3)
	second := observedEvent("api", "UPDATE", "20009", 4)

	window := newOpenWindow(first, newContentWriter(types.SensitiveResourcePolicy{}))
	window.add(first)
	window.add(second)

	data := buildLiveCommitMessageData(
		window.Author, window.GitTarget, "", window.orderedEvents())

	if len(data.Resources) != 1 {
		t.Fatalf("the window collapsed to %d resources, want 1 — both events write the same path",
			len(data.Resources))
	}
	surviving := data.Resources[0]
	if surviving.ResourceVersion != "20009" {
		t.Errorf("the surviving ref names %q, want the newest observed version 20009",
			surviving.ResourceVersion)
	}
	if surviving.Generation != 4 {
		t.Errorf("the surviving ref names generation %d, want the newest observed 4",
			surviving.Generation)
	}
}

func TestRenderLiveCommitMessage_RendersResourceVersion(t *testing.T) {
	config := CommitConfig{Message: CommitMessageConfig{
		LiveTemplate: `chore: sync {{.Count}}` + "\n" +
			`{{range .Resources}}- {{.Name}}{{with .ResourceVersion}}@{{.}}{{end}}` + "\n" + `{{end}}`,
	}}
	write := PendingWrite{Kind: PendingWriteCommit, Events: []Event{
		observedEvent("api", "CREATE", "20001", 3),
		{Identifier: types.NewResourceIdentifier("", "v1", "configmaps", "prod", "cm"), Operation: "RECONCILE"},
	}}

	got, err := renderLiveCommitMessage(write, config)
	if err != nil {
		t.Fatalf("renderLiveCommitMessage: %v", err)
	}
	if !strings.Contains(got, "- api@20001") {
		t.Errorf("message %q must name the observed version", got)
	}
	// The {{with}} guard is what keeps a mixed window renderable, so the unversioned entry has
	// to come out clean rather than as a trailing "@".
	if !strings.Contains(got, "- cm\n") {
		t.Errorf("message %q must render an unversioned resource without a dangling separator", got)
	}
}

// The invariant this whole feature rests on: the version reaches the MESSAGE and never the
// CONTENT. A version inside a committed manifest would make every observation a byte change,
// which is exactly what sanitize strips it to prevent — so anyone tempted to "fix" the empty
// field by leaving resourceVersion on the object fails here.
func TestObservedResourceVersionNeverReachesCommittedContent(t *testing.T) {
	live := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name": "api", "namespace": "prod", "resourceVersion": "20001", "generation": int64(3),
		},
	}}

	event := Event{
		Identifier:      types.NewResourceIdentifier("apps", "v1", "deployments", "prod", "api"),
		Operation:       "CREATE",
		Object:          sanitize.Sanitize(live),
		ResourceVersion: live.GetResourceVersion(),
		Generation:      live.GetGeneration(),
	}

	if event.ResourceVersion != "20001" || event.Generation != 3 {
		t.Fatalf("the event must carry the observed counters, got %q/%d",
			event.ResourceVersion, event.Generation)
	}
	if got := event.Object.GetResourceVersion(); got != "" {
		t.Errorf("the committed object still carries resourceVersion %q: it must never reach content", got)
	}
	if got := event.Object.GetGeneration(); got != 0 {
		t.Errorf("the committed object still carries generation %d: it must never reach content", got)
	}
	for _, field := range []string{"resourceVersion", "generation"} {
		if _, found, _ := unstructured.NestedFieldNoCopy(event.Object.Object, "metadata", field); found {
			t.Errorf("metadata.%s is present in the object the writer commits", field)
		}
	}
	// And the source object is untouched, so stamping the event cannot have consumed it.
	if live.GetResourceVersion() != "20001" || live.GetGeneration() != 3 {
		t.Error("sanitizing must not strip the counters from the observed object")
	}
}

func TestValidateCommitConfig_AcceptsResourceVersionTemplates(t *testing.T) {
	config := ResolveCommitConfig(nil).WithTargetMessage(&v1alpha3.CommitMessageSpec{
		LiveTemplate: `chore: sync {{.Count}}{{range .Resources}} {{.Name}}` +
			`{{with .ResourceVersion}}@{{.}}{{end}}{{with .Generation}} gen{{.}}{{end}}{{end}}`,
		ReconcileTemplate: `chore: reconcile {{.Count}}{{with .ResourceVersion}} at {{.}}{{end}}`,
	})

	if err := ValidateCommitConfig(config); err != nil {
		t.Errorf("a template naming ResourceVersion must validate: %v", err)
	}
}

// The validation samples must contain BOTH states, or a template that only renders cleanly for a
// versioned resource passes admission and fails months later in a mixed window.
func TestLiveValidationSamples_CoverVersionedAndUnversioned(t *testing.T) {
	var versioned, unversioned, generated, ungenerated bool
	for _, events := range liveValidationSamples(Event{Operation: "CREATE"}) {
		for _, event := range events {
			if event.ResourceVersion == "" {
				unversioned = true
			} else {
				versioned = true
			}
			if event.Generation == 0 {
				ungenerated = true
			} else {
				generated = true
			}
		}
	}
	if !versioned || !unversioned {
		t.Errorf("samples cover versioned=%v unversioned=%v, want both", versioned, unversioned)
	}
	if !generated || !ungenerated {
		t.Errorf("samples cover generation present=%v absent=%v, want both", generated, ungenerated)
	}
}
