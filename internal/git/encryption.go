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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// ResourceMeta is passed to encryptors for context and diagnostics.
type ResourceMeta struct {
	Identifier      types.ResourceIdentifier
	UID             string
	ResourceVersion string
	Generation      int64
}

// Encryptor transforms plaintext bytes into encrypted bytes.
type Encryptor interface {
	Encrypt(ctx context.Context, plain []byte, meta ResourceMeta) ([]byte, error)
}

// EncryptionConfig controls runtime encryption behavior for Secret writes.
type EncryptionConfig struct {
	SOPSBinaryPath string
	SOPSConfigPath string
}

// ConfigureSecretEncryption wires Secret encryption for git write paths.
func ConfigureSecretEncryption(cfg EncryptionConfig) error {
	if strings.TrimSpace(cfg.SOPSBinaryPath) == "" {
		defaultContentWriter.setEncryptor(nil)
		return nil
	}

	if err := cfg.Validate(); err != nil {
		return err
	}

	defaultContentWriter.setEncryptor(NewSOPSEncryptor(cfg.SOPSBinaryPath, cfg.SOPSConfigPath))
	return nil
}

// Validate verifies encryption config values. Empty SOPSBinaryPath is allowed
// and means encryption is not configured (Secret writes will be rejected).
func (c EncryptionConfig) Validate() error {
	binPath := strings.TrimSpace(c.SOPSBinaryPath)
	if binPath != "" {
		info, err := os.Stat(binPath)
		if err != nil {
			return fmt.Errorf("invalid sops-binary-path %q: %w", binPath, err)
		}
		if info.IsDir() {
			return fmt.Errorf("invalid sops-binary-path %q: path is a directory", binPath)
		}
	}

	configPath := strings.TrimSpace(c.SOPSConfigPath)
	if configPath != "" {
		info, err := os.Stat(configPath)
		if err != nil {
			return fmt.Errorf("invalid sops-config-path %q: %w", configPath, err)
		}
		if info.IsDir() {
			return fmt.Errorf("invalid sops-config-path %q: path is a directory", configPath)
		}
		if !filepath.IsAbs(configPath) {
			return fmt.Errorf("invalid sops-config-path %q: must be an absolute path", configPath)
		}
	}

	return nil
}
