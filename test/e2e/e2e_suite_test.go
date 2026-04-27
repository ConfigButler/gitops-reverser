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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purpose of being used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, and installs
// CertManager.
func TestE2E(t *testing.T) {
	initE2ECommandContext()
	watchForE2EInterrupts()
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting gitops-reverser integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	defer func() {
		if recovered := recover(); recovered != nil {
			markSuiteWidePreservation("BeforeSuite failed")
			panic(recovered)
		}
	}()

	if img := os.Getenv("PROJECT_IMAGE"); img == "" {
		By("local run: preparing cluster via Task target")
	} else {
		By(fmt.Sprintf("using pre-built image: %s", img))
	}

	By("preparing e2e cluster prerequisites via Task target")
	ensureE2EPrepared()
})

var _ = AfterEach(func() {
	dumpFailureDiagnostics()
})

func ensureE2EPrepared() {
	ctx := resolveE2EContext()
	setE2EKubectlContext(ctx)
	ns := resolveE2ENamespace()
	installMode, err := resolveE2EInstallMode()
	Expect(err).NotTo(HaveOccurred(), "INSTALL_MODE environment variable must be set for e2e runs")
	installName := resolveE2EInstallName(ns)
	cmd := taskCommand(
		fmt.Sprintf("CTX=%s", ctx),
		fmt.Sprintf("INSTALL_NAME=%s", installName),
		fmt.Sprintf("INSTALL_MODE=%s", installMode),
		fmt.Sprintf("NAMESPACE=%s", ns),
		"prepare-e2e",
	)
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "failed to run task target for e2e prepare")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s", output)

	// Some e2e shell scripts call `kubectl` without an explicit `--context` flag. Ensure `kubectl` is
	// pointed at the intended cluster context for the remainder of the test run.
	output, err = kubectlRun("config", "use-context", ctx)
	Expect(err).NotTo(HaveOccurred(), "failed to set kubectl context for e2e run")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s", output)

	By("ensuring IceCreamOrder CRD is removed before tests")
	output, err = kubectlRun(
		"delete", "crd", "icecreamorders.shop.example.com",
		"--ignore-not-found=true",
	)
	Expect(err).NotTo(HaveOccurred(), "failed to delete IceCreamOrder CRD before tests")
	_, _ = fmt.Fprintf(GinkgoWriter, "%s", output)

	// The Task prepare flow writes the age key under the stamp directory. When running `go test` directly
	// (without `task test-e2e`), ensure the suite uses that prepared key file.
	if strings.TrimSpace(os.Getenv("E2E_AGE_KEY_FILE")) == "" {
		wd, err := os.Getwd()
		Expect(err).NotTo(HaveOccurred(), "failed to get working directory for e2e run")
		ageKeyPath := filepath.Join(wd, ".stamps", "cluster", ctx, "age-key.txt")
		Expect(os.Setenv("E2E_AGE_KEY_FILE", ageKeyPath)).To(Succeed())
	}
}

func taskCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("task", args...)
	_, _ = fmt.Fprintf(
		GinkgoWriter,
		"task invocation: args=%v\n",
		args,
	)
	return cmd
}

func resolveE2EInstallName(namespace string) string {
	if value := strings.TrimSpace(os.Getenv("INSTALL_NAME")); value != "" {
		return value
	}
	return namespace
}

func resolveE2EInstallMode() (string, error) {
	if value := strings.TrimSpace(os.Getenv("INSTALL_MODE")); value != "" {
		return value, nil
	}
	return "", errors.New("INSTALL_MODE environment variable must be set")
}

func resolveE2EContext() string {
	if value := strings.TrimSpace(os.Getenv("CTX")); value != "" {
		return value
	}

	output, err := kubectlRun("config", "current-context")
	if err == nil {
		if value := strings.TrimSpace(output); value != "" {
			return value
		}
	}

	return "kind-gitops-reverser-test-e2e"
}

func watchForE2EInterrupts() {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	go func() {
		sig := <-signals
		markSuiteWidePreservation(fmt.Sprintf("received signal %s", sig))
		e2eCommandCancel()
	}()
}
