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
	"errors"
	"fmt"

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
	entries, err := client.XRevRangeN(ctx, stream, "+", "-", limit).Result()
	if err != nil {
		return auditPayloadEntry{}, err
	}

	for _, entry := range entries {
		if afterID != "" && entry.ID == afterID {
			break
		}

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

	return auditPayloadEntry{}, errors.New("no matching audit payload found")
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

func prettyAuditPayload(payload map[string]interface{}) string {
	formatted, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Sprintf("<failed to format audit payload: %v>", err)
	}
	return string(formatted)
}
