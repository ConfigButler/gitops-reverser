// SPDX-License-Identifier: Apache-2.0

package git

import (
	"path"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderSOPSBootstrapTemplate_MultipleRecipients(t *testing.T) {
	raw, err := bootstrapTemplateFS.ReadFile(path.Join(bootstrapTemplateDir, sopsConfigFileName))
	require.NoError(t, err)

	rendered, err := renderSOPSBootstrapTemplate(raw, bootstrapTemplateData{
		AgeRecipients: []string{
			"age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq7k8m6",
			"age1yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyv0r2a",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, string(rendered), "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq7k8m6")
	assert.Contains(t, string(rendered), "age1yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyv0r2a")
}

func TestRenderSOPSBootstrapTemplate_MissingRecipients(t *testing.T) {
	raw, err := bootstrapTemplateFS.ReadFile(path.Join(bootstrapTemplateDir, sopsConfigFileName))
	require.NoError(t, err)

	_, err = renderSOPSBootstrapTemplate(raw, bootstrapTemplateData{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing age recipients")
}
