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

package manifestanalyzer

import "fmt"

// AcceptanceRefusedError is the writer-facing error for a GitTarget folder the acceptance
// gate refused. It carries every issue so the surface (GitTarget status / a blocked stream)
// can name the offending file and reason. errors.As recovers it from a wrapped flush or
// resync error, so the watch layer can translate a refusal into a Blocked stream while a
// plain write fault keeps its existing handling.
type AcceptanceRefusedError struct {
	Issues []AcceptanceIssue
}

// Error returns a bounded one-liner: the first offending file and reason, plus a count of
// any others. Stable ordering comes from Accept's sortIssues, so the "first" issue is
// deterministic.
func (e *AcceptanceRefusedError) Error() string {
	if len(e.Issues) == 0 {
		return "Git path refused: unspecified unsupported content"
	}
	first := e.Issues[0]
	if len(e.Issues) == 1 {
		return fmt.Sprintf("Git path refused at %s: %s", first.Path, first.Message)
	}
	return fmt.Sprintf("Git path refused at %s: %s (and %d more issue(s))",
		first.Path, first.Message, len(e.Issues)-1)
}

// BlockMessage returns a bounded, human-readable one-liner suitable for a GitTarget status
// condition / stream-block message. It is the same text as Error today, named separately so
// the surface intent is explicit at the call site.
func (e *AcceptanceRefusedError) BlockMessage() string { return e.Error() }

// RefusalError returns an *AcceptanceRefusedError when the acceptance was not accepted, or
// nil when the folder is clean. The writer calls this immediately after running the gate, so
// a refusal aborts the commit before any file is touched.
func RefusalError(acc Acceptance) error {
	if acc.Accepted {
		return nil
	}
	return &AcceptanceRefusedError{Issues: acc.Issues}
}
