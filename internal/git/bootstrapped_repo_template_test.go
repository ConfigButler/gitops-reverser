/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
