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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestResolveCommitConfig_Defaults(t *testing.T) {
	config := ResolveCommitConfig(nil)

	assert.Equal(t, DefaultCommitterName, config.Committer.Name)
	assert.Equal(t, DefaultCommitterEmail, config.Committer.Email)
	assert.Equal(t, DefaultCommitMessageTemplate, config.Message.Template)
	assert.Equal(t, DefaultBatchCommitMessageTemplate, config.Message.BatchTemplate)
}

func TestResolveCommitConfig_CustomValues(t *testing.T) {
	config := ResolveCommitConfig(&v1alpha1.CommitSpec{
		Committer: &v1alpha1.CommitterSpec{
			Name:  "Audit Bot",
			Email: "audit@example.com",
		},
		Message: &v1alpha1.CommitMessageSpec{
			Template:      "audit: {{.Operation}} {{.Name}}",
			BatchTemplate: "snapshot: {{.Count}} {{.GitTarget}}",
		},
	})

	assert.Equal(t, "Audit Bot", config.Committer.Name)
	assert.Equal(t, "audit@example.com", config.Committer.Email)
	assert.Equal(t, "audit: {{.Operation}} {{.Name}}", config.Message.Template)
	assert.Equal(t, "snapshot: {{.Count}} {{.GitTarget}}", config.Message.BatchTemplate)
}

func TestValidateCommitConfig_InvalidTemplate(t *testing.T) {
	config := ResolveCommitConfig(&v1alpha1.CommitSpec{
		Message: &v1alpha1.CommitMessageSpec{
			Template: "{{.Operation",
		},
	})

	err := ValidateCommitConfig(config)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse event commit template")
}

func TestRenderEventCommitMessage_CustomTemplate(t *testing.T) {
	event := Event{
		Operation: "UPDATE",
		Identifier: types.ResourceIdentifier{
			Group:     "apps",
			Version:   "v1",
			Resource:  "deployments",
			Namespace: "prod",
			Name:      "api",
		},
		UserInfo:      UserInfo{Username: "alice"},
		GitTargetName: "platform",
	}

	message, err := renderEventCommitMessage(event, ResolveCommitConfig(&v1alpha1.CommitSpec{
		Message: &v1alpha1.CommitMessageSpec{
			Template: "audit({{.GitTarget}}): {{.Username}} {{.Operation}} {{.Namespace}}/{{.Name}}",
		},
	}))
	require.NoError(t, err)
	assert.Equal(t, "audit(platform): alice UPDATE prod/api", message)
}

func TestRenderBatchCommitMessage_DefaultTemplate(t *testing.T) {
	message, err := renderBatchCommitMessage(&WriteRequest{
		Events:        []Event{{Operation: "CREATE"}, {Operation: "DELETE"}},
		GitTargetName: "demo",
	}, ResolveCommitConfig(nil))
	require.NoError(t, err)
	assert.Equal(t, "reconcile: sync 2 resources", message)
}
