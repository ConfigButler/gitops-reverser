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

	flag.StringVar(&keyFile, "key-file", "", "path to write age identity file")
	flag.StringVar(&secretFile, "secret-file", "", "path to write Kubernetes Secret manifest")
	flag.StringVar(&namespace, "namespace", "", "Secret namespace")
	flag.StringVar(&secretName, "secret-name", "", "Secret name")
	flag.Parse()

	if keyFile == "" || secretFile == "" || namespace == "" || secretName == "" {
		exitf("all flags are required: --key-file --secret-file --namespace --secret-name")
	}

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		exitf("generate age identity: %v", err)
	}

	keyContent := fmt.Sprintf(
		"# created: %s\n# public key: %s\n%s\n",
		time.Now().UTC().Format(time.RFC3339),
		identity.Recipient(),
		identity.String(),
	)
	if err := writeFile(keyFile, []byte(keyContent), keyFileMode); err != nil {
		exitf("write key file: %v", err)
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
