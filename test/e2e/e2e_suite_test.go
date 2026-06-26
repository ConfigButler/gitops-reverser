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

	// Hold an exclusive lock on the cluster for the whole run before touching it,
	// so two concurrent e2e invocations against the same k3d cluster cannot
	// clobber each other. This replaces the per-task with-lock.sh wrapper: the
	// lock now lives in one place (here), and because Go opens the fd O_CLOEXEC
	// it is not inherited by the detached `kubectl port-forward` children that
	// prepare spawns — so it releases cleanly when this process exits, instead of
	// being pinned past the run. Destructive standalone tasks (clean-cluster)
	// honor the same lock via a flock precondition; see test/e2e/Taskfile.yml.
	acquireE2ERunLock()

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

// Release the cluster lock once every parallel process has finished. The second
// function runs only on process #1, AFTER every parallel process has completed its
// specs — so it is the last thing the suite does and the right place for a global,
// end-of-run invariant. assertLateLaneEmpty runs here (defer releases the lock even
// if the assertion fails — it also releases on process exit).
var _ = SynchronizedAfterSuite(func() {}, func() {
	defer releaseE2ERunLock()
	assertNoAnomalousAuditOutcomes()
})

// assertNoAnomalousAuditOutcomes is the headline invariant of the audit-event-outcome taxonomy:
// after a full run, no audit event ended in an error outcome (category="error" — a write_error,
// where the event never reached the per-type log). It asserts on the operator-facing
// gitopsreverser_audit_events_total counter (the same signal you would alert on) rather than
// poking Redis. dropped/diverted outcomes (e.g. older_than_high_water — inherent out-of-order
// audit delivery, recovered by the next checkpoint) are expected and deliberately NOT gated. The
// counter resets when the controller restarts (the restart-reconcile spec does this), but
// Prometheus retains the pre-restart samples, so max_over_time over the run window catches any
// error that ever happened, on any pod. See docs/design/stream/audit-diagnostic-streams-plan.md.
func assertNoAnomalousAuditOutcomes() {
	By("verifying no audit event ended in an error outcome (audit-outcome invariant)")
	ensurePrometheusClient()
	verifyPrometheusAvailable()

	total, err := queryPrometheus(`sum(max_over_time(gitopsreverser_audit_events_total[2h]))`)
	Expect(err).NotTo(HaveOccurred(), "failed to query the audit-events counter")
	if total == 0 {
		_, _ = fmt.Fprintf(GinkgoWriter,
			"✅ audit outcome invariant skipped: watch-first committer-only mode produced no audit events\n")
		return
	}

	// Diagnostic, non-gating: print the outcome breakdown so a flaky run (e.g. an inherent
	// older_than_high_water reorder, which is dropped/recovered and does NOT fail the gate) is
	// self-explaining in the artifacts. diag_all (when --audit-bytype-diag is on, as in e2e) holds
	// the full per-event records in Redis for deeper inspection.
	for _, oc := range []string{"queued", "older_than_high_water", "non_numeric_rv", "rvless_empty_highwater", "not_needed", "shallow_dropped", "write_error"} {
		n, qErr := queryPrometheus(fmt.Sprintf(
			`sum(max_over_time(gitopsreverser_audit_events_total{outcome=%q}[2h])) or vector(0)`, oc))
		if qErr == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "   audit outcome %-24s = %.0f\n", oc, n)
		}
	}

	const errorQuery = `sum(max_over_time(gitopsreverser_audit_events_total{category="error"}[2h]))`
	value, err := queryPrometheus(errorQuery)
	Expect(err).NotTo(HaveOccurred(), "failed to query the audit error-outcome metric")
	Expect(value).To(BeZero(),
		"audit events must never end in an error outcome, but %.0f did (query %q). category=\"error\" "+
			"is a write_error — the event never reached the per-type log. Inspect the "+
			"gitopsreverser_audit_events_total{category=\"error\",outcome=...} breakdown and controller logs.",
		value, errorQuery)
	_, _ = fmt.Fprintf(GinkgoWriter, "✅ no anomalous audit outcomes (0 error-category events across the run)\n")
}

func committerOnlyModeEnabled() bool {
	out, err := kubectlRunInNamespace(namespace, "logs", "deployment/gitops-reverser", "--since=30m")
	if err != nil {
		return false
	}
	return strings.Contains(out, "committer-only mode:")
}

var _ = AfterEach(func() {
	dumpFailureDiagnostics()
})

// e2eRunLock is the open file descriptor whose flock serializes e2e runs against
// one cluster. It is held by process #1 for the whole suite and released in
// SynchronizedAfterSuite (it would also release on process exit).
var e2eRunLock *os.File

// e2eRunLockPath is the lock co-located with the rest of the cluster stamps, so
// it matches the {{.CS}}/e2e.lock the Taskfile's clean-cluster precondition
// probes. utils.GetProjectDir() yields the repo root regardless of the test
// binary's working directory.
func e2eRunLockPath() string {
	projectDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred(), "failed to resolve project dir for e2e lock")
	return filepath.Join(projectDir, ".stamps", "cluster", resolveE2EContext(), "e2e.lock")
}

// acquireE2ERunLock takes an exclusive flock on the cluster lock file. By default
// it fails fast if another run holds it; set E2E_LOCK_WAIT=true to queue instead.
func acquireE2ERunLock() {
	lockPath := e2eRunLockPath()
	Expect(os.MkdirAll(filepath.Dir(lockPath), 0o755)).To(Succeed(), "failed to create e2e lock dir")

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	Expect(err).NotTo(HaveOccurred(), "failed to open e2e lock file %s", lockPath)

	how := syscall.LOCK_EX | syscall.LOCK_NB
	if e2eLockWaitEnabled() {
		how = syscall.LOCK_EX
		_, _ = fmt.Fprintf(GinkgoWriter, "Waiting for e2e lock %s (CTX=%s)...\n", lockPath, resolveE2EContext())
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		Fail(fmt.Sprintf(
			"another e2e run is already active for context %s (lock %s); "+
				"set E2E_LOCK_WAIT=true to wait for it instead of failing fast: %v",
			resolveE2EContext(), lockPath, err))
	}
	e2eRunLock = f
}

// releaseE2ERunLock closes the lock fd, which releases the flock.
func releaseE2ERunLock() {
	if e2eRunLock == nil {
		return
	}
	_ = e2eRunLock.Close()
	e2eRunLock = nil
}

// e2eLockWaitEnabled reports whether E2E_LOCK_WAIT opts into blocking on the lock.
func e2eLockWaitEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("E2E_LOCK_WAIT"))) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

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
		crdGroupRestartReconcile,
		crdGroupBiDirectional,
		crdGroupWildcardRule,
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
