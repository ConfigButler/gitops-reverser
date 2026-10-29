// SPDX-License-Identifier: Apache-2.0

package git

import (
	"maps"
	"reflect"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// A withheld apply (apply_window.go) is not mirrored, but it did land: the cluster runs the values it
// reverted until a Repair puts the edit back. A window holds whole objects, so the next event of the
// object, by anyone, carries each reverted value its author did not change. Committed as it came, it
// wrote the apply's value under that author's name, the edit the apply overwrote was lost, and the
// Repair then applied the loss.
//
// So the worker records, per object, the fields a withheld apply reverted: what the object's last
// accepted version held there, and what the apply wrote. The last accepted version is the object of
// the last event an apply window or a flush recorded as an edit (keepAccepted). This is the desired
// view of docs/design/window-field-ownership-and-apply-hold.md, limited to the fields a withheld apply
// reverted; commits stay whole objects, and no window owns fields.
//
// Every later event of the object, mirrored or not (the Repair's own landing included), and every
// snapshot of it a resync decides, is restored against the record: a field still holding the apply's
// value was carried over, and is set back to the accepted value; a field holding any other value was
// changed by someone since, and leaves the record. A delete clears the object's record.
//
// A delete is accepted too, as the object's absence. A withheld apply that makes the object again is
// recorded as one field, the whole object (an empty Path): every later event of the object is kept
// out of Git, and every resync snapshot of it left out, until a delete, the Repair's own included,
// clears the record. No update can be told from carrying the apply's object: an author who changed
// some field of it still changed an object only the apply made.
//
// The record also ends when nothing will put the accepted version back: the applier claim that would
// repair it is gone (dropWithheldOf), or a resync of the object's collection no longer finds it.
//
// Decided limits: a list is one field, so an edit to another item of a list the apply reverted still
// carries the reverted item; an author setting a field to the apply's value cannot be told from
// carrying it, and is set back; an object made again stays out of Git until it is deleted, so an
// applier that does not prune it keeps it out for as long as the record lives; and the record lives
// in memory, so a restart loses it.

// withheldFields is, by withheldKey, the fields withheld applies reverted that no event has changed
// since. It is plain data.
type withheldFields map[withheldKey]withheldObject

// withheldKey names an object as one GitTarget mirrors it. Two GitTargets on one branch can mirror
// objects of one name, from two clusters or into two folders: each has an accepted version and a
// record of its own, and an apply withheld for one never changes or holds out the other's.
type withheldKey struct {
	GitTarget itypes.ResourceReference
	Object    string
}

// withheldKeyOf is the withheldKey of a live event's object.
func withheldKeyOf(event Event) withheldKey {
	return withheldKey{
		GitTarget: itypes.NewResourceReference(event.GitTargetName, event.GitTargetNamespace),
		Object:    objectKey(event.Identifier),
	}
}

// withheldObject is the record of one object: the GitTarget it is mirrored for and the object, which
// a resync and an applier claim's removal match it by, and its fields.
type withheldObject struct {
	GitTarget  itypes.ResourceReference
	Identifier itypes.ResourceIdentifier
	Fields     []withheldField
}

// withheldField is one field a withheld apply reverted.
type withheldField struct {
	// Path is the field's map keys from the object's root. A list is one field: a path never goes
	// into one. An empty Path is the whole object, which the apply made again over a delete.
	Path []string
	// Accepted is what the object's last accepted version held at Path, Applier what the withheld
	// apply wrote there.
	Accepted fieldValue
	Applier  fieldValue
}

// fieldValue is a field's value, or its absence.
type fieldValue struct {
	Value  any
	Absent bool
}

// equal reports whether two field values are the same value, or both absent.
func (v fieldValue) equal(other fieldValue) bool {
	return v.Absent == other.Absent && (v.Absent || reflect.DeepEqual(v.Value, other.Value))
}

// keepAccepted keeps a live event's object as its object's last accepted version, after an apply
// window or a flush recorded the event as an edit. A delete is kept as the object's absence, a nil
// version. A field patch, which carries no whole object, forgets it: with no version to compare with,
// a withhold records nothing.
func (l *branchWorkerEventLoop) keepAccepted(event Event) {
	key := withheldKeyOf(event)
	deleted := event.Operation == string(itypes.OperationDelete)
	if event.Object == nil && !deleted {
		delete(l.accepted, key)
		return
	}
	if l.accepted == nil {
		l.accepted = map[withheldKey]*unstructured.Unstructured{}
	}
	if deleted {
		l.accepted[key] = nil
		return
	}
	l.accepted[key] = event.Object.DeepCopy()
}

// forgetAccepted drops the last accepted versions of the objects no open apply window and no flush
// records an edit of: a write to one of those is never withheld (judgeInWindow).
func (l *branchWorkerEventLoop) forgetAccepted(now time.Time) {
	for key := range l.accepted {
		if !l.editRecorded(key, now) {
			delete(l.accepted, key)
		}
	}
}

// editRecorded reports whether an open apply window of an applier that covers the object's
// GitTarget, or the flush in progress for that GitTarget, records an edit of the object.
func (l *branchWorkerEventLoop) editRecorded(key withheldKey, now time.Time) bool {
	for _, state := range l.appliers {
		window := state.window
		if !window.open(now) || !slices.ContainsFunc(state.targets, func(target branchTarget) bool {
			return target.ref == key.GitTarget
		}) {
			continue
		}
		if _, edited := window.edited[key.Object]; edited {
			return true
		}
		if _, carried := window.carried[key.Object]; carried && window.carrying(now) {
			return true
		}
	}
	_, edited := l.flushEdits[key.GitTarget][key.Object]
	return edited
}

// recordWithheld records the fields a withheld apply reverted: where its object differs from the
// object's last accepted version, or, when that version is the object's absence, the whole object. A
// field recorded already is replaced, with any recorded inside it or around it. With no accepted
// version kept there is nothing to compare with, and nothing is recorded.
//
// The event was restored first (restoreWithheld), so a field it still carried from an earlier
// withheld apply holds the accepted value, differs in nothing, and keeps its record.
func (l *branchWorkerEventLoop) recordWithheld(event Event) {
	key := withheldKeyOf(event)
	accepted, kept := l.accepted[key]
	if !kept || event.Object == nil {
		return
	}
	var reverted []withheldField
	if accepted == nil {
		reverted = []withheldField{{
			Accepted: fieldValue{Absent: true},
			Applier:  fieldValue{Value: copyValue(event.Object.Object)},
		}}
	} else {
		reverted = diffFields(nil, accepted.Object, event.Object.Object)
	}
	if len(reverted) == 0 {
		return
	}
	fields := slices.DeleteFunc(slices.Clone(l.withheld[key].Fields), func(field withheldField) bool {
		return slices.ContainsFunc(reverted, func(other withheldField) bool { return overlaps(field.Path, other.Path) })
	})
	if l.withheld == nil {
		l.withheld = withheldFields{}
	}
	l.withheld[key] = withheldObject{
		GitTarget:  key.GitTarget,
		Identifier: event.Identifier,
		Fields:     append(fields, reverted...),
	}
	l.w.Log.V(1).Info("Recorded the fields a withheld apply reverted, so the next event of the object does "+
		"not carry them into Git", "branch", l.w.Branch, "object", event.Identifier.String(), "fields", len(reverted))
}

// restoreWithheldEvent restores a live event against the record of its object, before it is
// judged: a delete clears the record, and any other event with a whole object has each field it
// carried from a withheld apply set back (restoreWithheld). It reports whether the event is kept out
// of Git: its object was deleted, and exists again only because a withheld apply made it.
func (l *branchWorkerEventLoop) restoreWithheldEvent(event *Event) bool {
	key := withheldKeyOf(*event)
	if event.Operation == string(itypes.OperationDelete) {
		delete(l.withheld, key)
		return false
	}
	restored := l.restoreWithheld(key, event.Object)
	switch {
	case restored == nil && event.Object != nil:
		l.w.Log.Info("Kept an event out of Git: its object was deleted, and only a withheld apply made it again",
			"branch", l.w.Branch, "object", event.Identifier.String(), "author", event.UserInfo.Username)
		return true
	case restored != event.Object:
		l.w.Log.Info("Set back the fields a withheld apply reverted, which the event carried: the cluster "+
			"still runs the apply's values, and the event's author did not change them",
			"branch", l.w.Branch, "object", event.Identifier.String(), "author", event.UserInfo.Username)
		event.Object = restored
	}
	return false
}

// restoreWithheldSnapshot restores each object of a resync's snapshot against its record, as a live
// event is, and leaves out an object a record keeps out of Git. The request's snapshot is left as it
// was: an object set back is a copy, in a copy of it.
//
// The snapshot holds every object of its scope for its GitTarget, so a recorded object it does not
// hold is gone from the cluster, and its record ends.
func (l *branchWorkerEventLoop) restoreWithheldSnapshot(
	req *ResyncRequest, desired []manifestanalyzer.DesiredResource,
) []manifestanalyzer.DesiredResource {
	if len(l.withheld) == 0 {
		return desired
	}
	target := itypes.NewResourceReference(req.GitTargetName, req.GitTargetNamespace)
	found := make(map[withheldKey]bool, len(req.Desired))
	for _, dr := range req.Desired {
		found[withheldKey{GitTarget: target, Object: objectKey(dr.Resource)}] = true
	}
	for key, record := range l.withheld {
		if key.GitTarget == target && req.Scope.Matches(record.Identifier) && !found[key] {
			delete(l.withheld, key)
		}
	}
	restored := make([]manifestanalyzer.DesiredResource, 0, len(desired))
	changed := false
	for _, dr := range desired {
		object := l.restoreWithheld(withheldKey{GitTarget: target, Object: objectKey(dr.Resource)}, dr.Object)
		if object != dr.Object {
			changed = true
		}
		if object == nil && dr.Object != nil {
			continue
		}
		dr.Object = object
		restored = append(restored, dr)
	}
	if !changed {
		return desired
	}
	return restored
}

// dropWithheldOf ends the records of the objects of an applier claim that is gone: no Repair of its
// will put the accepted version back.
func (l *branchWorkerEventLoop) dropWithheldOf(state *applierState) {
	for key := range l.withheld {
		if slices.ContainsFunc(state.targets, func(target branchTarget) bool { return target.ref == key.GitTarget }) {
			delete(l.withheld, key)
		}
	}
}

// restoreWithheld applies the record of an object to one of its later versions, and updates the
// record: a field still holding the withheld apply's value is set back to the accepted value, and
// stays recorded; a field holding any other value leaves the record. It returns the object to
// mirror, a copy when a field was set back, and nil while the record keeps the whole object out of
// Git, which no later version ends.
func (l *branchWorkerEventLoop) restoreWithheld(
	key withheldKey, object *unstructured.Unstructured,
) *unstructured.Unstructured {
	fields := l.withheld[key].Fields
	if len(fields) == 0 || object == nil {
		return object
	}
	if slices.ContainsFunc(fields, func(field withheldField) bool { return len(field.Path) == 0 }) {
		return nil
	}
	restored := object
	var kept []withheldField
	for _, field := range fields {
		value, ok := lookupField(object.Object, field.Path)
		if !ok || !value.equal(field.Applier) {
			continue
		}
		kept = append(kept, field)
		if restored == object {
			restored = object.DeepCopy()
		}
		setField(restored.Object, field.Path, field.Accepted)
	}
	if len(kept) == 0 {
		delete(l.withheld, key)
	} else {
		record := l.withheld[key]
		record.Fields = kept
		l.withheld[key] = record
	}
	return restored
}

// diffFields lists the fields where applier differs from accepted, under path. Two maps are compared
// key by key; anything else, a list included, is one field.
func diffFields(path []string, accepted, applier map[string]any) []withheldField {
	var fields []withheldField
	keys := slices.Collect(maps.Keys(accepted))
	for key := range applier {
		if _, both := accepted[key]; !both {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		at := append(slices.Clone(path), key)
		was, inAccepted := accepted[key]
		now, inApplier := applier[key]
		wasMap, wasIsMap := was.(map[string]any)
		nowMap, nowIsMap := now.(map[string]any)
		switch {
		case inAccepted && inApplier && wasIsMap && nowIsMap:
			fields = append(fields, diffFields(at, wasMap, nowMap)...)
		case inAccepted && inApplier && reflect.DeepEqual(was, now):
		default:
			fields = append(fields, withheldField{
				Path:     at,
				Accepted: fieldValue{Value: copyValue(was), Absent: !inAccepted},
				Applier:  fieldValue{Value: copyValue(now), Absent: !inApplier},
			})
		}
	}
	return fields
}

// lookupField is the value at path in object, or its absence. It is not ok when the path runs
// through something that is not a map: whatever is there, it is not what the record compares.
func lookupField(object map[string]any, path []string) (fieldValue, bool) {
	current := object
	for i, key := range path {
		value, found := current[key]
		if !found {
			return fieldValue{Absent: true}, true
		}
		if i == len(path)-1 {
			return fieldValue{Value: value}, true
		}
		next, isMap := value.(map[string]any)
		if !isMap {
			return fieldValue{}, false
		}
		current = next
	}
	return fieldValue{}, false
}

// setField sets the field at path in object to value, or removes it when value is absent. A map on
// the way that is missing is created.
func setField(object map[string]any, path []string, value fieldValue) {
	current := object
	for _, key := range path[:len(path)-1] {
		next, isMap := current[key].(map[string]any)
		if !isMap {
			if value.Absent {
				return
			}
			next = map[string]any{}
			current[key] = next
		}
		current = next
	}
	last := path[len(path)-1]
	if value.Absent {
		delete(current, last)
		return
	}
	current[last] = copyValue(value.Value)
}

// overlaps reports whether one path is the other or lies inside it.
func overlaps(a, b []string) bool {
	n := min(len(a), len(b))
	return slices.Equal(a[:n], b[:n])
}

// copyValue deep-copies a value of an unstructured object.
func copyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, item := range typed {
			copied[key] = copyValue(item)
		}
		return copied
	case []any:
		copied := make([]any, len(typed))
		for i, item := range typed {
			copied[i] = copyValue(item)
		}
		return copied
	default:
		return value
	}
}
