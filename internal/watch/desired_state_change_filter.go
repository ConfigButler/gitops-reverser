// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"crypto/sha256"
	"encoding/json"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// desiredStateChangeFilter is one stream's memory of the desired state the branch worker last
// accepted for each object it delivers, keyed by UID. It passes CREATE, DELETE, and every UPDATE
// that changes Git-visible content, retained labels and annotations included, and drops an UPDATE
// whose sanitized content equals that baseline: the classic /status-only update.
//
// Dropping it is not about Git content, since a routed no-op finds no diff. A /status update has no
// audit author (the policy drops /status writes), so routing one would close a named author's open
// commit window on the author change and split a save in two. It is also most of the churn.
//
// The stream owns the map, and only the stream goroutine reads or writes it. It survives the
// stream's reconnects and goes with the stream. Two things write it, both only once the worker
// took what they describe: an accepted live event records its own object (accept), and an accepted
// replay snapshot replaces the whole map (adopt), so a baseline older than the replay never
// outlives it. One stream per object, which collection-overlap refusal guarantees per GitTarget, is
// what makes a per-stream baseline the worker's.
//
// A missing baseline and content that cannot be hashed both pass: never drop a change that cannot
// be proven a no-op.
type desiredStateChangeFilter struct {
	baselines map[k8stypes.UID]string
}

func newDesiredStateChangeFilter() *desiredStateChangeFilter {
	return &desiredStateChangeFilter{baselines: map[k8stypes.UID]string{}}
}

// desiredStateChange is one live event's verdict, taken before it is routed and recorded only
// once the worker accepted it.
type desiredStateChange struct {
	uid    k8stypes.UID
	remove bool
	hash   string
	hashed bool
}

// check reports whether a live event changes no desired state the worker accepted. It reads the map
// and never writes it: recording before the worker took the event let a refused UPDATE, redelivered
// from the unchanged cursor, match its own hash and be skipped.
//
// A nil filter passes everything.
func (f *desiredStateChangeFilter) check(
	u *unstructured.Unstructured, event *git.Event, op string,
) (desiredStateChange, bool) {
	change := desiredStateChange{uid: u.GetUID()}
	if op == string(types.OperationDelete) {
		change.remove = true
		return change, false
	}
	change.hash, change.hashed = desiredStateHash(event.Object)
	if f == nil || !change.hashed || change.uid == "" {
		return change, false
	}
	baseline, ok := f.baselines[change.uid]
	return change, op == string(types.OperationUpdate) && ok && baseline == change.hash
}

// accept records what the worker just accepted: a DELETE clears the object's baseline, so a
// recreate under the same UID can never match its predecessor, and anything else stores its hash.
func (f *desiredStateChangeFilter) accept(change desiredStateChange) {
	if f == nil || change.uid == "" {
		return
	}
	if change.remove {
		delete(f.baselines, change.uid)
		return
	}
	if change.hashed {
		f.baselines[change.uid] = change.hash
	}
}

// adopt replaces every baseline with an accepted snapshot's. An object the snapshot does not hold
// loses its baseline with it.
func (f *desiredStateChangeFilter) adopt(baselines map[k8stypes.UID]string) {
	if f == nil {
		return
	}
	if baselines == nil {
		baselines = map[k8stypes.UID]string{}
	}
	f.baselines = baselines
}

// desiredStateHash hashes a sanitized object, the content Git holds for it. ok=false means it cannot
// be hashed (nil, or a marshal error).
func desiredStateHash(obj *unstructured.Unstructured) (string, bool) {
	if obj == nil {
		return "", false
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(raw)
	return string(sum[:]), true
}

// replaySnapshot is one session's snapshot as it is gathered: the desired set the resync carries,
// and each object's desired-state hash by UID, for the stream's filter to adopt once the worker
// accepted the resync. A snapshot that is refused, or a session that ends before it is complete,
// is discarded with both.
type replaySnapshot struct {
	desired   []manifestanalyzer.DesiredResource
	baselines map[k8stypes.UID]string
}

// add folds one observed object into the snapshot. An object desiredFromObject excludes (a
// Terminating one) gets no baseline either: Git does not hold it.
func (r *replaySnapshot) add(gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	desired, ok := desiredFromObject(gvr, u)
	if !ok {
		return
	}
	r.desired = append(r.desired, desired)
	if r.baselines == nil {
		r.baselines = map[k8stypes.UID]string{}
	}
	if hash, hashed := desiredStateHash(desired.Object); hashed && u.GetUID() != "" {
		r.baselines[u.GetUID()] = hash
	}
}

// snapshotFromList folds a LIST fallback's items exactly as a replay folds its events.
func snapshotFromList(gvr schema.GroupVersionResource, list *unstructured.UnstructuredList) replaySnapshot {
	var snapshot replaySnapshot
	if list == nil {
		return snapshot
	}
	for i := range list.Items {
		snapshot.add(gvr, &list.Items[i])
	}
	return snapshot
}
