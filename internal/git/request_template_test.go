// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const requestBodyTemplate = "{{.RequestMessage}}\n\n" +
	"{{range .Resources -}}\n- [{{.Operation}}] {{.Resource}}/{{.Name}}\n{{end -}}"

func requestConfig(requestTemplate string) CommitConfig {
	return ResolveCommitConfig(nil).WithTargetMessage(&v1alpha3.CommitMessageSpec{
		RequestTemplate: requestTemplate,
	})
}

func requestWrite(message string, config CommitConfig) PendingWrite {
	return PendingWrite{
		Kind:          PendingWriteCommit,
		CommitMessage: message,
		CommitConfig:  config,
		Events: []Event{{
			Operation: "UPDATE",
			Identifier: types.ResourceIdentifier{
				Group: "apps", Version: "v1", Resource: "deployments",
				Namespace: "prod", Name: "api",
			},
			UserInfo:           UserInfo{Username: "alice"},
			GitTargetName:      "platform",
			GitTargetNamespace: "default",
		}},
		Targets: map[pendingTargetKey]ResolvedTargetMetadata{
			{Name: "platform", Namespace: "default"}: {Name: "platform", Namespace: "default"},
		},
	}
}

// The gap this feature closes: the override used to REPLACE the template, so supplying a save
// message cost you the resource body liveTemplate would have produced. "Why I saved" and "what was
// saved" were mutually exclusive.
func TestFramedRequestMessage_ComposesTheReasonWithWhatWasSaved(t *testing.T) {
	config := requestConfig(requestBodyTemplate)
	write := requestWrite("fix(api): correct the service port", config)

	message, resolution := write.framedRequestMessage()

	assert.Equal(t, messageResolutionRequestTemplate, resolution)
	assert.Equal(t, "fix(api): correct the service port\n\n- [UPDATE] deployments/api\n", message)
}

// The contract PR 1 of this series must not break: the request's bytes reach the commit unaltered.
// A target with no requestTemplate commits exactly what was supplied — no trimming, no framing, no
// reformatting — which is what makes a CommitRequest auditable against the commit it produced.
func TestCommitMetadata_WithoutARequestTemplateTheLiteralIsCommittedByteForByte(t *testing.T) {
	literal := "  fix(api): correct the service port  \n\n  Route traffic to the API container.  "
	write := requestWrite(literal, ResolveCommitConfig(nil))

	message, _, resolution, err := write.commitMetadata()
	require.NoError(t, err)

	assert.Equal(t, literal, message, "an unframed request message must not be altered at all")
	assert.Equal(t, messageResolutionRequest, resolution)
	assert.Equal(t, messageSourceCommitRequest, resolution.label())
}

// A requestTemplate that fails at render time must not cost the window. The literal already passed
// admission validation, so a correct answer is always in hand; failing would discard the requester's
// save AND every other author's retained events, to punish a formatting mistake.
//
// It is not silent: the commit moves to its own message_source, which is the only signal an
// operator gets for a template that has quietly stopped applying.
func TestFramedRequestMessage_RenderFailureFallsBackToTheLiteralAndSaysSo(t *testing.T) {
	// missingkey=error: Labels carries no "team" on this sample, so this renders at admission for a
	// labelled resource and dies here for one without.
	config := requestConfig("{{.RequestMessage}} {{(index .Resources 0).Labels.team}}")
	write := requestWrite("fix(api): correct the service port", config)

	// No error return at all: "the template failed" is not an outcome this function can report,
	// which is exactly the guarantee the window depends on.
	message, resolution := write.framedRequestMessage()

	assert.Equal(t, "fix(api): correct the service port", message)
	assert.Equal(t, messageResolutionRequestFallback, resolution)
	assert.Equal(t, messageSourceCommitRequestFallback, resolution.label())
}

// The framed and fallback arms must stay distinguishable in commits_total. Folding them together
// would leave "the template works" and "the template silently stopped applying" reading identically.
func TestMessageSource_SeparatesFramedFromFallbackAndPlainRequest(t *testing.T) {
	framedConfig := requestConfig(requestBodyTemplate)

	assert.Equal(t, messageSourceCommitRequest,
		requestWrite("save", ResolveCommitConfig(nil)).messageSource())
	assert.Equal(t, messageSourceCommitRequestFramed,
		requestWrite("save", framedConfig).messageSource())

	// Stamped at commit time, which is the only place the fallback is known.
	stamped := requestWrite("save", framedConfig)
	stamped.committedMessageSource = messageResolutionRequestFallback
	assert.Equal(t, messageSourceCommitRequestFallback, stamped.messageSource())
}

// Precedence: a configured requestTemplate outranks the verbatim arm, and a window with no request
// message is untouched by the field entirely.
func TestResolveMessage_RequestTemplateOutranksTheLiteralArm(t *testing.T) {
	framedConfig := requestConfig(requestBodyTemplate)

	assert.Equal(t, messageResolutionRequestTemplate,
		requestWrite("save", framedConfig).resolveMessage())
	assert.Equal(t, messageResolutionRequest,
		requestWrite("save", ResolveCommitConfig(nil)).resolveMessage())
	assert.Equal(t, messageResolutionLiveTemplate,
		requestWrite("", framedConfig).resolveMessage(),
		"a window no request attached to renders liveTemplate, template configured or not")
}

// The rule that makes the audit property hold rather than merely hoped for. Without it a target
// could frame away the requester's reason entirely and the commit would still count as
// request-sourced.
func TestValidateCommitConfig_RejectsARequestTemplateThatDropsTheMessage(t *testing.T) {
	err := ValidateCommitConfig(requestConfig("chore: sync {{.Count}} resources"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "requestTemplate must render {{.RequestMessage}}")
}

// The accept half, and the reason the check probes the RENDERED OUTPUT instead of scanning the
// template source: every one of these is a legitimate way to put the message in the commit, and a
// source scan for the literal "{{.RequestMessage}}" would reject all but the first.
func TestValidateCommitConfig_AcceptsEveryHonestSpellingOfRequestMessage(t *testing.T) {
	for name, template := range map[string]string{
		"direct":        "{{.RequestMessage}}",
		"piped":         `{{.RequestMessage | printf "%s"}}`,
		"via variable":  "{{$m := .RequestMessage}}chore: save\n\n{{$m}}",
		"guarded":       "{{if .RequestMessage}}{{.RequestMessage}}{{end}}",
		"with a body":   requestBodyTemplate,
		"with a prefix": "save: {{.RequestMessage}}",
	} {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, ValidateCommitConfig(requestConfig(template)))
		})
	}
}

func TestValidateCommitConfig_RejectsAnUnparseableRequestTemplate(t *testing.T) {
	err := ValidateCommitConfig(requestConfig("{{.RequestMessage"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse request commit template")
}

// missingkey=error makes a label a template names but a resource does not carry a render failure.
// Catching it at GitTarget admission is the whole reason validation renders samples: the
// alternative is a commit that falls back months later, for a fault nobody was told about.
//
// It only holds because requestTemplate is validated against the SAME window shapes as
// liveTemplate. A single sample would not catch this: the labelled sample carries "team", so the
// fault appears only on the entries that carry no object at all — which is exactly the shape a
// DELETE has in production, since the object is gone by the time the event is built.
func TestValidateCommitConfig_RejectsARequestTemplateThatReadsAMissingLabel(t *testing.T) {
	err := ValidateCommitConfig(requestConfig(
		"{{.RequestMessage}}{{range .Resources}} {{.Labels.team}}{{end}}"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "execute request commit template")

	// The accessor form renders empty instead of failing, and must still be accepted — the same
	// escape hatch liveTemplate documents.
	assert.NoError(t, ValidateCommitConfig(requestConfig(
		`{{.RequestMessage}}{{range .Resources}} {{.Label "team"}}{{end}}`)))
}

// An unset requestTemplate is "do not frame", not "use a standard framing", so it must have no
// built-in default the way the other two templates do.
func TestResolveCommitConfig_RequestTemplateHasNoDefault(t *testing.T) {
	assert.Empty(t, ResolveCommitConfig(nil).Message.RequestTemplate)
	require.NoError(t, ValidateCommitConfig(ResolveCommitConfig(nil)))

	overlaid := ResolveCommitConfig(nil).WithTargetMessage(&v1alpha3.CommitMessageSpec{
		RequestTemplate: "  " + requestBodyTemplate + "  ",
	})
	assert.Equal(t, requestBodyTemplate, overlaid.Message.RequestTemplate, "overlay trims like the others")
}

// Framing must not become a route that smuggles a message past the check the verbatim arm
// enforces. The controller validates earlier, so this is not reachable through the normal flow —
// it is the invariant that has to survive a second producer of PendingWrite appearing.
func TestCommitMetadata_FramingStillValidatesTheLiteralMessage(t *testing.T) {
	config := requestConfig(requestBodyTemplate)

	for name, message := range map[string]string{
		"a control character": "fix(api): correct\tthe service port",
		"whitespace only":     "   \n   ",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := requestWrite(message, config).commitMetadata()
			require.Error(t, err, "a framed message must face the same literal check as a verbatim one")

			// Same verdict on both arms, so framing cannot change what is accepted.
			_, _, _, unframedErr := requestWrite(message, ResolveCommitConfig(nil)).commitMetadata()
			require.Error(t, unframedErr)
		})
	}

	// And the valid case still renders, so the check gates nothing it should not.
	message, _, resolution, err := requestWrite("fix(api): correct the service port", config).commitMetadata()
	require.NoError(t, err)
	assert.Equal(t, messageResolutionRequestTemplate, resolution)
	assert.Contains(t, message, "fix(api): correct the service port")
}
