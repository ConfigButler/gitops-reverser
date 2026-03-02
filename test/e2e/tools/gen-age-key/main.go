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

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
)

const (
	keyFileMode    = 0o600
	secretFileMode = 0o644
	dirMode        = 0o750
)

func main() {
	var keyFile string
	var secretFile string
	var namespace string
	var secretName string

	flag.StringVar(&keyFile, "key-file", "", "path to write (or read, if it already exists) age identity file")
	flag.StringVar(&secretFile, "secret-file", "", "path to write Kubernetes Secret manifest (optional; requires --namespace and --secret-name)")
	flag.StringVar(&namespace, "namespace", "", "Secret namespace (required with --secret-file)")
	flag.StringVar(&secretName, "secret-name", "", "Secret name (required with --secret-file)")
	flag.Parse()

	if keyFile == "" {
		exitf("--key-file is required")
	}
	if secretFile != "" && (namespace == "" || secretName == "") {
		exitf("--namespace and --secret-name are required when --secret-file is provided")
	}

	// If the key file already exists, reuse it; otherwise generate a fresh identity.
	var identity *age.X25519Identity
	if _, err := os.Stat(keyFile); err == nil {
		parsed, err := parseIdentityFile(keyFile)
		if err != nil {
			exitf("parse existing key file: %v", err)
		}
		identity = parsed
	} else if os.IsNotExist(err) {
		generated, err := age.GenerateX25519Identity()
		if err != nil {
			exitf("generate age identity: %v", err)
		}
		identity = generated

		keyContent := fmt.Sprintf(
			"# created: %s\n# public key: %s\n%s\n",
			time.Now().UTC().Format(time.RFC3339),
			identity.Recipient(),
			identity.String(),
		)
		if err := writeFile(keyFile, []byte(keyContent), keyFileMode); err != nil {
			exitf("write key file: %v", err)
		}
	} else {
		exitf("stat key file: %v", err)
	}

	if secretFile == "" {
		return
	}

	secretContent := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
stringData:
  identity.agekey: |
    %s
`, secretName, namespace, identity.String())
	if err := writeFile(secretFile, []byte(secretContent), secretFileMode); err != nil {
		exitf("write secret file: %v", err)
	}
}

// parseIdentityFile reads an age identity file, locates the AGE-SECRET-KEY line,
// and returns the parsed X25519Identity.
func parseIdentityFile(path string) (*age.X25519Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "AGE-SECRET-KEY-") {
			return age.ParseX25519Identity(line)
		}
	}
	return nil, fmt.Errorf("no AGE-SECRET-KEY line found in %s", path)
}

func writeFile(path string, content []byte, mode os.FileMode) error {
	parent := filepath.Dir(path)
	if parent != "" && parent != "." {
		if err := os.MkdirAll(parent, dirMode); err != nil {
			return err
		}
	}
	return os.WriteFile(path, content, mode)
}

func exitf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
