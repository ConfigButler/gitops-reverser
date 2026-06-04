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

package manifestanalyzer

import (
	"fmt"
	"strings"
)

// WatchState is the analyzer's verdict on whether a GVK is a watched API
// resource. Unknown is a first-class value: it means no API truth was injected,
// not "absent from the API".
type WatchState string

const (
	// WatchUnknown means no WatchSource could decide (no API truth available).
	WatchUnknown WatchState = "unknown"
	// WatchWatched means the GVK is a watched API resource.
	WatchWatched WatchState = "watched"
	// WatchUnwatched means the GVK is not a watched API resource.
	WatchUnwatched WatchState = "unwatched"
)

// WatchSource is the injectable "what is in the API" dependency. Implementations
// may be backed by live informers (controller), a cluster client or static list
// (CLI), or nothing at all (structure-only analysis).
type WatchSource interface {
	// WatchStateFor reports whether the analyzer treats a GVK as watched.
	WatchStateFor(gvk GVK) WatchState
}

// NoWatchSource has no API truth and reports every GVK as unknown. It is the
// default, enabling structure-only analysis with no cluster.
type NoWatchSource struct{}

// WatchStateFor always returns WatchUnknown.
func (NoWatchSource) WatchStateFor(GVK) WatchState { return WatchUnknown }

// StaticWatchSource decides watch state against a fixed set of GVKs. A GVK in
// the set is watched; any other GVK is unwatched.
type StaticWatchSource struct {
	watched map[GVK]struct{}
}

// NewStaticWatchSource builds a StaticWatchSource from a list of watched GVKs.
func NewStaticWatchSource(gvks []GVK) StaticWatchSource {
	m := make(map[GVK]struct{}, len(gvks))
	for _, g := range gvks {
		m[g] = struct{}{}
	}
	return StaticWatchSource{watched: m}
}

// WatchStateFor returns WatchWatched for GVKs in the set, WatchUnwatched otherwise.
func (s StaticWatchSource) WatchStateFor(gvk GVK) WatchState {
	if _, ok := s.watched[gvk]; ok {
		return WatchWatched
	}
	return WatchUnwatched
}

// Number of slash-separated parts in a GVK reference: "version/kind" (core) or
// "group/version/kind".
const (
	gvkRefCoreParts = 2
	gvkRefFullParts = 3
)

// ParseGVKRef parses a "group/version/kind" or "version/kind" (core) reference,
// such as "apps/v1/Deployment" or "v1/ConfigMap".
func ParseGVKRef(ref string) (GVK, error) {
	parts := strings.Split(strings.TrimSpace(ref), "/")
	var g GVK
	switch len(parts) {
	case gvkRefCoreParts:
		g = GVK{Version: parts[0], Kind: parts[1]}
	case gvkRefFullParts:
		g = GVK{Group: parts[0], Version: parts[1], Kind: parts[2]}
	default:
		return GVK{}, fmt.Errorf("invalid GVK reference %q: want group/version/kind or version/kind", ref)
	}
	if g.Version == "" || g.Kind == "" {
		return GVK{}, fmt.Errorf("invalid GVK reference %q: version and kind are required", ref)
	}
	return g, nil
}
