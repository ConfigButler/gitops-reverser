// SPDX-License-Identifier: Apache-2.0

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
