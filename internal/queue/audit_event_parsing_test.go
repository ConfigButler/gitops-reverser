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

package queue

import (
	"testing"

	"github.com/stretchr/testify/assert"
	authnv1 "k8s.io/api/authentication/v1"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

func TestResolveUserInfo(t *testing.T) {
	t.Run("PlainUser", func(t *testing.T) {
		got := resolveUserInfo(auditv1.Event{User: authnv1.UserInfo{Username: "alice"}})
		assert.Equal(t, "alice", got.Username)
		assert.Empty(t, got.DisplayName)
		assert.Empty(t, got.Email)
	})

	t.Run("ImpersonatedUserPreferred", func(t *testing.T) {
		got := resolveUserInfo(auditv1.Event{
			User:             authnv1.UserInfo{Username: "admin"},
			ImpersonatedUser: &authnv1.UserInfo{Username: "bob"},
		})
		assert.Equal(t, "bob", got.Username)
	})

	t.Run("EmptyImpersonatedUserIgnored", func(t *testing.T) {
		got := resolveUserInfo(auditv1.Event{
			User:             authnv1.UserInfo{Username: "admin"},
			ImpersonatedUser: &authnv1.UserInfo{Username: ""},
		})
		assert.Equal(t, "admin", got.Username)
	})

	t.Run("OIDCExtrasPopulateDisplayNameAndEmail", func(t *testing.T) {
		got := resolveUserInfo(auditv1.Event{User: authnv1.UserInfo{
			Username: "carol",
			Extra: map[string]authnv1.ExtraValue{
				displayNameExtraKey: {"Carol Q. User"},
				emailExtraKey:       {"carol@example.com"},
			},
		}})
		assert.Equal(t, "carol", got.Username)
		assert.Equal(t, "Carol Q. User", got.DisplayName)
		assert.Equal(t, "carol@example.com", got.Email)
	})
}

func TestFirstExtraValue(t *testing.T) {
	extra := map[string]authnv1.ExtraValue{"k": {"first", "second"}}
	assert.Equal(t, "first", firstExtraValue(extra, "k"))
	assert.Empty(t, firstExtraValue(extra, "missing"))
	assert.Empty(t, firstExtraValue(nil, "k"))
}
