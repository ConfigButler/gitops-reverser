// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// WriteExclusion is one rule's negative clause: the field managers and identities whose
// writes that rule declines to mirror. The zero value excludes nothing.
type WriteExclusion struct {
	// FieldManagers is the sorted, de-duplicated set from ResourceRule.ExcludeFieldManagers.
	FieldManagers []string
	// Users is the sorted, de-duplicated set from ResourceRule.ExcludeUsers.
	Users []string
}

// newWriteExclusion normalizes a rule's two exclusion slices so two rules that declare the
// same exclusions in a different order fold together in the watched-type table.
func newWriteExclusion(fieldManagers, users []string) WriteExclusion {
	return WriteExclusion{
		FieldManagers: normalizeExclusionList(fieldManagers),
		Users:         normalizeExclusionList(users),
	}
}

func normalizeExclusionList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// Empty reports whether this clause excludes nothing, the common case.
func (e WriteExclusion) Empty() bool {
	return len(e.FieldManagers) == 0 && len(e.Users) == 0
}

// Key is a stable fingerprint used to fold rules with identical exclusions and to detect
// when a rule change must restart a watch.
func (e WriteExclusion) Key() string {
	if e.Empty() {
		return ""
	}
	return strings.Join(e.FieldManagers, ",") + "|" + strings.Join(e.Users, ",")
}

// excludesUser reports whether this clause names the attributed identity. An unresolved
// author (empty username) never matches: see admits.
func (e WriteExclusion) excludesUser(username string) bool {
	if username == "" {
		return false
	}
	return containsString(e.Users, username)
}

// excludesLastWriters reports whether this clause names the writer of a change.
//
// lastWriters is the set of field managers that share the newest managedFields timestamp.
// The clause excludes the change only when EVERY one of them is listed: a tie means we
// cannot tell which manager produced this particular write, and mirroring a machine's
// write is a smaller harm than dropping a human's. An object with no managedFields (an
// empty set) is never excluded, for the same reason.
func (e WriteExclusion) excludesLastWriters(lastWriters []string) bool {
	if len(e.FieldManagers) == 0 || len(lastWriters) == 0 {
		return false
	}
	for _, writer := range lastWriters {
		if !containsString(e.FieldManagers, writer) {
			return false
		}
	}
	return true
}

func containsString(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// RuleSelection is one compiled rule's admission clause for a watched type in one
// namespace: the operations it selects, and the writers it declines.
type RuleSelection struct {
	Ops       OperationSet
	Exclusion WriteExclusion
}

// RuleSelections are every rule clause that selects one (GitTarget, type, namespace).
// Rules are a logical OR, so an event is admitted when ANY clause admits it.
type RuleSelections []RuleSelection

// NeedsAuthor reports whether resolving the event's author is required to decide
// admission. Attribution costs a bounded wait (the grace window), so it is only paid when
// some clause actually declares excludeUsers.
func (s RuleSelections) NeedsAuthor() bool {
	for _, sel := range s {
		if len(sel.Exclusion.Users) > 0 {
			return true
		}
	}
	return false
}

// HasExclusions reports whether any clause declines any writer.
func (s RuleSelections) HasExclusions() bool {
	for _, sel := range s {
		if !sel.Exclusion.Empty() {
			return true
		}
	}
	return false
}

// Ops is the union of every clause's operations — what the watch must actually stream. A
// coarse op filter runs against this before admission is evaluated.
func (s RuleSelections) Ops() OperationSet {
	union := OperationSet{}
	for _, sel := range s {
		for op := range sel.Ops {
			union[op] = struct{}{}
		}
	}
	return union
}

// Key fingerprints the whole selection set so a rule change that alters operations or
// exclusions restarts the affected watch, and one that alters neither does not. It reads
// as "CREATE+UPDATE" for an unrestricted clause and "UPDATE/flux|;*/" when clauses differ.
func (s RuleSelections) Key() string {
	parts := make([]string, 0, len(s))
	for _, sel := range s {
		part := strings.Join(sel.Ops.Sorted(), "+")
		if !sel.Exclusion.Empty() {
			part += "/" + sel.Exclusion.Key()
		}
		parts = append(parts, part)
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

// Admits decides whether a live change is mirrored, implementing the OR over rules:
//
//	route ⟺ ∃ rule : rule selects op ∧ writer ∉ rule.excludeFieldManagers ∧ user ∉ rule.excludeUsers
//
// An exclusion is a veto within its own rule, never a global filter, so an unrestricted
// rule for the same type re-admits everything a restricted one excluded.
//
// lastWriters is empty for a DELETE: managedFields names who last wrote the object, not
// who deleted it, so a field-manager exclusion must not suppress deletes. username is
// empty when the author was not resolved, which makes every user clause fail open.
func (s RuleSelections) Admits(op string, lastWriters []string, username string) bool {
	for _, sel := range s {
		if !sel.Ops.Match(op) {
			continue
		}
		if sel.Exclusion.excludesLastWriters(lastWriters) {
			continue
		}
		if sel.Exclusion.excludesUser(username) {
			continue
		}
		return true
	}
	return false
}

// ExclusionReason names why an event was dropped, for the excluded-events metric. It
// re-walks the clauses only on the drop path, which is off the hot path by construction.
func (s RuleSelections) ExclusionReason(op string, lastWriters []string, username string) string {
	byFieldManager := false
	for _, sel := range s {
		if !sel.Ops.Match(op) {
			continue
		}
		if sel.Exclusion.excludesLastWriters(lastWriters) {
			byFieldManager = true
			continue
		}
		if sel.Exclusion.excludesUser(username) {
			return exclusionReasonUser
		}
	}
	if byFieldManager {
		return exclusionReasonFieldManager
	}
	return exclusionReasonUnknown
}

const (
	exclusionReasonFieldManager = "field_manager"
	exclusionReasonUser         = "user"
	exclusionReasonUnknown      = "unknown"
)

// warnIfExcludeUsersWithoutAttribution logs, once per rule, that an excludeUsers clause
// can never match because no author is ever resolved. The clause fails open, so the
// symptom is a rule that silently does nothing — which is worth one loud line. Rule
// resolution runs on every reconcile, so the warning is edge-triggered on first sight.
func (m *Manager) warnIfExcludeUsersWithoutAttribution(ruleRef string, exclusion WriteExclusion) {
	if len(exclusion.Users) == 0 || m.AuthorResolver != nil {
		return
	}
	if _, warned := m.excludeUsersWarned.LoadOrStore(ruleRef, struct{}{}); warned {
		return
	}
	m.Log.Info("WARNING: excludeUsers has no effect without author attribution; "+
		"every write will be mirrored. Enable --author-attribution, or use excludeFieldManagers, "+
		"which reads the object's managedFields and needs no audit fact.",
		"rule", ruleRef, "excludeUsers", exclusion.Users)
}

// recordExcludedWatchEvent counts one live event a rule declined to mirror, so an operator
// can see the forward leg's applies being dropped rather than committed. No-op until the
// counter is registered.
func (m *Manager) recordExcludedWatchEvent(
	gitDest types.ResourceReference,
	gvr schema.GroupVersionResource,
	reason string,
) {
	if telemetry.WatchEventsExcludedTotal == nil {
		return
	}
	telemetry.WatchEventsExcludedTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("gittarget_namespace", gitDest.Namespace),
		attribute.String("gittarget_name", gitDest.Name),
		attribute.String("group", gvr.Group),
		attribute.String("resource", gvr.Resource),
		attribute.String("reason", reason),
	))
}

// lastWriteFieldManagers returns the field managers of the managedFields entries carrying
// the newest timestamp — the managers that could have produced this write.
//
// Entries with no timestamp are ignored: they cannot be compared, and an entry the API
// server wrote without one is not evidence of recency. Entries for a subresource are
// counted like any other, because a status write is still a write by that manager; the
// content dedup upstream already drops status-only churn that carries no Git change.
//
// The result is sorted and de-duplicated. It is empty when the object has no usable
// managedFields, which makes every field-manager exclusion fail open.
func lastWriteFieldManagers(u *unstructured.Unstructured) []string {
	if u == nil {
		return nil
	}
	entries := u.GetManagedFields()
	if len(entries) == 0 {
		return nil
	}

	var newest *metav1.Time
	for i := range entries {
		ts := entries[i].Time
		if ts == nil || ts.IsZero() {
			continue
		}
		if newest == nil || ts.After(newest.Time) {
			newest = ts
		}
	}
	if newest == nil {
		return nil
	}

	seen := map[string]struct{}{}
	writers := make([]string, 0, 2)
	for i := range entries {
		ts := entries[i].Time
		if ts == nil || !ts.Equal(newest) {
			continue
		}
		manager := strings.TrimSpace(entries[i].Manager)
		if manager == "" {
			continue
		}
		if _, dup := seen[manager]; dup {
			continue
		}
		seen[manager] = struct{}{}
		writers = append(writers, manager)
	}
	sort.Strings(writers)
	return writers
}

// lastWritersForOperation resolves the field managers a field-manager exclusion may be
// evaluated against. A removal returns none: managedFields names the last writer, and a
// human deleting a Flux-managed object would otherwise be silently ignored — the exact
// failure a label selector has, and the reason this filter reads the write rather than
// the object.
func lastWritersForOperation(op string, u *unstructured.Unstructured) []string {
	if op == string(configv1alpha3.OperationDelete) {
		return nil
	}
	return lastWriteFieldManagers(u)
}
