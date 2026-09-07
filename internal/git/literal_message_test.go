// SPDX-License-Identifier: Apache-2.0

package git

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateLiteralCommitMessage_Contract(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		valid         bool
	}{
		{"omitted", "", true},
		{"unicode boundary", strings.Repeat("ü", 1024), true},
		{"too long", strings.Repeat("ü", 1025), false},
		{"literal multiline", "  fix: {{.Author}}\n\nbody  ", true},
		{"spaces", " \n ", false},
		{"unicode whitespace", "\u0085\u00a0\u1680\u2000\u2028\u2029\u202f\u205f\u3000", false},
		{"tab", "a\tb", false}, {"CR", "a\rb", false},
		{"DEL", "a\x7fb", false}, {"NUL", "a\x00b", false},
		{"invalid utf8", "\xff", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLiteralCommitMessage(tc.message)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestCommitMetadata_LiteralPrecedenceAndPreservation(t *testing.T) {
	for _, kind := range []PendingWriteKind{PendingWriteCommit, PendingWriteAtomic} {
		for _, message := range []string{"  fix: {{.Author}}\n\nbody  ", strings.Repeat("ü", 1024)} {
			p := PendingWrite{Kind: kind, Events: []Event{makeEvent("alice", "api")}, CommitMessage: message,
				CommitConfig: ResolveCommitConfig(nil)}
			p.CommitConfig.Message.LiveTemplate = "{{ invalid"
			p.CommitConfig.Message.ReconcileTemplate = "{{ invalid"
			actual, options, err := p.commitMetadata()
			require.NoError(t, err)
			assert.Equal(t, message, actual)
			assert.Equal(t, DefaultCommitterName, options.Committer.Name)
		}
	}
	p := PendingWrite{Kind: PendingWriteCommit, CommitMessage: " \n ", CommitConfig: ResolveCommitConfig(nil)}
	_, _, err := p.commitMetadata()
	require.ErrorContains(t, err, "non-whitespace")
}

func TestValidateCommitConfig_ConditionalBranches(t *testing.T) {
	for _, template := range []string{
		"{{if gt .Count 1}}{{.Invalid}}{{end}}",
		"{{if not .Author}}{{.Invalid}}{{end}}",
		"{{range .Resources}}{{if eq .Operation `DELETE`}}{{.Invalid}}{{end}}{{end}}",
	} {
		config := ResolveCommitConfig(nil)
		config.Message.LiveTemplate = template
		require.Error(t, ValidateCommitConfig(config))
	}
	require.NoError(t, ValidateCommitConfig(ResolveCommitConfig(nil)))
}
