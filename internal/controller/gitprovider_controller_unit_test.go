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

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

func TestValidateCommitConfiguration_InvalidTemplate(t *testing.T) {
	reconciler := &GitProviderReconciler{}
	provider := &configbutleraiv1alpha1.GitProvider{
		Spec: configbutleraiv1alpha1.GitProviderSpec{
			Commit: &configbutleraiv1alpha1.CommitSpec{
				Message: &configbutleraiv1alpha1.CommitMessageSpec{
					Template: "{{.Operation",
				},
			},
		},
	}

	err := reconciler.validateCommitConfiguration(provider)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid commit configuration")
	assert.Empty(t, provider.Status.SigningPublicKey)
}

func TestValidateCommitConfiguration_SigningNotImplemented(t *testing.T) {
	reconciler := &GitProviderReconciler{}
	provider := &configbutleraiv1alpha1.GitProvider{
		Status: configbutleraiv1alpha1.GitProviderStatus{
			SigningPublicKey: "ssh-ed25519 AAAA old",
		},
		Spec: configbutleraiv1alpha1.GitProviderSpec{
			Commit: &configbutleraiv1alpha1.CommitSpec{
				Signing: &configbutleraiv1alpha1.CommitSigningSpec{
					SecretRef: configbutleraiv1alpha1.LocalSecretReference{
						Name: "signing-secret",
					},
				},
			},
		},
	}

	err := reconciler.validateCommitConfiguration(provider)
	require.ErrorIs(t, err, ErrCommitSigningNotImplemented)
	assert.Empty(t, provider.Status.SigningPublicKey)
}
