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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncryptionConfigValidate(t *testing.T) {
	t.Run("empty config is allowed", func(t *testing.T) {
		cfg := EncryptionConfig{}
		require.NoError(t, cfg.Validate())
	})

	t.Run("invalid binary path fails", func(t *testing.T) {
		cfg := EncryptionConfig{SOPSBinaryPath: "/does/not/exist/sops"}
		require.Error(t, cfg.Validate())
	})

	t.Run("relative config path fails", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "sops")
		require.NoError(t, os.WriteFile(f, []byte("binary"), 0700))
		cfg := EncryptionConfig{
			SOPSBinaryPath: f,
			SOPSConfigPath: "relative/path",
		}
		require.Error(t, cfg.Validate())
	})

	t.Run("absolute paths pass", func(t *testing.T) {
		dir := t.TempDir()
		bin := filepath.Join(dir, "sops")
		cfgPath := filepath.Join(dir, ".sops.yaml")
		require.NoError(t, os.WriteFile(bin, []byte("binary"), 0700))
		require.NoError(t, os.WriteFile(cfgPath, []byte("creation_rules: []"), 0600))
		cfg := EncryptionConfig{
			SOPSBinaryPath: bin,
			SOPSConfigPath: cfgPath,
		}
		require.NoError(t, cfg.Validate())
	})
}

func TestConfigureSecretEncryption(t *testing.T) {
	originalWriter := defaultContentWriter
	defaultContentWriter = newContentWriter()
	t.Cleanup(func() { defaultContentWriter = originalWriter })

	require.NoError(t, ConfigureSecretEncryption(EncryptionConfig{}))
	assert.Nil(t, defaultContentWriter.encryptor)
}
