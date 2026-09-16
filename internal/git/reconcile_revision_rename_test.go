// SPDX-License-Identifier: Apache-2.0

package git

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// A stored reconcileTemplate still naming {{.Revision}} must be refused with a SENTENCE. Without
// the tombstone method the render fails too, but with "can't evaluate field Revision in type
// git.ReconcileCommitMessageData" — which lands on a GitTarget's Validated condition and tells its
// owner nothing about what to write instead. The message is the whole point of the tombstone, so
// the assertion is on the message.
func TestValidateCommitConfig_RejectsRetiredRevisionWithBothSpellings(t *testing.T) {
	config := ResolveCommitConfig(nil).WithTargetMessage(&v1alpha3.CommitMessageSpec{
		ReconcileTemplate: "chore: reconcile {{.Count}} at {{.Revision}}",
	})

	err := ValidateCommitConfig(config)
	if err == nil {
		t.Fatal("a reconcileTemplate naming the retired {{.Revision}} must be rejected")
	}
	for _, want := range []string{"{{.Revision}}", "{{.ResourceVersion}}"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name %s so the fix is readable from the condition", err, want)
		}
	}
}

// A scan for the literal "{{.Revision}}" would miss every indirect spelling. The tombstone is a
// method, so the render fails wherever the field is reached from.
func TestValidateCommitConfig_RejectsRetiredRevisionIndirectly(t *testing.T) {
	for name, template := range map[string]string{
		"with":     "chore: reconcile {{.Count}}{{with .Revision}} at {{.}}{{end}}",
		"pipeline": `chore: reconcile {{.Count}} at {{.Revision | printf "%s"}}`,
		"variable": "{{$rv := .Revision}}chore: reconcile {{.Count}} at {{$rv}}",
		"if":       "chore: reconcile {{.Count}}{{if .Revision}} pinned{{end}}",
	} {
		t.Run(name, func(t *testing.T) {
			config := ResolveCommitConfig(nil).WithTargetMessage(&v1alpha3.CommitMessageSpec{
				ReconcileTemplate: template,
			})
			if err := ValidateCommitConfig(config); err == nil {
				t.Errorf("%q reaches the retired field and must be rejected", template)
			}
		})
	}
}

// The tombstone must not become a compatibility shim: honouring the old name would keep the wrong
// word alive indefinitely, which is the "accepted, and quietly carried on" upgrade failure the
// rejection exists to avoid.
func TestReconcileCommitMessageDataRevision_NeverReturnsTheValue(t *testing.T) {
	value, err := ReconcileCommitMessageData{ResourceVersion: "1331"}.Revision()
	if err == nil {
		t.Fatal("the Revision tombstone must always error")
	}
	if value != "" {
		t.Errorf("the tombstone returned %q: it must never carry the value under the old name", value)
	}
}

// The rename is a rename: the default subject's output is unchanged, so a target on the default
// template crosses the release without noticing.
func TestDefaultReconcileTemplate_SubjectUnchangedByTheRename(t *testing.T) {
	scope := ResyncScopeFor(
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "team-a")
	message, err := renderReconcileCommitMessage(4, "dest", &scope, "1331", ResolveCommitConfig(nil))
	if err != nil {
		t.Fatalf("renderReconcileCommitMessage: %v", err)
	}
	const want = "chore: reconcile 4 deployments in team-a (last resourceVersion: 1331)"
	if message != want {
		t.Errorf("subject = %q, want %q", message, want)
	}
}
