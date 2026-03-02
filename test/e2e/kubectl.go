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

package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

var e2eKubectlContext string //nolint:gochecknoglobals // set once in suite, used across e2e helpers

const kubectlNamespaceArgCount = 2

func setE2EKubectlContext(ctx string) {
	e2eKubectlContext = strings.TrimSpace(ctx)
}

func kubectlContext() string {
	if value := strings.TrimSpace(e2eKubectlContext); value != "" {
		return value
	}

	// Keep compatibility with existing Makefile/shell usage.
	if value := strings.TrimSpace(os.Getenv("CTX")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("KUBE_CONTEXT")); value != "" {
		return value
	}

	return ""
}

func kubectlArgs(args ...string) []string {
	ctx := kubectlContext()
	if ctx == "" {
		return args
	}
	return append([]string{"--context", ctx}, args...)
}

func kubectlArgsInNamespace(namespace string, args ...string) []string {
	if strings.TrimSpace(namespace) == "" {
		return kubectlArgs(args...)
	}

	withCtx := kubectlArgs(args...)
	if hasNamespaceArg(withCtx) {
		return withCtx
	}

	// Ensure we don't append namespace after a "--" separator (e.g., `kubectl exec ... -- ...`),
	// since anything after "--" is passed to the executed command.
	doubleDashIdx := -1
	for i, arg := range withCtx {
		if arg == "--" {
			doubleDashIdx = i
			break
		}
	}
	if doubleDashIdx == -1 {
		return append(withCtx, "-n", namespace)
	}

	updated := make([]string, 0, len(withCtx)+kubectlNamespaceArgCount)
	updated = append(updated, withCtx[:doubleDashIdx]...)
	updated = append(updated, "-n", namespace)
	updated = append(updated, withCtx[doubleDashIdx:]...)
	return updated
}

func hasNamespaceArg(args []string) bool {
	for _, arg := range args {
		if arg == "-n" || arg == "--namespace" {
			return true
		}
		if strings.HasPrefix(arg, "--namespace=") {
			return true
		}
	}
	return false
}

func kubectlCmd(ctx context.Context, args ...string) *exec.Cmd {
	//nolint:gosec // e2e helper intentionally shells out to kubectl
	return exec.CommandContext(ctx, "kubectl", kubectlArgs(args...)...)
}

func kubectlCmdInNamespace(ctx context.Context, namespace string, args ...string) *exec.Cmd {
	//nolint:gosec // e2e helper intentionally shells out to kubectl
	return exec.CommandContext(ctx, "kubectl", kubectlArgsInNamespace(namespace, args...)...)
}

func kubectlRun(args ...string) (string, error) {
	return utils.Run(kubectlCmd(context.Background(), args...))
}

func kubectlRunInNamespace(namespace string, args ...string) (string, error) {
	return utils.Run(kubectlCmdInNamespace(context.Background(), namespace, args...))
}

func kubectlRunWithStdin(namespace, stdin string, args ...string) (string, error) {
	cmd := kubectlCmdInNamespace(context.Background(), namespace, args...)
	cmd.Stdin = strings.NewReader(stdin)
	return utils.Run(cmd)
}
