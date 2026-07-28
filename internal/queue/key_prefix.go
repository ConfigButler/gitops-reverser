// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"fmt"
	"strings"
)

// maxKeyPrefixLength bounds the root namespace. Redis keys are capped at 512MB, so this
// is not a protocol limit — it is a legibility limit, and a guard against a prefix that
// dwarfs the key it namespaces.
const maxKeyPrefixLength = 128

// DefaultKeyPrefix is the root namespace every Redis key (cursors, fact streams, and command
// author records alike) carries when --redis-key-prefix is not set. It is also the value every
// release before the flag existed used, so the default is a no-op upgrade.
const DefaultKeyPrefix = "gitops-reverser"

// routeKeyInfix carries the AUDIT ROUTE dimension so a fact from cluster A never joins a watch
// event from cluster B — the rv-only hatch especially, since RV is not globally unique. The route
// is what the audit events arrived under, NOT the ClusterProvider's name: an API server has one
// webhook backend and posts under one route, so several providers naming one cluster all declare
// that route and share its facts (ClusterProvider.AuditRoute()). It is spelled "route" rather than
// "auditRoute" because the key already says audit one segment earlier, and it sits directly after
// the domain suffix so one route's streams share a single glob prefix.
const routeKeyInfix = "route:"

// groupResourceKey renders a GroupResource as an API-path-style segment: "configmaps" for the core
// group, "apps/deployments" otherwise. Publish side, follow side, and the index share it so the
// name never drifts. "/" never appears in a group or resource name, so the form stays unambiguously
// splittable — unlike schema.GroupResource.String()'s reversed dot form ("deployments.apps"), whose
// dot also collides with dotted group names.
func groupResourceKey(group, resource string) string {
	if group == "" {
		return resource
	}
	return group + "/" + resource
}

// escapeKeyField neutralizes the ":" delimiter and the "%" escape character within a single key
// field. Group/resource and a UUID never contain either, so this is defensive for the uid and route
// fields against a stray delimiter; everything else passes through unchanged for readability. Keys
// are only ever matched exactly, never parsed back, so escaping is one-way.
func escapeKeyField(s string) string {
	if !strings.ContainsAny(s, "%:") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		switch s[i] {
		case '%':
			b.WriteString("%25")
		case ':':
			b.WriteString("%3A")
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// ValidateKeyPrefix checks a --redis-key-prefix value and returns its normalized form.
//
// Two independent constraints shape the allowed character set:
//
//   - A prefix names a keyspace an operator inspects and, on a bad day, deletes by glob. Redis
//     glob metacharacters (*, ?, [, ], \) in it would make "<prefix>:*" match more than this
//     install's keys, so they are rejected rather than escaped — a prefix is an operator-chosen
//     identifier, not user data.
//   - Key fields (uid, resourceVersion, namespace) are ':'-delimited and %-escaped by
//     escapeKeyField. '%' in the prefix would make an escaped key ambiguous with an
//     unescaped one, so it is rejected too.
//
// ':' is allowed, because a prefix like "cell-a:tenant-7" is a natural nesting and every
// suffix constant already begins with ':'. A trailing ':' is normalized away so
// "tenant-7:" and "tenant-7" name the same keyspace rather than two that differ by an
// empty segment.
//
// An empty prefix is rejected: an unprefixed keyspace collides with Redis's own key
// namespace conventions and, more importantly, silently un-namespaces an install that
// meant to set the flag and passed the empty string. Use DefaultKeyPrefix to opt out.
func ValidateKeyPrefix(prefix string) (string, error) {
	normalized := strings.TrimRight(strings.TrimSpace(prefix), ":")
	if normalized == "" {
		return "", fmt.Errorf("redis-key-prefix must be a non-empty identifier (default %q)", DefaultKeyPrefix)
	}
	if len(normalized) > maxKeyPrefixLength {
		return "", fmt.Errorf("redis-key-prefix must be at most %d characters, got %d",
			maxKeyPrefixLength, len(normalized))
	}
	for _, r := range normalized {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return "", fmt.Errorf(
				"redis-key-prefix %q contains %q; allowed characters are [A-Za-z0-9], '-', '_', '.' and ':'",
				prefix, string(r))
		}
	}
	return normalized, nil
}

// resolveKeyPrefix normalizes a prefix for internal use, falling back to the default when
// empty. Construction paths that did not go through ValidateKeyPrefix (tests, and the
// zero-value RedisStoreConfig) land here rather than building keys with an empty root.
func resolveKeyPrefix(prefix string) string {
	normalized := strings.TrimRight(strings.TrimSpace(prefix), ":")
	if normalized == "" {
		return DefaultKeyPrefix
	}
	return normalized
}
