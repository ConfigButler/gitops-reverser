// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"strings"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// GVR represents a concrete Group/Version/Resource target with a scope.
// This is used to plan dynamic informer creation from active rules.
type GVR struct {
	Group    string
	Version  string
	Resource string
	Scope    configv1alpha3.ResourceScope
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

// dedupeGVRs removes duplicate GVRs, preserving first-seen order.
func dedupeGVRs(in []GVR) []GVR {
	seen := make(map[GVR]struct{}, len(in))
	out := make([]GVR, 0, len(in))
	for _, gvr := range in {
		if _, ok := seen[gvr]; ok {
			continue
		}
		seen[gvr] = struct{}{}
		out = append(out, gvr)
	}
	return out
}
