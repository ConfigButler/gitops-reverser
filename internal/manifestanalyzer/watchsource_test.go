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

import "testing"

func TestNoWatchSource(t *testing.T) {
	if got := (NoWatchSource{}).WatchStateFor(GVK{Version: "v1", Kind: "Pod"}); got != WatchUnknown {
		t.Errorf("NoWatchSource = %s, want unknown", got)
	}
}

func TestStaticWatchSource(t *testing.T) {
	ws := NewStaticWatchSource([]GVK{{Group: "apps", Version: "v1", Kind: "Deployment"}})
	if got := ws.WatchStateFor(GVK{Group: "apps", Version: "v1", Kind: "Deployment"}); got != WatchWatched {
		t.Errorf("Deployment = %s, want watched", got)
	}
	if got := ws.WatchStateFor(GVK{Version: "v1", Kind: "ConfigMap"}); got != WatchUnwatched {
		t.Errorf("ConfigMap = %s, want unwatched", got)
	}
}

func TestParseGVKRef(t *testing.T) {
	good := []struct {
		ref  string
		want GVK
	}{
		{"apps/v1/Deployment", GVK{"apps", "v1", "Deployment"}},
		{"v1/ConfigMap", GVK{"", "v1", "ConfigMap"}},
		{"/v1/Secret", GVK{"", "v1", "Secret"}},
		{"  apps/v1/StatefulSet  ", GVK{"apps", "v1", "StatefulSet"}},
	}
	for _, c := range good {
		got, err := ParseGVKRef(c.ref)
		if err != nil {
			t.Errorf("ParseGVKRef(%q) error: %v", c.ref, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseGVKRef(%q) = %+v, want %+v", c.ref, got, c.want)
		}
	}

	bad := []string{"", "Deployment", "a/b/c/d", "/v1/", "v1/"}
	for _, ref := range bad {
		if _, err := ParseGVKRef(ref); err == nil {
			t.Errorf("ParseGVKRef(%q) expected error", ref)
		}
	}
}
