// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAcceptanceRefusedError_BlockMessageIncludesStableDiagnosticFields(t *testing.T) {
	refused := &AcceptanceRefusedError{
		Issues: []AcceptanceIssue{{
			Kind:     IssueUnsupportedKustomize,
			Path:     "apps/kustomization.yaml",
			Message:  "uses remote base",
			Solvable: true,
			Actor:    ActorRepositoryAuthor,
		}},
	}

	got := refused.BlockMessage()

	assert.Contains(t, got, "kind=unsupported-kustomize")
	assert.Contains(t, got, "path=apps/kustomization.yaml")
	assert.Contains(t, got, "actor=repository-author")
	assert.Contains(t, got, "uses remote base")
	assert.Contains(t, got, "edit the unsupported kustomization feature")
	assert.NotContains(t, refused.Error(), "kind=unsupported-kustomize",
		"Error stays stable for callers that only need the coarse refusal")
}

func TestAcceptanceRefusedError_BlockMessageIsBounded(t *testing.T) {
	refused := &AcceptanceRefusedError{
		Issues: []AcceptanceIssue{{
			Kind:    IssueForeignFile,
			Path:    "notes.txt",
			Message: strings.Repeat("x", acceptanceBlockMessageMax),
		}},
	}

	assert.LessOrEqual(t, len(refused.BlockMessage()), acceptanceBlockMessageMax)
}
