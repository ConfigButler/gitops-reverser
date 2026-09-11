// SPDX-License-Identifier: Apache-2.0

package git

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// labeledDeploymentEvent is one live event whose object carries the metadata a commit template
// can now read: the kind, and labels.
func labeledDeploymentEvent(name, namespace string, labels map[string]string) Event {
	asAny := make(map[string]any, len(labels))
	for k, v := range labels {
		asAny[k] = v
	}
	return Event{
		Object: &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]any{
				"name": name, "namespace": namespace, "labels": asAny,
			},
		}},
		Identifier: types.NewResourceIdentifier("apps", "v1", "deployments", namespace, name),
		Operation:  "CREATE",
	}
}

func TestBuildLiveCommitMessageData_CarriesKindAndLabels(t *testing.T) {
	data := buildLiveCommitMessageData("someone", "target", []Event{
		labeledDeploymentEvent("api", "prod", map[string]string{"team": "payments"}),
	})

	ref := data.Resources[0]
	if ref.Kind != "Deployment" {
		t.Errorf("Kind = %q, want the object's kind", ref.Kind)
	}
	if ref.Namespace != "prod" {
		t.Errorf("Namespace = %q, want the namespace for a namespaced resource", ref.Namespace)
	}
	if got := ref.Label("team"); got != "payments" {
		t.Errorf("Label(team) = %q, want payments", got)
	}
	if got := ref.Label("absent"); got != "" {
		t.Errorf("Label(absent) = %q, want the empty string rather than an error", got)
	}
}

// A cluster-scoped resource renders the same scope sentinel a path does, so a template need not
// guard an empty namespace by hand.
func TestBuildLiveCommitMessageData_ClusterScopedRendersTheSentinel(t *testing.T) {
	event := labeledDeploymentEvent("admin", "", nil)
	event.Identifier = types.NewResourceIdentifier("rbac.authorization.k8s.io", "v1", "clusterroles", "", "admin")

	data := buildLiveCommitMessageData("someone", "target", []Event{event})

	if got := data.Resources[0].Namespace; got != types.ClusterScopeSegment {
		t.Errorf("Namespace = %q, want the %q sentinel, so a template need not guard it",
			got, types.ClusterScopeSegment)
	}
}

// A DELETE carries no object in production (the resource is gone), so the metadata fields are
// empty rather than guessed. A template reading them must still render.
func TestBuildLiveCommitMessageData_DeleteHasNoObjectMetadata(t *testing.T) {
	event := labeledDeploymentEvent("api", "prod", map[string]string{"team": "payments"})
	event.Object = nil
	event.Operation = "DELETE"

	ref := buildLiveCommitMessageData("someone", "target", []Event{event}).Resources[0]

	if ref.Kind != "" || ref.Labels != nil {
		t.Errorf("Kind = %q, Labels = %v, want both empty for a DELETE", ref.Kind, ref.Labels)
	}
	if ref.Name != "api" || ref.Namespace != "prod" {
		t.Errorf("identity fields must survive a DELETE, got %+v", ref)
	}
}

// A commit is 1:n, so a label is a SET here where placement reads a single value.
func TestLiveCommitMessageData_LabelValues(t *testing.T) {
	data := buildLiveCommitMessageData("someone", "target", []Event{
		labeledDeploymentEvent("api", "prod", map[string]string{"team": "payments"}),
		labeledDeploymentEvent("web", "prod", map[string]string{"team": "storefront"}),
		labeledDeploymentEvent("cache", "prod", map[string]string{"team": "payments"}),
		labeledDeploymentEvent("bare", "prod", nil),
		labeledDeploymentEvent("blank", "prod", map[string]string{"team": ""}),
	})

	got := data.LabelValues("team")
	want := []string{"payments", "storefront"}
	if len(got) != len(want) {
		t.Fatalf("LabelValues = %v, want %v (distinct, sorted, no blanks)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LabelValues = %v, want %v", got, want)
		}
	}
	if v := data.LabelValue("team"); v != "" {
		t.Errorf("LabelValue = %q, want empty when the commit spans more than one team", v)
	}
	if v := data.LabelValue("absent"); v != "" {
		t.Errorf("LabelValue(absent) = %q, want empty", v)
	}
}

// The subject-line case: one shared value names the whole commit.
func TestLiveCommitMessageData_LabelValue_AgreedValue(t *testing.T) {
	data := buildLiveCommitMessageData("someone", "target", []Event{
		labeledDeploymentEvent("api", "prod", map[string]string{"team": "payments"}),
		labeledDeploymentEvent("cache", "prod", map[string]string{"team": "payments"}),
	})

	if v := data.LabelValue("team"); v != "payments" {
		t.Errorf("LabelValue = %q, want payments", v)
	}
}

func TestRenderLiveCommitMessage_ReadsMetadataFields(t *testing.T) {
	config := CommitConfig{Message: CommitMessageConfig{
		LiveTemplate: `chore: sync {{.Count}} resources{{with .LabelValue "team"}} for {{.}}{{end}}` + "\n" +
			`{{range .Resources}}- {{.Kind}} {{.Namespace}}/{{.Name}}` +
			` ({{.Label "app.kubernetes.io/instance"}})` + "\n" + `{{end}}`,
	}}
	write := PendingWrite{Kind: PendingWriteCommit, Events: []Event{
		labeledDeploymentEvent("api", "prod", map[string]string{
			"team": "payments", "app.kubernetes.io/instance": "voter",
		}),
	}}

	got, err := renderLiveCommitMessage(write, config)
	if err != nil {
		t.Fatalf("renderLiveCommitMessage: %v", err)
	}
	for _, want := range []string{"for payments", "- Deployment prod/api (voter)"} {
		if !strings.Contains(got, want) {
			t.Errorf("message %q is missing %q", got, want)
		}
	}
}

// The accessor is the documented spelling because the raw map is a trap: these templates render
// with missingkey=error, so indexing a label a resource does not carry fails the render, and a
// failed render fails the commit. The admission validator must catch that, not production.
func TestValidateCommitConfig_RejectsRawLabelIndexingAndAcceptsTheAccessor(t *testing.T) {
	trap := CommitConfig{Message: CommitMessageConfig{
		LiveTemplate:      `chore: {{range .Resources}}{{.Labels.team}}{{end}}`,
		ReconcileTemplate: DefaultReconcileCommitMessageTemplate,
	}}
	if err := ValidateCommitConfig(trap); err == nil {
		t.Error("a template indexing .Labels directly must be rejected: it fails for an unlabeled resource")
	}

	safe := CommitConfig{Message: CommitMessageConfig{
		LiveTemplate: `chore: sync {{.Count}}{{range .Resources}} {{.Kind}}/{{.Namespace}}` +
			`/{{.Name}}{{.Label "team"}}{{end}}`,
		ReconcileTemplate: DefaultReconcileCommitMessageTemplate,
	}}
	if err := ValidateCommitConfig(safe); err != nil {
		t.Errorf("the accessor form must validate: %v", err)
	}
}
