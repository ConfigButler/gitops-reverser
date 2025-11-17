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

// Package git provides Git repository operations and abstractions for the GitOps Reverser controller.
package git

import (
	"errors"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// CommitFile represents a single file to be committed.
type CommitFile struct {
	Path    string
	Content []byte
}

// GetHTTPAuthMethod returns an HTTP basic authentication method from username and password.
func GetHTTPAuthMethod(username, password string) (transport.AuthMethod, error) {
	if username == "" {
		return nil, errors.New("username cannot be empty")
	}
	if password == "" {
		return nil, errors.New("password cannot be empty")
	}

	return &http.BasicAuth{
		Username: username,
		Password: password,
	}, nil
}
