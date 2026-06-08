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

package auditutil

// Subresource forwarding policy. The webhook gate forwards mutating subresource
// audit events so the consumer can translate supported ones (e.g. deployments/scale)
// into parent-manifest field patches. This hard-deny list names the subresources
// that must NEVER be mirrored regardless of verb — observed state, runtime streams,
// proxies, logs, and placement — so they are dropped before Redis. See
// docs/design/manifest/version2/scale-subresource-audit-rehydration.md.

// IsHardDeniedSubresource reports whether a (resource, subresource) audit target is
// on the hard-deny list: a subresource that must never be mirrored, regardless of
// verb. A top-level resource (empty subresource) is never denied here — only the
// subresource gate consults this. resource is the plural form from objectRef (e.g.
// "pods", "deployments").
func IsHardDeniedSubresource(resource, subresource string) bool {
	if subresource == "" {
		return false
	}
	// Denied for ANY parent — none is a desired-state mutation worth committing:
	//   status   — observed state, not desired manifest state
	//   finalize — lifecycle control, not a normal desired manifest update
	//   approval — workflow decision; do not infer parent desired state
	//   token    — mints a credential (serviceaccounts/token); nothing to mirror
	switch subresource {
	case "status", "finalize", "approval", "token":
		return true
	}
	// Denied for a specific parent: runtime command/stream endpoints, proxies, logs,
	// eviction, and scheduler binding — none carry a parent desired-state mutation.
	switch resource + "/" + subresource {
	case "pods/exec", "pods/attach", "pods/portforward", "pods/proxy",
		"pods/log", "pods/eviction", "pods/binding",
		"services/proxy", "nodes/proxy":
		return true
	}
	return false
}
