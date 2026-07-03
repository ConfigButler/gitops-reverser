// SPDX-License-Identifier: Apache-2.0

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
cat <<'EOF'
apiVersion: v1
kind: Secret
sops:
  version: 3.9.0
encrypted_regex: "^(data|stringData)$"
EOF
cat >/dev/null
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
