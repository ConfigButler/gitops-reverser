// SPDX-License-Identifier: Apache-2.0

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
