// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-logr/logr"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// watchSpec is everything about one collection's stream that, when it changes, invalidates the running
// one: the served version the watch is opened at.
//
// The version is spec DATA rather than identity. A collection is versionless
// (see [types.CollectionKey]), so a storage-version bump is one collection whose spec changed — a
// `restart` — and not the retirement of one key plus the birth of another. Diffing the
// declared targetWatchKeys directly would classify it the second way, which would replay
// the collection AND drop its readiness result rather than replacing the stream in place.
type watchSpec struct {
	// Version is the served version the stream opens its watch at.
	Version string
}

// targetWatchPlan is the desired watch set of one GitTarget, keyed by collection.
type targetWatchPlan struct {
	Collections map[types.CollectionKey]watchSpec
}

// targetWatchPlanDiff is the classification of a previous plan against a desired one. Every collection
// named by either plan appears in exactly one of the four lists, each sorted for a stable log.
//
// It is what the streams are driven from: keep leaves a stream and its readiness result alone,
// start and restart open one, and stop cancels one and drops its key without touching files.
type targetWatchPlanDiff struct {
	// Keep is the collections whose key and specification are unchanged.
	Keep []types.CollectionKey
	// Start is the collections only in the desired plan.
	Start []types.CollectionKey
	// Restart is the collections in both plans whose specification changed — and, on a forced
	// recheck, every desired collection.
	Restart []types.CollectionKey
	// Stop is the collections only in the previous plan.
	Stop []types.CollectionKey
}

// targetWatchPlanFor re-keys the declared streams by collection, carrying the served version across
// as spec data.
//
// [targetWatchStreams] guarantees one stream per collection, so the re-keying cannot collide — but a
// collision would silently drop one of the two streams from the plan, and the plan is what the
// streams are started from, so it is asserted here rather than assumed.
func targetWatchPlanFor(keys []targetWatchKey) (targetWatchPlan, error) {
	plan := targetWatchPlan{Collections: make(map[types.CollectionKey]watchSpec, len(keys))}
	for _, key := range keys {
		collection := key.Collection()
		if prior, seen := plan.Collections[collection]; seen {
			return targetWatchPlan{}, fmt.Errorf(
				"two declared streams share the collection %s: served versions %s and %s",
				collection, prior.Version, key.GVR.Version)
		}
		plan.Collections[collection] = watchSpec{Version: key.GVR.Version}
	}
	return plan, nil
}

// diffTargetWatchPlans classifies previous against desired, per the table in
// docs/design/target-watch-plan.md, "Diff the plan".
//
// `force` is a forced recovery, not a fifth outcome: it classifies every desired collection as a
// restart, including one the previous plan never held, so a recheck reopens the whole target
// without a state machine of its own. Collections the desired plan dropped are still `stop`.
func diffTargetWatchPlans(previous, desired targetWatchPlan, force bool) targetWatchPlanDiff {
	var diff targetWatchPlanDiff
	for collection, want := range desired.Collections {
		switch have, running := previous.Collections[collection]; {
		case force:
			diff.Restart = append(diff.Restart, collection)
		case !running:
			diff.Start = append(diff.Start, collection)
		case have != want:
			diff.Restart = append(diff.Restart, collection)
		default:
			diff.Keep = append(diff.Keep, collection)
		}
	}
	for collection := range previous.Collections {
		if _, wanted := desired.Collections[collection]; !wanted {
			diff.Stop = append(diff.Stop, collection)
		}
	}
	sortCollections(diff.Keep)
	sortCollections(diff.Start)
	sortCollections(diff.Restart)
	sortCollections(diff.Stop)
	return diff
}

func sortCollections(collections []types.CollectionKey) {
	sort.Slice(collections, func(i, j int) bool {
		if collections[i].Group != collections[j].Group {
			return collections[i].Group < collections[j].Group
		}
		if collections[i].Resource != collections[j].Resource {
			return collections[i].Resource < collections[j].Resource
		}
		if collections[i].Namespace != collections[j].Namespace {
			return collections[i].Namespace < collections[j].Namespace
		}
		return collections[i].LabelSelector < collections[j].LabelSelector
	})
}

// describeCollections renders collections as "<collection>@<version>", the same convention
// [describeWatchKeys] uses: name every stream rather than only counting them, because a count
// cannot tell an operator WHICH collection a plan change is about to replay.
func describeCollections(collections []types.CollectionKey, plan targetWatchPlan) string {
	parts := make([]string, 0, len(collections))
	for _, collection := range collections {
		spec := plan.Collections[collection]
		parts = append(parts, fmt.Sprintf("%s@%s", collection, spec.Version))
	}
	return strings.Join(parts, " | ")
}

// logTargetWatchPlanDiff records the classification once per reconcile, naming every collection it
// acted on. An all-keep reconcile is logged too: "nothing changed" is the most common outcome
// and the one an operator most wants confirmed.
func logTargetWatchPlanDiff(log logr.Logger, previous, desired targetWatchPlan, diff targetWatchPlanDiff) {
	kv := []any{
		"keep", len(diff.Keep),
		"start", len(diff.Start),
		"restart", len(diff.Restart),
		"stop", len(diff.Stop),
	}
	// A stopped collection is gone from the desired plan, so its specification only survives on the
	// previous one.
	for _, named := range []struct {
		key         string
		collections []types.CollectionKey
		plan        targetWatchPlan
	}{
		{"keepCollections", diff.Keep, desired},
		{"startCollections", diff.Start, desired},
		{"restartCollections", diff.Restart, desired},
		{"stopCollections", diff.Stop, previous},
	} {
		if len(named.collections) > 0 {
			kv = append(kv, named.key, describeCollections(named.collections, named.plan))
		}
	}
	log.Info("target watch plan reconciled", kv...)
}
