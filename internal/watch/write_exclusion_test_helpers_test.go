// SPDX-License-Identifier: Apache-2.0

package watch

// nsSelections lifts the pre-exclusion "namespace -> operations" shape most tests care
// about into the per-rule clause shape the table now carries: one unrestricted clause per
// namespace, excluding nobody.
func nsSelections(ops map[string]OperationSet) map[string]RuleSelections {
	out := make(map[string]RuleSelections, len(ops))
	for ns, set := range ops {
		out[ns] = RuleSelections{{Ops: set}}
	}
	return out
}

// opsFilter builds the watch admission state for a rule that excludes nobody.
func opsFilter(ops OperationSet) watchFilter {
	return watchFilter{ops: ops, selections: RuleSelections{{Ops: ops}}}
}

// exclusionFilter builds the watch admission state for a single rule that declines the
// given field managers and users.
func exclusionFilter(ops OperationSet, fieldManagers, users []string) watchFilter {
	selections := RuleSelections{{Ops: ops, Exclusion: newWriteExclusion(fieldManagers, users)}}
	return watchFilter{ops: selections.Ops(), selections: selections}
}
