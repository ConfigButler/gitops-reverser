// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// The templates used to be validated on the GitProvider. They moved with the field rather than
// being dropped: a template that will not parse otherwise has no symptom except a commit that
// never happens, discovered from a log line nobody is reading.
func TestValidateCommitConfig(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  *configbutleraiv1alpha3.GitTargetCommitSpec
		ok    bool
		saysA string
	}{
		{
			name: "no commit stanza at all",
			ok:   true,
		},
		{
			name: "an empty stanza is the defaults",
			spec: &configbutleraiv1alpha3.GitTargetCommitSpec{},
			ok:   true,
		},
		{
			name: "a valid window and template",
			spec: &configbutleraiv1alpha3.GitTargetCommitSpec{
				Window: ptr.To("30s"),
				Message: &configbutleraiv1alpha3.CommitMessageSpec{
					LiveTemplate: "chore(mirror): {{.Count}} by {{.Author}}",
				},
			},
			ok: true,
		},
		{
			name: "zero is a real choice, not an omission",
			spec: &configbutleraiv1alpha3.GitTargetCommitSpec{Window: ptr.To("0s")},
			ok:   true,
		},
		{
			name:  "a window that is not a duration",
			spec:  &configbutleraiv1alpha3.GitTargetCommitSpec{Window: ptr.To("5 seconds")},
			saysA: "spec.commit.window",
		},
		{
			name:  "a negative window",
			spec:  &configbutleraiv1alpha3.GitTargetCommitSpec{Window: ptr.To("-1s")},
			saysA: "negative",
		},
		{
			// A retired field is refused before any template is parsed, so a stored legacy value
			// reports the migration instruction rather than a parse error about a dead field.
			name: "a retired event template",
			spec: &configbutleraiv1alpha3.GitTargetCommitSpec{
				Message: &configbutleraiv1alpha3.CommitMessageSpec{EventTemplate: "{{.Operation"},
			},
			saysA: "retired",
		},
		{
			name: "a retired group template",
			spec: &configbutleraiv1alpha3.GitTargetCommitSpec{
				Message: &configbutleraiv1alpha3.CommitMessageSpec{GroupTemplate: "grouped: {{.Author}}"},
			},
			saysA: "retired",
		},
		{
			name: "an unparseable live template",
			spec: &configbutleraiv1alpha3.GitTargetCommitSpec{
				Message: &configbutleraiv1alpha3.CommitMessageSpec{LiveTemplate: "{{.Author"},
			},
			saysA: "spec.commit.message",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := &configbutleraiv1alpha3.GitTarget{}
			target.Spec.Commit = tc.spec

			ok, msg := validateCommitConfig(target)

			if tc.ok {
				assert.True(t, ok, msg)
				assert.Empty(t, msg)
				return
			}
			assert.False(t, ok)
			assert.Contains(t, msg, tc.saysA,
				"the message must name the field an operator has to edit")
		})
	}
}
