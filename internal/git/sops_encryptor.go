// SPDX-License-Identifier: Apache-2.0

package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// SOPSEncryptor encrypts YAML by invoking the external sops binary.
type SOPSEncryptor struct {
	binaryPath string
	configPath string
	workDir    string
	env        map[string]string
}

// NewSOPSEncryptor creates an Encryptor that shells out to sops.
func NewSOPSEncryptor(binaryPath, configPath string) *SOPSEncryptor {
	return NewSOPSEncryptorWithEnv(binaryPath, configPath, "", nil)
}

// NewSOPSEncryptorWithEnv creates an Encryptor that shells out to sops with additional environment variables.
func NewSOPSEncryptorWithEnv(binaryPath, configPath, workDir string, env map[string]string) *SOPSEncryptor {
	copiedEnv := make(map[string]string, len(env))
	for key, value := range env {
		copiedEnv[key] = value
	}
	return &SOPSEncryptor{
		binaryPath: binaryPath,
		configPath: configPath,
		workDir:    workDir,
		env:        copiedEnv,
	}
}

// Encrypt streams plaintext YAML to sops over stdin and returns encrypted YAML bytes.
func (e *SOPSEncryptor) Encrypt(ctx context.Context, plain []byte, meta ResourceMeta) ([]byte, error) {
	args := []string{
		"--encrypt",
		"--input-type", "yaml",
		"--output-type", "yaml",
		"--filename-override", sopsFilenameOverride(meta),
		"/dev/stdin",
	}
	if strings.TrimSpace(e.configPath) != "" {
		args = append([]string{"--config", e.configPath}, args...)
	}

	cmd := newCommandContext(ctx, e.binaryPath, args...)
	if strings.TrimSpace(e.workDir) != "" {
		cmd.Dir = e.workDir
	}
	cmd.Env = buildCommandEnvironment(e.env)
	cmd.Stdin = bytes.NewReader(plain)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sops encryption failed: %w: %s", err, strings.TrimSpace(string(out)))
	}

	return out, nil
}

func newCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

func buildCommandEnvironment(extra map[string]string) []string {
	environment := slices.Clone(os.Environ())
	if len(extra) == 0 {
		return environment
	}

	for key, value := range extra {
		environment = append(environment, fmt.Sprintf("%s=%s", key, value))
	}

	return environment
}

func sopsFilenameOverride(meta ResourceMeta) string {
	if meta.Identifier.Name == "" {
		return "resource.sops.yaml"
	}

	path := meta.Identifier.ToGitPath()
	if strings.HasSuffix(path, ".yaml") {
		return strings.TrimSuffix(path, ".yaml") + ".sops.yaml"
	}

	return path + ".sops.yaml"
}
