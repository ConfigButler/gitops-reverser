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
	"os/exec"
	"path/filepath"
	"strings"
)

// SOPSEncryptor encrypts YAML by invoking the external sops binary.
type SOPSEncryptor struct {
	binaryPath string
	configPath string
}

// NewSOPSEncryptor creates an Encryptor that shells out to sops.
func NewSOPSEncryptor(binaryPath, configPath string) *SOPSEncryptor {
	return &SOPSEncryptor{
		binaryPath: binaryPath,
		configPath: configPath,
	}
}

// Encrypt writes plaintext to a secure temp file, encrypts it with sops, and
// returns encrypted YAML bytes.
func (e *SOPSEncryptor) Encrypt(ctx context.Context, plain []byte, _ ResourceMeta) ([]byte, error) {
	tmpDir, err := os.MkdirTemp("", "gitops-reverser-sops-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir for sops: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	inFile := filepath.Join(tmpDir, "secret.yaml")
	if writeErr := os.WriteFile(inFile, plain, 0600); writeErr != nil {
		return nil, fmt.Errorf("failed to write temp secret file: %w", writeErr)
	}

	args := []string{
		"--encrypt",
		"--input-type", "yaml",
		"--output-type", "yaml",
		inFile,
	}
	if strings.TrimSpace(e.configPath) != "" {
		args = append([]string{"--config", e.configPath}, args...)
	}

	cmd := exec.CommandContext(ctx, e.binaryPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sops encryption failed: %w: %s", err, strings.TrimSpace(string(out)))
	}

	return out, nil
}
