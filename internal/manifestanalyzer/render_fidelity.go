// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"regexp"
	"sort"
	"strconv"
)

var renderTokenPattern = regexp.MustCompile(`\$\{[^{}]+\}`)

// RenderDivergence identifies a rendered ${...} value that is not present unchanged in the
// sanitized live object. Field is a human-readable structured path; Token is the first token in
// the rendered scalar, retained so the refusal tells an operator why the field is protected.
type RenderDivergence struct {
	Field string
	Token string
}

// RenderTokenDivergences returns every rendered ${...} scalar whose matching live field is absent
// or differs. It walks parsed object values, never YAML source bytes: comments therefore do not
// participate, and a token kustomize overwrote in the render is not considered. Only ${...} is a
// token; native Kubernetes $(...) syntax is deliberately outside this predicate.
func RenderTokenDivergences(rendered, live map[string]interface{}) []RenderDivergence {
	var out []RenderDivergence
	walkRenderTokens(rendered, live, "", &out)
	return out
}

func walkRenderTokens(rendered, live interface{}, field string, out *[]RenderDivergence) {
	switch rendered := rendered.(type) {
	case string:
		match := renderTokenPattern.FindString(rendered)
		if match == "" {
			return
		}
		liveValue, livePresent := live.(string)
		if !livePresent || liveValue != rendered {
			*out = append(*out, RenderDivergence{Field: field, Token: match})
		}
	case map[string]interface{}:
		liveMap, _ := live.(map[string]interface{})
		keys := make([]string, 0, len(rendered))
		for key := range rendered {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			liveValue := interface{}(nil)
			if liveMap != nil {
				liveValue = liveMap[key]
			}
			walkRenderTokens(rendered[key], liveValue, joinRenderFidelityField(field, key), out)
		}
	case []interface{}:
		walkRenderTokenList(rendered, live, field, out)
	}
}

func walkRenderTokenList(rendered []interface{}, live interface{}, field string, out *[]RenderDivergence) {
	liveList, _ := live.([]interface{})
	renderedByName, renderedNamed := renderFidelityNamedList(rendered)
	liveByName, liveNamed := renderFidelityNamedList(liveList)
	if renderedNamed && liveNamed {
		for _, item := range rendered {
			name := renderFidelityName(item)
			walkRenderTokens(renderedByName[name], liveByName[name], renderFidelityListField(field, name), out)
		}
		return
	}
	for index, item := range rendered {
		var liveItem interface{}
		if index < len(liveList) {
			liveItem = liveList[index]
		}
		walkRenderTokens(item, liveItem, renderFidelityListField(field, strconv.Itoa(index)), out)
	}
}

func renderFidelityNamedList(items []interface{}) (map[string]interface{}, bool) {
	if len(items) == 0 {
		return map[string]interface{}{}, true
	}
	byName := make(map[string]interface{}, len(items))
	for _, item := range items {
		name := renderFidelityName(item)
		if name == "" {
			return nil, false
		}
		if _, duplicate := byName[name]; duplicate {
			return nil, false
		}
		byName[name] = item
	}
	return byName, true
}

func renderFidelityName(value interface{}) string {
	item, ok := value.(map[string]interface{})
	if !ok {
		return ""
	}
	name, _ := item["name"].(string)
	return name
}

func joinRenderFidelityField(field, key string) string {
	if field == "" {
		return key
	}
	return field + "." + key
}

func renderFidelityListField(field, item string) string {
	return field + "[" + item + "]"
}
