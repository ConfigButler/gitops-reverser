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

// ValidateKeyPrefix checks a --redis-key-prefix value and returns its normalized form.
//
// Two independent constraints shape the allowed character set:
//
//   - The attribution telemetry gauge SCANs "<prefix>:author:v1:audit:*". Redis glob
//     metacharacters (*, ?, [, ], \) in the prefix would silently make that pattern match
//     the wrong keyspace, so they are rejected rather than escaped — a prefix is an
//     operator-chosen identifier, not user data.
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
