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
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSOPSEncryptorEncrypt(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "sops")
	require.NoError(t, os.WriteFile(script, []byte(`#!/usr/bin/env bash
set -euo pipefail
infile="${@: -1}"
cat <<'EOF'
apiVersion: v1
kind: Secret
sops:
  version: 3.9.0
encrypted_regex: "^(data|stringData)$"
EOF
cat "$infile" >/dev/null
`), 0700))

	encryptor := NewSOPSEncryptor(script, "")
	out, err := encryptor.Encrypt(context.Background(), []byte("apiVersion: v1\nkind: Secret\n"), ResourceMeta{})
	require.NoError(t, err)
	assert.Contains(t, string(out), "sops:")
	assert.Contains(t, string(out), "encrypted_regex:")
}

func TestSOPSEncryptorEncryptFailure(t *testing.T) {
	encryptor := NewSOPSEncryptor("/does/not/exist/sops", "")
	_, err := encryptor.Encrypt(context.Background(), []byte("apiVersion: v1\nkind: Secret\n"), ResourceMeta{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sops encryption failed")
}
