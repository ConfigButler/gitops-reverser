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

// Package e2e stores suite-scoped state shared by the end-to-end helpers.
//
// This state allows the suite to preserve resources for debugging when a spec fails or the run is interrupted
// with Ctrl+C / SIGTERM. Cleanup helpers consult it before deleting resources, and kubectl helpers use the
// shared cancellation signal so long-running commands can stop promptly during shutdown.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
)

var (
	suiteWidePreserve        atomic.Bool                                //nolint:gochecknoglobals // suite-scoped preservation state shared across e2e helpers
	suiteWidePreserveMu      sync.Mutex                                 //nolint:gochecknoglobals // protects suite-scoped preservation reasons
	suiteWidePreserveReasons []string                                   //nolint:gochecknoglobals // suite-scoped preservation reasons shared across e2e helpers
	preservedNamespacesMu    sync.RWMutex                               //nolint:gochecknoglobals // protects namespace-scoped preservation state
	preservedNamespaces                         = map[string]struct{}{} //nolint:gochecknoglobals // namespaces intentionally preserved across cleanup calls
	e2eCommandCancel         context.CancelFunc = func() {}             //nolint:gochecknoglobals // suite-scoped cancellation hook for e2e commands
	e2eCommandDoneCh         <-chan struct{}                            //nolint:gochecknoglobals // suite-scoped done signal for e2e commands
	e2eCommandDoneMu         sync.RWMutex                               //nolint:gochecknoglobals // protects suite-scoped command done signal
)

func markSuiteWidePreservation(reason string) {
	trimmedReason := strings.TrimSpace(reason)
	if trimmedReason == "" {
		trimmedReason = "preserve requested"
	}

	suiteWidePreserve.Store(true)

	suiteWidePreserveMu.Lock()
	defer suiteWidePreserveMu.Unlock()

	for _, existing := range suiteWidePreserveReasons {
		if existing == trimmedReason {
			return
		}
	}

	suiteWidePreserveReasons = append(suiteWidePreserveReasons, trimmedReason)
	_, _ = fmt.Fprintf(GinkgoWriter, "Preserving e2e resources: %s\n", trimmedReason)
}

func preserveNamespace(namespace string) {
	trimmedNamespace := strings.TrimSpace(namespace)
	if trimmedNamespace == "" {
		return
	}

	preservedNamespacesMu.Lock()
	defer preservedNamespacesMu.Unlock()

	if _, exists := preservedNamespaces[trimmedNamespace]; exists {
		return
	}

	preservedNamespaces[trimmedNamespace] = struct{}{}
	_, _ = fmt.Fprintf(GinkgoWriter, "Preserving e2e namespace: %s\n", trimmedNamespace)
}

func isPreservedNamespace(namespace string) bool {
	trimmedNamespace := strings.TrimSpace(namespace)
	if trimmedNamespace == "" {
		return false
	}

	preservedNamespacesMu.RLock()
	defer preservedNamespacesMu.RUnlock()

	_, exists := preservedNamespaces[trimmedNamespace]
	return exists
}

// skipCleanupBecauseResourcesArePreserved lets cleanup helpers opt out only when the entire run has been
// halted (Ctrl-C / SIGTERM / BeforeSuite panic) or a specific namespace has been intentionally preserved
// (e.g. the playground for Tilt reuse). Per-spec failures deliberately do NOT preserve: the run finishes,
// cleanup runs, and the next spec starts from a known clean state. Diagnostics for the failed spec are
// still emitted by dumpFailureDiagnostics in AfterEach.
func skipCleanupBecauseResourcesArePreserved(scope, namespace string) bool {
	if suiteWidePreserve.Load() {
		return logCleanupSkip(scope, e2ePreservationSummary())
	}

	if isPreservedNamespace(namespace) {
		return logCleanupSkip(
			scope,
			fmt.Sprintf("Preserving e2e namespace %s for reuse.", strings.TrimSpace(namespace)),
		)
	}

	return false
}

func logCleanupSkip(scope, summary string) bool {
	message := "preserving e2e resources"
	if trimmedScope := strings.TrimSpace(scope); trimmedScope != "" {
		message = fmt.Sprintf("preserving e2e resources; skipping cleanup for %s", trimmedScope)
	}

	By(message)
	if trimmedSummary := strings.TrimSpace(summary); trimmedSummary != "" {
		_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", trimmedSummary)
	}

	return true
}

func e2ePreservationSummary() string {
	suiteWidePreserveMu.Lock()
	defer suiteWidePreserveMu.Unlock()

	if len(suiteWidePreserveReasons) == 0 {
		return "Preserving e2e resources for investigation."
	}

	return fmt.Sprintf(
		"Preserving e2e resources for investigation. Reasons: %s",
		strings.Join(suiteWidePreserveReasons, "; "),
	)
}

func initE2ECommandContext() {
	//nolint:gosec // cancel is stored for suite-lifetime interrupt handling
	e2eExecutionContext, cancel := context.WithCancel(context.Background())
	e2eCommandCancel = cancel
	setE2ECommandContext(e2eExecutionContext)
}

// setE2ECommandDone stores the shared cancellation signal used by kubectl helpers so Ctrl+C or suite aborts
// can stop long-running e2e commands.
func setE2ECommandDone(done <-chan struct{}) {
	e2eCommandDoneMu.Lock()
	defer e2eCommandDoneMu.Unlock()

	e2eCommandDoneCh = done
}

func e2eCommandDone() <-chan struct{} {
	e2eCommandDoneMu.RLock()
	defer e2eCommandDoneMu.RUnlock()

	return e2eCommandDoneCh
}
