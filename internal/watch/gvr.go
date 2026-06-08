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

package watch

import (
	"fmt"
	"strings"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// GVR represents a concrete Group/Version/Resource target with a scope.
// This is used to plan dynamic informer creation from active rules.
type GVR struct {
	Group    string
	Version  string
	Resource string
	Scope    configv1alpha1.ResourceScope
}

// ComputeRequestedGVRs aggregates the watched GVRs from the active RuleStore: the union
// of every GitTarget's watched types, read from the resident tables.
func (m *Manager) ComputeRequestedGVRs() []GVR {
	if m.RuleStore == nil {
		return nil
	}
	var out []GVR
	for _, table := range m.allWatchedTypeTables() {
		for _, wt := range table.Types {
			out = append(out, GVR{
				Group:    wt.GVR.Group,
				Version:  wt.GVR.Version,
				Resource: wt.GVR.Resource,
				Scope:    wt.Scope,
			})
		}
	}
	return dedupeGVRs(out)
}

// normalizeResource lowercases the resource for consistent matching.
func normalizeResource(r string) string {
	return strings.ToLower(strings.TrimSpace(r))
}

// FormatResolveMisses produces an actionable summary for status and logs.
func FormatResolveMisses(misses []ResolveMiss) string {
	if len(misses) == 0 {
		return "all rule resources resolved"
	}
	parts := make([]string, 0, len(misses))
	for _, miss := range misses {
		detail := miss.Detail
		if detail == "" {
			detail = string(miss.Reason)
		}
		parts = append(parts, fmt.Sprintf("%q: %s", miss.Resource, detail))
	}
	return strings.Join(uniqueStrings(parts), "; ")
}
