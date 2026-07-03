// SPDX-License-Identifier: Apache-2.0

package queue

import (
	authnv1 "k8s.io/api/authentication/v1"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// This file is the audit-event → author-identity interpretation used by the
// attribution index. Watch carries the object body now, so the audit body is no
// longer parsed into Git-writable objects (the per-type splice/tail that did that
// was retired with the watch-first rewrite); only the user identity is extracted.

const (
	// displayNameExtraKey is the audit-event user.extra key carrying the OIDC
	// "name" claim, when the API server is configured to map it.
	displayNameExtraKey = "configbutler.ai/claims/display-name"
	// emailExtraKey is the audit-event user.extra key carrying the OIDC
	// "email" claim, when the API server is configured to map it.
	emailExtraKey = "configbutler.ai/claims/email"
)

// resolveUserInfo extracts the effective user identity from an audit event,
// preferring the impersonated user when present. When the effective user
// carries the OIDC display-name / email extras, those populate the optional
// UserInfo fields; absent values are left empty so commit authoring falls back
// to the username.
func resolveUserInfo(event auditv1.Event) git.UserInfo {
	user := event.User
	if event.ImpersonatedUser != nil && event.ImpersonatedUser.Username != "" {
		user = *event.ImpersonatedUser
	}

	return git.UserInfo{
		Username:    user.Username,
		DisplayName: firstExtraValue(user.Extra, displayNameExtraKey),
		Email:       firstExtraValue(user.Extra, emailExtraKey),
	}
}

// firstExtraValue returns the first value for key in an audit event's
// user.extra map, or "" when the key is absent or carries no values.
func firstExtraValue(extra map[string]authnv1.ExtraValue, key string) string {
	values := extra[key]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
