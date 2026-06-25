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

package recorder

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/store"
)

const maxConversionBody = 8 << 20

// Conversion is the lab's CRD conversion webhook recorder. The apiserver calls it
// to convert the lab's two-version Widget custom resource between v1 and v2 (Row
// 14). It records each ConversionReview — so the corpus shows the apiserver's
// conversion contract — and performs the field rename the two schemas differ by
// (v1 spec.sizeBytes:int <-> v2 spec.size:string), which is what makes the
// submitted / stored / served shapes genuinely diverge.
type Conversion struct {
	store *store.Store
}

// NewConversion returns a Conversion recorder backed by the given store.
func NewConversion(s *store.Store) *Conversion { return &Conversion{store: s} }

// conversionReview is a lenient projection of apiextensions.k8s.io/v1
// ConversionReview, reading only the request fields the lab needs and keeping the
// objects as raw bytes so the corpus stores what the apiserver actually sent.
type conversionReview struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Request    *struct {
		UID               string            `json:"uid"`
		DesiredAPIVersion string            `json:"desiredAPIVersion"`
		Objects           []json.RawMessage `json:"objects"`
	} `json:"request"`
}

// ServeHTTP decodes one ConversionReview, records it, converts each object to the
// desired version, and returns the converted objects.
func (c *Conversion) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConversionBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var review conversionReview
	if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
		http.Error(w, "decode ConversionReview", http.StatusBadRequest)
		return
	}

	target := versionOf(review.Request.DesiredAPIVersion)
	converted := make([]json.RawMessage, 0, len(review.Request.Objects))
	for _, raw := range review.Request.Objects {
		converted = append(converted, convertWidget(raw, review.Request.DesiredAPIVersion))
	}

	c.store.Add(mutationlab.Record{
		Source:     mutationlab.SourceConversion,
		Scenario:   scenarioFromLabels(objectLabels(rawBytes(review.Request.Objects)...), ""),
		ObservedAt: time.Now(),
		Key:        conversionKey(rawBytes(review.Request.Objects), review.Request.DesiredAPIVersion),
		Summary: mutationlab.RecordSummary{
			Operation: "to-" + target,
			HasObject: len(review.Request.Objects) > 0,
		},
		Raw: body,
	})

	writeConversionResponse(w, review.APIVersion, review.Request.UID, converted)
}

// convertWidget rewrites one Widget object to desiredAPIVersion, translating the
// field the two versions differ by: v1 spec.sizeBytes (integer) <-> v2 spec.size
// (string). Unknown shapes pass through with only apiVersion rewritten, so the
// webhook never drops an object.
func convertWidget(raw json.RawMessage, desiredAPIVersion string) json.RawMessage {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	obj["apiVersion"] = desiredAPIVersion
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		return remarshal(obj, raw)
	}
	switch versionOf(desiredAPIVersion) {
	case "v2":
		if n, ok := widgetSize(spec["sizeBytes"]); ok {
			spec["size"] = strconv.FormatInt(n, 10)
			delete(spec, "sizeBytes")
		}
	case "v1":
		if n, ok := widgetSize(spec["size"]); ok {
			spec["sizeBytes"] = n
			delete(spec, "size")
		}
	}
	return remarshal(obj, raw)
}

// widgetSize reads the Widget size from either the v1 integer or the v2 string
// form, returning the canonical integer.
func widgetSize(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	case float64:
		return int64(t), true
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func remarshal(obj map[string]any, fallback json.RawMessage) json.RawMessage {
	out, err := json.Marshal(obj)
	if err != nil {
		return fallback
	}
	return out
}

// versionOf returns the version segment of a group/version string
// ("mutationlab.configbutler.ai/v2" -> "v2").
func versionOf(apiVersion string) string {
	if i := strings.LastIndex(apiVersion, "/"); i >= 0 {
		return apiVersion[i+1:]
	}
	return apiVersion
}

func conversionKey(objects [][]byte, desiredAPIVersion string) mutationlab.ObjectKey {
	key := mutationlab.ObjectKey{Version: versionOf(desiredAPIVersion), Name: objectName(objects...)}
	if i := strings.LastIndex(desiredAPIVersion, "/"); i > 0 {
		key.Group = desiredAPIVersion[:i]
	}
	return key
}

// rawBytes adapts a slice of json.RawMessage to the [][]byte the shared
// objectLabels/objectName helpers take.
func rawBytes(msgs []json.RawMessage) [][]byte {
	out := make([][]byte, len(msgs))
	for i, m := range msgs {
		out[i] = m
	}
	return out
}

func writeConversionResponse(w http.ResponseWriter, apiVersion, uid string, converted []json.RawMessage) {
	if apiVersion == "" {
		apiVersion = "apiextensions.k8s.io/v1"
	}
	resp := map[string]any{
		"apiVersion": apiVersion,
		"kind":       "ConversionReview",
		"response": map[string]any{
			"uid":              uid,
			"convertedObjects": converted,
			"result":           map[string]any{"status": "Success"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
