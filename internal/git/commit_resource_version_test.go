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
// resourceVersion carried BESIDE it.
func observedEvent(name, operation, resourceVersion string) Event {
	event := Event{
		Identifier:      types.NewResourceIdentifier("apps", "v1", "deployments", "prod", name),
		Operation:       operation,
		ResourceVersion: resourceVersion,
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
		observedEvent("api", "CREATE", "20001"),
		// A DELETE carries no object, and so no Kind and no labels — but it DOES carry a
		// version: the watch Deleted frame delivers the final object, so the last version that
		// existed is knowable even though the object is not.
		observedEvent("web", "DELETE", "20003"),
		// A reconcile-sourced event observed nothing, so it names no version.
		{Identifier: types.NewResourceIdentifier("", "v1", "configmaps", "prod", "cm"), Operation: "RECONCILE"},
	})

	want := []string{"20001", "20003", ""}
	for i, w := range want {
		if got := data.Resources[i].ResourceVersion; got != w {
			t.Errorf("Resources[%d].ResourceVersion = %q, want %q", i, got, w)
		}
	}
	if kind := data.Resources[1].Kind; kind != "" {
		t.Errorf("a DELETE must still carry no Kind, got %q", kind)
	}
}

// A resource re-edited inside one window collapses to its last routed event, so the version the
// message names is the state the commit actually writes — not the one that opened the window.
func TestBuildLiveCommitMessageData_ResourceVersionIsTheLastObserved(t *testing.T) {
	data := buildLiveCommitMessageData("someone", "target", "", []Event{
		observedEvent("api", "CREATE", "20001"),
		observedEvent("api", "UPDATE", "20009"),
	})

	last := data.Resources[len(data.Resources)-1]
	if last.ResourceVersion != "20009" {
		t.Errorf("last ref names %q, want the newest observed version 20009", last.ResourceVersion)
	}
}

func TestRenderLiveCommitMessage_RendersResourceVersion(t *testing.T) {
	config := CommitConfig{Message: CommitMessageConfig{
		LiveTemplate: `chore: sync {{.Count}}` + "\n" +
			`{{range .Resources}}- {{.Name}}{{with .ResourceVersion}}@{{.}}{{end}}` + "\n" + `{{end}}`,
	}}
	write := PendingWrite{Kind: PendingWriteCommit, Events: []Event{
		observedEvent("api", "CREATE", "20001"),
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
			"name": "api", "namespace": "prod", "resourceVersion": "20001",
		},
	}}

	event := Event{
		Identifier:      types.NewResourceIdentifier("apps", "v1", "deployments", "prod", "api"),
		Operation:       "CREATE",
		Object:          sanitize.Sanitize(live),
		ResourceVersion: live.GetResourceVersion(),
	}

	if event.ResourceVersion != "20001" {
		t.Fatalf("the event must carry the observed version, got %q", event.ResourceVersion)
	}
	if got := event.Object.GetResourceVersion(); got != "" {
		t.Errorf("the committed object still carries resourceVersion %q: it must never reach content", got)
	}
	if _, found, _ := unstructured.NestedString(event.Object.Object, "metadata", "resourceVersion"); found {
		t.Error("metadata.resourceVersion is present in the object the writer commits")
	}
	// And the source object is untouched, so stamping the event cannot have consumed it.
	if live.GetResourceVersion() != "20001" {
		t.Error("sanitizing must not strip the version from the observed object")
	}
}

func TestValidateCommitConfig_AcceptsResourceVersionTemplates(t *testing.T) {
	config := ResolveCommitConfig(nil).WithTargetMessage(&v1alpha3.CommitMessageSpec{
		LiveTemplate: `chore: sync {{.Count}}{{range .Resources}} {{.Name}}` +
			`{{with .ResourceVersion}}@{{.}}{{end}}{{end}}`,
		ReconcileTemplate: `chore: reconcile {{.Count}}{{with .ResourceVersion}} at {{.}}{{end}}`,
	})

	if err := ValidateCommitConfig(config); err != nil {
		t.Errorf("a template naming ResourceVersion must validate: %v", err)
	}
}

// The validation samples must contain BOTH states, or a template that only renders cleanly for a
// versioned resource passes admission and fails months later in a mixed window.
func TestLiveValidationSamples_CoverVersionedAndUnversioned(t *testing.T) {
	var versioned, unversioned bool
	for _, events := range liveValidationSamples(Event{Operation: "CREATE"}) {
		for _, event := range events {
			if event.ResourceVersion == "" {
				unversioned = true
			} else {
				versioned = true
			}
		}
	}
	if !versioned || !unversioned {
		t.Errorf("samples cover versioned=%v unversioned=%v, want both", versioned, unversioned)
	}
}
