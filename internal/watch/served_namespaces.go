// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// ServedNamespaces is the set of config-plane namespaces one operator instance reconciles, from
// --watch-namespaces. The zero value serves every namespace.
//
// The manager's cache already confines the namespaced kinds to the set. A ClusterWatchRule is
// cluster-scoped, so every instance sees every one; it belongs to the instance serving the
// namespace of the GitTarget it names, and the others leave it alone, status included.
type ServedNamespaces struct {
	names map[string]struct{}
}

// ParseServedNamespaces reads a comma-separated list of namespace names. Empty serves every
// namespace. A name that is not a valid namespace name, or an empty entry, is an error.
func ParseServedNamespaces(raw string) (ServedNamespaces, error) {
	if strings.TrimSpace(raw) == "" {
		return ServedNamespaces{}, nil
	}
	names := make(map[string]struct{})
	for entry := range strings.SplitSeq(raw, ",") {
		name := strings.TrimSpace(entry)
		if name == "" {
			return ServedNamespaces{}, fmt.Errorf("--watch-namespaces %q has an empty entry", raw)
		}
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			return ServedNamespaces{}, fmt.Errorf("--watch-namespaces: %q is not a namespace name: %s",
				name, strings.Join(errs, "; "))
		}
		names[name] = struct{}{}
	}
	return ServedNamespaces{names: names}, nil
}

// All reports whether every namespace is served.
func (s ServedNamespaces) All() bool { return len(s.names) == 0 }

// Serves reports whether namespace is one this instance reconciles.
func (s ServedNamespaces) Serves(namespace string) bool {
	if s.All() {
		return true
	}
	_, ok := s.names[namespace]
	return ok
}

// Names returns the served namespaces, sorted; nil when every namespace is served.
func (s ServedNamespaces) Names() []string {
	if s.All() {
		return nil
	}
	out := make([]string, 0, len(s.names))
	for name := range s.names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
