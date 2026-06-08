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
	"sort"

	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// Observations projects the catalog scan into typeset.Observation values — the
// "Scan -> Observation" layer of the followability model. It converts the discovery
// facts the catalog already holds (verbs, scope, preferred, trust state, resource
// policy) into the neutral typeset.Entry shape and lets typeset own the reduction
// (identity uniqueness, origin, scale), so the live and snapshot paths build
// observations identically.
//
// sensitive is the operator-configured SensitiveResourcePolicy (core Secrets plus any
// additional types). Sensitivity is a startup-known policy applied here, when each
// entry is built, exactly like the served-resource (allow/deny) policy — typeset never
// infers it.
func (c *APIResourceCatalog) Observations(sensitive types.SensitiveResourcePolicy) []typeset.Observation {
	c.mu.RLock()
	entries := make([]typeset.Entry, 0, len(c.byGVR))
	for _, e := range c.byGVR {
		entries = append(entries, typeset.Entry{
			GVK:          e.GVK,
			GVR:          e.GVR,
			Namespaced:   e.Namespaced,
			Verbs:        sortedVerbSet(e.Verbs),
			Preferred:    e.Preferred,
			Subresource:  e.Subresource,
			Allowed:      e.Allowed,
			PolicyReason: e.PolicyReason,
			Degraded:     c.groupVersion[e.GVR.GroupVersion()].degraded,
			Sensitive:    sensitive.IsSensitive(e.GVR.Group, e.GVR.Resource),
		})
	}
	ready := c.ready
	c.mu.RUnlock()

	return typeset.ObservationsFromEntries(entries, ready)
}

func sortedVerbSet(verbs map[string]struct{}) []string {
	out := make([]string, 0, len(verbs))
	for verb := range verbs {
		out = append(out, verb)
	}
	sort.Strings(out)
	return out
}
