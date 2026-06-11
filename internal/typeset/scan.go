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

package typeset

import "k8s.io/apimachinery/pkg/runtime/schema"

// Scan is one normalized discovery result — the per-scan facts a catalog scan
// produces, with no judgement attached. The catalog stays a thin normalizer; ALL
// cross-scan judgement ("additions fast, removals slow": retain-on-error, the removal
// grace for omissions) is applied by Registry.UpdateFromScan. See
// docs/design/typeset-owns-discovery-grace.md.
type Scan struct {
	// Entries are the resources this scan served, policy-annotated. All are trusted
	// facts: a failed group/version contributes no entries (the registry carries its
	// last-known records forward instead).
	Entries []Entry
	// ScannedGroupVersions are the group/versions this scan returned a (possibly
	// empty) resource list for. A previously-known record of a scanned group/version
	// that is missing from Entries is meaningfully absent — the removal grace judges
	// it — even on an otherwise incomplete scan.
	ScannedGroupVersions []schema.GroupVersion
	// FailedGroupVersions are the group/versions discovery reported as failed
	// (IsGroupDiscoveryFailedError). Their previously-known records are retained with
	// last-known facts, marked untrusted, for as long as the failure persists.
	FailedGroupVersions []schema.GroupVersion
	// Complete reports a scan with no discovery error: only then is a wholly
	// unscanned group/version's disappearance meaningful (and even then it rides the
	// removal grace, never an instant prune).
	Complete bool
	// Generation is the catalog's scan generation — bumped by the caller only when
	// the normalized facts changed, so registry revisions do not churn on steady
	// rescans.
	Generation uint64
}

// UpdateFromScan publishes one normalized discovery scan, applying the unified
// "additions fast, removals slow" policy in this one place:
//
//   - entries in the scan become fresh trusted records (additions fast);
//   - a previously-known record whose group/version FAILED keeps its last-known
//     facts marked untrusted — VerdictRetained for as long as the error persists
//     (retain-on-error, moved here from the catalog);
//   - a previously-known record whose group/version was scanned (or which is gone
//     from a complete scan) and is missing goes absent and rides the EXISTING
//     RemovalGrace — Retained for the grace, then dropped (removals slow, with no
//     instant prune anywhere);
//   - on an incomplete scan, an unscanned, non-failed group/version's records are
//     carried unchanged: an incomplete scan judges nothing it did not see.
//
// A record already inside its removal grace is never resurrected by a carry-forward;
// its absence clock keeps running until the type is freshly observed.
func (r *Registry) UpdateFromScan(scan Scan) {
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()

	observations := ObservationsFromEntries(scan.Entries, true)
	observations = append(observations, r.carriedObservations(scan, observations)...)
	r.update(observations, scan.Generation)
}

// carriedObservations synthesizes the carry-forward set for one scan: the stored
// observations of previously-known records the scan could not re-confirm but must not
// let go absent (failed group/versions, and unscanned ones on an incomplete scan).
// Callers hold dispatchMu; r.mu is taken for the read.
func (r *Registry) carriedObservations(scan Scan, fresh []Observation) []Observation {
	freshKeys := make(map[recordKey]struct{}, len(fresh))
	for _, obs := range fresh {
		freshKeys[observationKey(obs)] = struct{}{}
	}
	failed := groupVersionSet(scan.FailedGroupVersions)
	scanned := groupVersionSet(scan.ScannedGroupVersions)

	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Observation
	for key, prev := range r.entries {
		if _, ok := freshKeys[key]; ok {
			continue // freshly observed; the scan's record wins
		}
		if !prev.absentSince.IsZero() {
			continue // already mid-grace; the absence clock keeps running
		}
		gv := key.gvr.GroupVersion()
		if _, isFailed := failed[gv]; isFailed {
			obs := prev.obs
			obs.Served = true
			obs.Trusted = false
			obs.AbsenceExpired = false
			out = append(out, obs)
			continue
		}
		if _, wasScanned := scanned[gv]; wasScanned || scan.Complete {
			continue // meaningfully absent → retainAbsentLocked applies the grace
		}
		// Incomplete scan, group/version neither scanned nor failed: no judgement.
		out = append(out, prev.obs)
	}
	return out
}

func groupVersionSet(gvs []schema.GroupVersion) map[schema.GroupVersion]struct{} {
	out := make(map[schema.GroupVersion]struct{}, len(gvs))
	for _, gv := range gvs {
		out[gv] = struct{}{}
	}
	return out
}
