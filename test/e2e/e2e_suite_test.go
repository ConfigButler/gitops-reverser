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

// SynchronizedBeforeSuite splits bootstrap into work that must happen exactly
// once (the Task prepare flow and the cluster-scoped CRD pre-cleanup, run on
// parallel process #1) and per-process configuration (the E2E_AGE_KEY_FILE
// fallback, which relies on os.Setenv and therefore must run in every process).
var _ = SynchronizedBeforeSuite(func() []byte {
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
	prepareE2EClusterOnce()
	return nil
}, func(_ []byte) {
	configureE2EProcess()
})

var _ = AfterEach(func() {
	dumpFailureDiagnostics()
})

// prepareE2EClusterOnce runs the expensive, cluster-mutating bootstrap exactly
// once (parallel process #1): the Task prepare flow plus the cluster-scoped CRD
// pre-cleanup. It is safe to run concurrently with nothing else.
func prepareE2EClusterOnce() {
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

	// Each CRD-installing e2e file now owns its own IceCreamOrder CRD group
	// (see test/e2e/icecream.go). Remove all of them, plus the legacy shared
	// group, so a warm/reused cluster starts clean.
	By("ensuring IceCreamOrder CRDs are removed before tests")
	for _, group := range []string{
		crdGroupCRDLifecycle,
		crdGroupRestartSnapshot,
		crdGroupBiDirectional,
		"shop.example.com", // legacy pre-isolation group
	} {
		output, err = kubectlRun(
			"delete", "crd", iceCreamCRDName(group),
			"--ignore-not-found=true",
		)
		Expect(err).NotTo(HaveOccurred(),
			"failed to delete IceCreamOrder CRD %q before tests", iceCreamCRDName(group))
		_, _ = fmt.Fprintf(GinkgoWriter, "%s", output)
	}

	// The Gitea org is a singleton every repo fixture lives under. Create it
	// once here — this runs on parallel process #1 only and blocks the other
	// processes until it returns — so no two per-spec BeforeAlls ever race a
	// concurrent POST /orgs. An in-process lock cannot solve this: Ginkgo
	// --procs runs specs in separate OS processes, so the race is cross-process.
	By("ensuring the shared Gitea organization exists before parallel specs run")
	ensureSharedGiteaOrgOnce()
}

// ensureSharedGiteaOrgOnce creates the shared test org exactly once, from the
// SynchronizedBeforeSuite process-#1 function. Per-spec bootstrap then only
// creates its own uniquely-named repo under an org that already exists.
func ensureSharedGiteaOrgOnce() {
	gitea := giteaTestInstance()
	Expect(waitForGiteaAPI(gitea.Client())).To(Succeed(),
		"Gitea API must be reachable before creating the shared org")

	ctx, cancel := gitea.Context()
	defer cancel()
	_, err := gitea.Client().EnsureOrg(ctx, gitea.Org, "Test Organization", "E2E Test Organization")
	Expect(err).NotTo(HaveOccurred(), "failed to ensure shared Gitea org %q", gitea.Org)
}

// configureE2EProcess runs in every parallel process. The Task prepare flow
// writes the age key under the stamp directory; when running `go test` directly
// (without `task test-e2e`), point the suite at that prepared key file. This
// relies on os.Setenv, so it must run per-process rather than once.
func configureE2EProcess() {
	if strings.TrimSpace(os.Getenv("E2E_AGE_KEY_FILE")) == "" {
		wd, err := os.Getwd()
		Expect(err).NotTo(HaveOccurred(), "failed to get working directory for e2e run")
		ageKeyPath := filepath.Join(wd, ".stamps", "cluster", resolveE2EContext(), "age-key.txt")
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
