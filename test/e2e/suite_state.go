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
	e2ePreserveResources atomic.Bool                    //nolint:gochecknoglobals // suite-scoped preservation state shared across e2e helpers
	e2ePreserveReasonMu  sync.Mutex                     //nolint:gochecknoglobals // protects suite-scoped preservation reasons
	e2ePreserveReasons   []string                       //nolint:gochecknoglobals // suite-scoped preservation reasons shared across e2e helpers
	e2eCommandCancel     context.CancelFunc = func() {} //nolint:gochecknoglobals // suite-scoped cancellation hook for e2e commands
	e2eCommandDoneCh     <-chan struct{}                //nolint:gochecknoglobals // suite-scoped done signal for e2e commands
	e2eCommandDoneMu     sync.RWMutex                   //nolint:gochecknoglobals // protects suite-scoped command done signal
)

func markE2EResourcesForPreservation(reason string) {
	trimmedReason := strings.TrimSpace(reason)
	if trimmedReason == "" {
		trimmedReason = "preserve requested"
	}

	e2ePreserveResources.Store(true)

	e2ePreserveReasonMu.Lock()
	defer e2ePreserveReasonMu.Unlock()

	for _, existing := range e2ePreserveReasons {
		if existing == trimmedReason {
			return
		}
	}

	e2ePreserveReasons = append(e2ePreserveReasons, trimmedReason)
	_, _ = fmt.Fprintf(GinkgoWriter, "Preserving e2e resources: %s\n", trimmedReason)
}

func shouldPreserveE2EResources() bool {
	return e2ePreserveResources.Load()
}

// skipCleanupBecauseResourcesArePreserved lets cleanup helpers opt out once the suite has decided to keep
// cluster resources around for post-failure or post-interrupt investigation.
func skipCleanupBecauseResourcesArePreserved(scope string) bool {
	if !shouldPreserveE2EResources() {
		return false
	}

	message := "preserving e2e resources"
	if trimmedScope := strings.TrimSpace(scope); trimmedScope != "" {
		message = fmt.Sprintf("preserving e2e resources; skipping cleanup for %s", trimmedScope)
	}

	By(message)
	_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", e2ePreservationSummary())
	return true
}

func e2ePreservationSummary() string {
	e2ePreserveReasonMu.Lock()
	defer e2ePreserveReasonMu.Unlock()

	if len(e2ePreserveReasons) == 0 {
		return "Preserving e2e resources for investigation."
	}

	return fmt.Sprintf(
		"Preserving e2e resources for investigation. Reasons: %s",
		strings.Join(e2ePreserveReasons, "; "),
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
