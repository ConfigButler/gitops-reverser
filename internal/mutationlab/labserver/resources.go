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

package labserver

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ParseGVRs parses a comma-separated GVR spec for the watch recorder. Each item
// is "group/version/resource", or "version/resource" for core types (e.g.
// "v1/configmaps,apps/v1/deployments"). Whitespace and empty items are ignored.
func ParseGVRs(spec string) ([]schema.GroupVersionResource, error) {
	var out []schema.GroupVersionResource
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		gvr, err := parseGVR(item)
		if err != nil {
			return nil, err
		}
		out = append(out, gvr)
	}
	return out, nil
}

// coreGVRParts is "version/resource" (core group); fullGVRParts is
// "group/version/resource".
const (
	coreGVRParts = 2
	fullGVRParts = 3
)

func parseGVR(item string) (schema.GroupVersionResource, error) {
	parts := strings.Split(item, "/")
	switch len(parts) {
	case coreGVRParts:
		return schema.GroupVersionResource{Version: parts[0], Resource: parts[1]}, nil
	case fullGVRParts:
		return schema.GroupVersionResource{Group: parts[0], Version: parts[1], Resource: parts[2]}, nil
	default:
		return schema.GroupVersionResource{}, fmt.Errorf(
			"invalid resource %q: want group/version/resource or version/resource", item)
	}
}
