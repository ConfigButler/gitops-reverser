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

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

type auditPayloadEntry struct {
	ID      string
	Payload map[string]interface{}
	RawJSON string
}

func newE2EValkeyClient() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     valkeyPortForwardAddr(),
		Password: valkeyPortForwardPassword(),
	})
}

func latestAuditStreamID(ctx context.Context, client *redis.Client, stream string) (string, error) {
	entries, err := client.XRevRangeN(ctx, stream, "+", "-", 1).Result()
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", nil
	}

	return entries[0].ID, nil
}

func findAuditPayloadSince(
	ctx context.Context,
	client *redis.Client,
	stream, afterID string,
	limit int64,
	match func(map[string]interface{}) bool,
) (auditPayloadEntry, error) {
	start := "-"
	if afterID != "" {
		start = "(" + afterID
	}

	entries, err := client.XRangeN(ctx, stream, start, "+", limit).Result()
	if err != nil {
		return auditPayloadEntry{}, err
	}

	for _, entry := range entries {
		rawJSON, ok := entry.Values["payload_json"].(string)
		if !ok || rawJSON == "" {
			continue
		}

		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(rawJSON), &payload); err != nil {
			return auditPayloadEntry{}, fmt.Errorf("decode payload_json for stream entry %s: %w", entry.ID, err)
		}
		if match(payload) {
			return auditPayloadEntry{
				ID:      entry.ID,
				Payload: payload,
				RawJSON: rawJSON,
			}, nil
		}
	}

	return auditPayloadEntry{}, fmt.Errorf(
		"no matching audit payload found after scanning %d entries from %q: %s",
		len(entries),
		start,
		summarizeAuditPayloadEntries(entries, 12),
	)
}

func countAuditPayloadsByAuditIDSince(
	ctx context.Context,
	client *redis.Client,
	stream, afterID, auditID string,
	limit int64,
) (int, error) {
	start := "-"
	if afterID != "" {
		start = "(" + afterID
	}

	entries, err := client.XRangeN(ctx, stream, start, "+", limit).Result()
	if err != nil {
		return 0, err
	}

	count := 0
	for _, entry := range entries {
		value, _ := entry.Values["audit_id"].(string)
		if value == auditID {
			count++
		}
	}
	return count, nil
}

func auditPayloadID(payload map[string]interface{}) string {
	value, _ := payload["auditID"].(string)
	return value
}

func auditPayloadMatches(
	payload map[string]interface{},
	apiGroup, resource, namespace, name, verb string,
) bool {
	objectRef, _ := payload["objectRef"].(map[string]interface{})
	if objectRef == nil {
		return false
	}
	if value, _ := payload["verb"].(string); verb != "" && value != verb {
		return false
	}
	if value, _ := objectRef["apiGroup"].(string); apiGroup != value {
		return false
	}
	if value, _ := objectRef["resource"].(string); resource != value {
		return false
	}
	if value, _ := objectRef["namespace"].(string); namespace != value {
		return false
	}
	if name == "" {
		return true
	}

	value, _ := objectRef["name"].(string)
	return name == value
}

func auditObjectRefName(payload map[string]interface{}) string {
	objectRef, _ := payload["objectRef"].(map[string]interface{})
	if objectRef == nil {
		return ""
	}

	value, _ := objectRef["name"].(string)
	return value
}

func auditPayloadHasObject(payload map[string]interface{}, field string) bool {
	object, _ := payload[field].(map[string]interface{})
	return len(object) > 0
}

func summarizeAuditPayloadEntries(entries []redis.XMessage, maxEntries int) string {
	if len(entries) == 0 {
		return "<empty stream range>"
	}
	if maxEntries <= 0 {
		maxEntries = len(entries)
	}
	start := 0
	if len(entries) > maxEntries {
		start = len(entries) - maxEntries
	}

	var builder strings.Builder
	if start > 0 {
		_, _ = fmt.Fprintf(&builder, "... %d earlier entries omitted; ", start)
	}
	for i, entry := range entries[start:] {
		if i > 0 {
			builder.WriteString("; ")
		}
		rawJSON, _ := entry.Values["payload_json"].(string)
		if rawJSON == "" {
			_, _ = fmt.Fprintf(&builder, "%s <missing payload_json>", entry.ID)
			continue
		}

		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(rawJSON), &payload); err != nil {
			_, _ = fmt.Fprintf(&builder, "%s <invalid payload_json: %v>", entry.ID, err)
			continue
		}
		_, _ = fmt.Fprintf(
			&builder,
			"%s auditID=%s verb=%s ref=%s/%s ns=%s name=%s requestObject=%t responseObject=%t",
			entry.ID,
			auditPayloadID(payload),
			auditPayloadString(payload, "verb"),
			auditPayloadObjectRefString(payload, "apiGroup"),
			auditPayloadObjectRefString(payload, "resource"),
			auditPayloadObjectRefString(payload, "namespace"),
			auditPayloadObjectRefString(payload, "name"),
			auditPayloadHasObject(payload, "requestObject"),
			auditPayloadHasObject(payload, "responseObject"),
		)
	}
	return builder.String()
}

func auditPayloadString(payload map[string]interface{}, field string) string {
	value, _ := payload[field].(string)
	return value
}

func auditPayloadObjectRefString(payload map[string]interface{}, field string) string {
	objectRef, _ := payload["objectRef"].(map[string]interface{})
	if objectRef == nil {
		return ""
	}
	value, _ := objectRef[field].(string)
	return value
}

func prettyAuditPayload(payload map[string]interface{}) string {
	formatted, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Sprintf("<failed to format audit payload: %v>", err)
	}
	return string(formatted)
}
