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

package giteaclient

// User is the subset of the Gitea user payload we care about.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Email string `json:"email"`
}

// Organization is the subset of the Gitea org payload we care about.
type Organization struct {
	ID       int64  `json:"id"`
	UserName string `json:"username"`
	FullName string `json:"full_name"`
}

// PublicKey mirrors Gitea's public key shape. Note: Gitea's API does NOT
// expose the internal `public_key.verified` DB column; KeyType is what
// distinguishes auth keys ("user") from deploy/principal keys.
type PublicKey struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
	KeyType     string `json:"key_type"`
	ReadOnly    bool   `json:"read_only"`
	CreatedAt   string `json:"created_at"`
	LastUsedAt  string `json:"last_used_at"`
}

// Repository is the subset of the Gitea repo payload we use.
type Repository struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url"`
	SSHURL   string `json:"ssh_url"`
	Private  bool   `json:"private"`
	Owner    User   `json:"owner"`
}

// AccessToken is the subset of the Gitea token payload used by the e2e harness.
type AccessToken struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	SHA1 string `json:"sha1"`
}

// RepoHookConfig is the subset of the repository webhook config payload we use.
type RepoHookConfig struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
}

// RepoHook is the subset of the repository webhook payload we use.
type RepoHook struct {
	ID     int64          `json:"id"`
	Type   string         `json:"type"`
	Active bool           `json:"active"`
	Config RepoHookConfig `json:"config"`
}

// CommitVerification is the verification block of the commit API.
type CommitVerification struct {
	Verified  bool   `json:"verified"`
	Reason    string `json:"reason"`
	Signature string `json:"signature"`
	Payload   string `json:"payload"`
	Signer    *User  `json:"signer,omitempty"`
}

// CommitResponse wraps the verification block returned by the commit API.
type CommitResponse struct {
	SHA    string `json:"sha"`
	Commit struct {
		Verification CommitVerification `json:"verification"`
	} `json:"commit"`
}

// TestUser is a local Gitea identity with a password we captured at creation
// time, so tooling can re-authenticate as that user (required for per-user
// endpoints like key verification).
type TestUser struct {
	Login    string
	Email    string
	Password string
	ID       int64
}
