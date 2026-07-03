// SPDX-License-Identifier: Apache-2.0

// Package recorder turns live Kubernetes observations — audit webhook posts,
// admission reviews, and native watch events — into mutationlab.Record values in
// the store. Each recorder attributes a record to a scenario so the corpus stays
// isolated even when audit batches arrive late and cross scenario boundaries.
package recorder

import (
	"net/url"
	"strings"
)

// ScenarioLabel is stamped on every lab object so watch/audit/admission records
// can be attributed to a scenario from the label even for name-less requests.
const ScenarioLabel = "mutationlab.configbutler.ai/scenario"

// scenarioFromLabels returns the explicit scenario label, falling back to the
// namespace, which the harness names lab-<scenario>-<runid>. An explicit label is
// preferred because it survives name-less and cross-namespace requests.
func scenarioFromLabels(labels map[string]string, namespace string) string {
	if labels != nil {
		if s := labels[ScenarioLabel]; s != "" {
			return s
		}
	}
	return namespace
}

// scenarioFromRequestURI recovers a scenario from an audit requestURI when the
// event has no object to read a label from (the name-less deletecollection case):
// it prefers a label selector on ScenarioLabel and falls back to the
// /namespaces/<ns>/ path segment.
func scenarioFromRequestURI(requestURI string) string {
	u, err := url.Parse(requestURI)
	if err != nil {
		return ""
	}
	if sel := u.Query().Get("labelSelector"); sel != "" {
		if s := scenarioFromSelector(sel); s != "" {
			return s
		}
	}
	return namespaceFromPath(u.Path)
}

func scenarioFromSelector(selector string) string {
	for _, term := range strings.Split(selector, ",") {
		if k, v, ok := strings.Cut(term, "="); ok && strings.TrimSpace(k) == ScenarioLabel {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func namespaceFromPath(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == "namespaces" {
			return segs[i+1]
		}
	}
	return ""
}
