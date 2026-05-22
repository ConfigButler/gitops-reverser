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

package types

import (
	"fmt"
	"sort"
	"strings"
)

const sensitiveResourceParts = 2

type sensitiveResourceKey struct {
	group    string
	resource string
}

// SensitiveResourcePolicy classifies resource types that must use the encrypted
// Git write path. Core Kubernetes Secrets are always sensitive.
type SensitiveResourcePolicy struct {
	additional map[sensitiveResourceKey]struct{}
}

// ParseSensitiveResourcePolicy builds a policy from comma-separated additional
// entries in resource or group/resource form.
func ParseSensitiveResourcePolicy(additional string) (SensitiveResourcePolicy, error) {
	policy := SensitiveResourcePolicy{}
	if strings.TrimSpace(additional) == "" {
		return policy, nil
	}

	policy.additional = make(map[sensitiveResourceKey]struct{})
	for _, entry := range strings.Split(additional, ",") {
		key, err := parseSensitiveResourceEntry(entry)
		if err != nil {
			return SensitiveResourcePolicy{}, err
		}
		policy.additional[key] = struct{}{}
	}
	return policy, nil
}

// IsSensitive reports whether group/resource must use the encrypted Git write path.
func (p SensitiveResourcePolicy) IsSensitive(group, resource string) bool {
	key := normalizeSensitiveResourceKey(group, resource)
	if key.group == "" && key.resource == "secrets" {
		return true
	}
	_, ok := p.additional[key]
	return ok
}

// Entries returns the built-in and additional sensitive resource types in flag form.
func (p SensitiveResourcePolicy) Entries() []string {
	entries := []string{"secrets"}
	for key := range p.additional {
		entries = append(entries, formatSensitiveResourceEntry(key))
	}
	sort.Strings(entries)
	return entries
}

func parseSensitiveResourceEntry(entry string) (sensitiveResourceKey, error) {
	trimmed := strings.TrimSpace(entry)
	if trimmed == "" {
		return sensitiveResourceKey{}, errorsSensitiveResourceEntry(entry, "entry is empty")
	}

	parts := strings.Split(trimmed, "/")
	switch len(parts) {
	case 1:
		key := normalizeSensitiveResourceKey("", parts[0])
		if key.resource == "" {
			return sensitiveResourceKey{}, errorsSensitiveResourceEntry(entry, "resource is empty")
		}
		return key, nil
	case sensitiveResourceParts:
		key := normalizeSensitiveResourceKey(parts[0], parts[1])
		if key.group == "" {
			return sensitiveResourceKey{}, errorsSensitiveResourceEntry(entry, "group is empty")
		}
		if key.resource == "" {
			return sensitiveResourceKey{}, errorsSensitiveResourceEntry(entry, "resource is empty")
		}
		return key, nil
	default:
		return sensitiveResourceKey{}, errorsSensitiveResourceEntry(
			entry,
			"expected resource or group/resource",
		)
	}
}

func normalizeSensitiveResourceKey(group, resource string) sensitiveResourceKey {
	return sensitiveResourceKey{
		group:    strings.ToLower(strings.TrimSpace(group)),
		resource: strings.ToLower(strings.TrimSpace(resource)),
	}
}

func formatSensitiveResourceEntry(key sensitiveResourceKey) string {
	if key.group == "" {
		return key.resource
	}
	return key.group + "/" + key.resource
}

func errorsSensitiveResourceEntry(entry, detail string) error {
	return fmt.Errorf("invalid additional sensitive resource %q: %s", entry, detail)
}
