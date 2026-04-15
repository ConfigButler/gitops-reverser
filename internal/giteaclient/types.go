/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler
*/

package giteaclient

// User is the subset of the Gitea user payload we care about.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Email string `json:"email"`
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
