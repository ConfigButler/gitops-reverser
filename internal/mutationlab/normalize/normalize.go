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

// Package normalize rewrites the volatile fields of captured Kubernetes payloads
// to stable, relational placeholders so the lab corpus changes only when
// behavior changes — never when a run merely produces fresh UIDs, resource
// versions, or timestamps.
//
// The placeholders are deliberately relational, not flattened: collapsing every
// UID to one <uid> would erase exactly the evidence the hard scenarios exist to
// capture (which objects in a deletecollection fan-out are distinct, that a
// finalizer's terminal DELETED is the *same* object at a *higher* resourceVersion).
// Instead each volatile field becomes an indexed placeholder, assigned
// deterministically so that equal inputs map to the same placeholder, distinct
// inputs to distinct placeholders, and the index order reflects real order.
//
// This package is the ONLY thing allowed to mutate a payload on its way to disk.
package normalize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// tsPlaceholder is the single, non-relational token every timestamp collapses to.
//
// Timestamps are deliberately NOT relational (<ts-1>, <ts-2>, ...). An earlier
// chronological scheme proved unstable for objects with many near-simultaneous
// timestamps: Kubernetes emits them at one-second granularity, so whether two
// events (a Pod's creationTimestamp and its first status condition, say) fall in
// the same second or adjacent seconds varies run to run, which changes how many
// distinct values exist and shuffles every index. Object-version sequencing is
// carried by resourceVersion (relational and numeric) and by the moment file
// ordering instead, so collapsing every timestamp to one token keeps the corpus
// stable without losing the ordering that matters.
const tsPlaceholder = "<ts>"

// isTimestampKey reports whether a JSON key's string value is an RFC3339
// timestamp the normalizer collapses to tsPlaceholder. "time" covers
// managedFields[].time; lastTransitionTime/lastUpdateTime cover status.conditions[]
// (Deployments, Pods, and most status-bearing types); startTime/startedAt/
// finishedAt cover Pod and container lifecycle (Row 7's graceful delete).
func isTimestampKey(key string) bool {
	switch key {
	case "creationTimestamp", "deletionTimestamp", "stageTimestamp", "requestReceivedTimestamp", "time",
		"lastTransitionTime", "lastUpdateTime", "startTime", "startedAt", "finishedAt":
		return true
	default:
		return false
	}
}

// Normalize rewrites the ordered payloads of one scenario into a single shared
// placeholder space, so an identity that recurs across records (e.g. the same
// UID in a watch event and an audit event) collapses to the same token. Input
// payloads are JSON; output values are generic decoded structures ready for
// deterministic YAML marshaling.
//
// Indexing rules, per the design:
//   - uid / auditID / ip / containerID / nodeName / generateName-suffix: by first
//     appearance;
//   - resourceVersion: numeric order when every observed RV in the scenario is an
//     integer (the stream is then provably orderable), otherwise first appearance;
//   - timestamps: collapsed to a single non-relational token (see tsPlaceholder).
func Normalize(payloads []json.RawMessage) ([]any, error) {
	decoded := make([]any, len(payloads))
	for i, raw := range payloads {
		v, err := decodeJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("payload %d: %w", i, err)
		}
		decoded[i] = v
	}

	c := newCollector()
	for _, v := range decoded {
		c.walk(v)
	}
	idx := c.buildIndices()

	out := make([]any, len(decoded))
	for i, v := range decoded {
		out[i] = idx.transform(v)
	}
	return out, nil
}

// Single normalizes one payload in its own placeholder space. It is a
// convenience for callers (e.g. the /records API) that handle a single
// observation rather than a whole scenario.
func Single(payload json.RawMessage) (any, error) {
	out, err := Normalize([]json.RawMessage{payload})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// decodeJSON decodes into generic values, keeping numbers as json.Number so that
// integers (resourceVersion, ports, codes) round-trip without float reformatting.
func decodeJSON(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// ordered is a first-appearance-ordered set of distinct string values.
type ordered struct {
	seen  map[string]int
	items []string
}

func newOrdered() *ordered { return &ordered{seen: map[string]int{}} }

func (o *ordered) add(v string) {
	if _, ok := o.seen[v]; !ok {
		o.seen[v] = len(o.items)
		o.items = append(o.items, v)
	}
}

type collector struct {
	uid     *ordered
	rv      *ordered
	auditID *ordered
	ip      *ordered
	cid     *ordered
	node    *ordered
	cred    *ordered
	rnd     *ordered
	ns      *ordered
}

func newCollector() *collector {
	return &collector{
		uid:     newOrdered(),
		rv:      newOrdered(),
		auditID: newOrdered(),
		ip:      newOrdered(),
		cid:     newOrdered(),
		node:    newOrdered(),
		cred:    newOrdered(),
		rnd:     newOrdered(),
		ns:      newOrdered(),
	}
}

// credentialIDKey is the audit user.extra key whose value (e.g.
// "X509SHA256=<fingerprint>") identifies the client certificate. The fingerprint
// is regenerated whenever the cluster's CA is recreated, so it must be normalized
// or the corpus would drift on every cluster rebuild and every Kubernetes-version
// bump (defeating the version-validation goal). Its value is a list of strings.
const credentialIDKey = "authentication.kubernetes.io/credential-id"

func (c *collector) walk(v any) {
	switch t := v.(type) {
	case map[string]any:
		c.collectGenerateNameSuffix(t)
		// Walk keys in sorted order so first-appearance indexing is deterministic
		// regardless of Go's randomized map iteration.
		for _, k := range sortedKeys(t) {
			c.collectScalar(k, t[k])
			c.walk(t[k])
		}
	case []any:
		for _, e := range t {
			c.walk(e)
		}
	}
}

// collectGenerateNameSuffix registers the random suffix the apiserver appends to
// a generateName prefix, so name "cm-x7k2p" with generateName "cm-" yields a
// <rand-N> for "x7k2p" while the stable prefix is preserved.
func (c *collector) collectGenerateNameSuffix(m map[string]any) {
	gn, ok := stringVal(m["generateName"])
	if !ok || gn == "" {
		return
	}
	name, ok := stringVal(m["name"])
	if !ok || !strings.HasPrefix(name, gn) || len(name) <= len(gn) {
		return
	}
	c.rnd.add(name[len(gn):])
}

func (c *collector) collectScalar(key string, v any) {
	if key == "sourceIPs" || key == credentialIDKey {
		dest := c.ip
		if key == credentialIDKey {
			dest = c.cred
		}
		if arr, ok := v.([]any); ok {
			for _, e := range arr {
				if s, ok := stringVal(e); ok {
					dest.add(s)
				}
			}
		}
		return
	}
	s, ok := stringVal(v)
	if !ok || s == "" {
		return
	}
	// Timestamps collapse to a constant token, so they are not collected.
	if o := c.orderedFor(key); o != nil {
		o.add(s)
	}
}

// orderedFor returns the relational category a scalar key belongs to, or nil if
// the key is not normalized (or is collapsed, like timestamps).
func (c *collector) orderedFor(key string) *ordered {
	switch {
	case key == "uid":
		return c.uid
	case key == "resourceVersion":
		return c.rv
	case key == "auditID":
		return c.auditID
	case key == "namespace":
		return c.ns
	case isIPKey(key):
		return c.ip
	case key == "containerID":
		return c.cid
	case key == "nodeName":
		return c.node
	default:
		return nil
	}
}

// isIPKey reports whether a JSON key carries a single IP-valued string the
// normalizer rewrites to <ip-N>: pod/host IPs and the "ip" entries inside the
// podIPs/hostIPs arrays. (sourceIPs is an array and is handled separately.)
func isIPKey(key string) bool {
	switch key {
	case "podIP", "hostIP", "ip":
		return true
	default:
		return false
	}
}

// indices holds the final raw-value -> placeholder maps for one scenario.
type indices struct {
	uid     map[string]string
	rv      map[string]string
	auditID map[string]string
	ip      map[string]string
	cid     map[string]string
	node    map[string]string
	cred    map[string]string
	rnd     map[string]string
	ns      map[string]string
	// nsByLen / ipByLen / uidByLen are the namespace / IP / UID values sorted
	// longest-first, so substring replacement (in requestURIs and in managedFields
	// association keys like k:{"ip":"10.42.3.14"} or k:{"uid":"<owner-uid>"}) never
	// leaves a prefix of a longer value. An ownerReference is keyed by the owner's
	// UID inside managedFields, so without uidByLen a cascaded child's owner UID
	// churns the corpus every run (Row 10).
	nsByLen  []string
	ipByLen  []string
	uidByLen []string
}

func (c *collector) buildIndices() *indices {
	idx := &indices{
		uid:     byFirstAppearance(c.uid, "uid"),
		auditID: byFirstAppearance(c.auditID, "auditID"),
		ip:      byFirstAppearance(c.ip, "ip"),
		cid:     byFirstAppearance(c.cid, "containerID"),
		node:    byFirstAppearance(c.node, "node"),
		cred:    byFirstAppearance(c.cred, "credential"),
		rnd:     byFirstAppearance(c.rnd, "rand"),
		rv:      assignResourceVersions(c.rv),
		ns:      byFirstAppearance(c.ns, "ns"),
	}
	idx.nsByLen = append(idx.nsByLen, c.ns.items...)
	sort.SliceStable(idx.nsByLen, func(i, j int) bool {
		return len(idx.nsByLen[i]) > len(idx.nsByLen[j])
	})
	idx.ipByLen = append(idx.ipByLen, c.ip.items...)
	sort.SliceStable(idx.ipByLen, func(i, j int) bool {
		return len(idx.ipByLen[i]) > len(idx.ipByLen[j])
	})
	idx.uidByLen = append(idx.uidByLen, c.uid.items...)
	sort.SliceStable(idx.uidByLen, func(i, j int) bool {
		return len(idx.uidByLen[i]) > len(idx.uidByLen[j])
	})
	return idx
}

func byFirstAppearance(o *ordered, prefix string) map[string]string {
	m := make(map[string]string, len(o.items))
	for i, v := range o.items {
		m[v] = placeholder(prefix, i+1)
	}
	return m
}

// assignResourceVersions assigns numeric order only when every observed RV is an
// integer (the stream is then provably orderable); otherwise first-appearance.
func assignResourceVersions(o *ordered) map[string]string {
	order := append([]string(nil), o.items...)
	if nums, ok := allIntegers(order); ok {
		sort.SliceStable(order, func(i, j int) bool {
			return nums[order[i]].Cmp(nums[order[j]]) < 0
		})
	}
	m := make(map[string]string, len(order))
	for i, v := range order {
		m[v] = placeholder("rv", i+1)
	}
	return m
}

func (idx *indices) transform(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			// Values key off the original k (for type detection); the output key
			// is rewritten so a volatile value embedded in a managedFields
			// association key (k:{"ip":"10.42.3.14"}) does not churn the corpus.
			out[idx.rewriteKey(k)] = idx.transformScalar(k, val, t)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = idx.transform(e)
		}
		return out
	default:
		return v
	}
}

// transformScalar rewrites a single key/value, falling back to a recursive
// transform when the value is not a normalized leaf.
func (idx *indices) transformScalar(key string, v any, parent map[string]any) any {
	if key == "name" {
		if rewritten, ok := idx.rewriteGeneratedName(v, parent); ok {
			return rewritten
		}
	}
	if key == "sourceIPs" || key == credentialIDKey {
		m := idx.ip
		if key == credentialIDKey {
			m = idx.cred
		}
		if arr, ok := v.([]any); ok {
			out := make([]any, len(arr))
			for i, e := range arr {
				out[i] = mapped(m, e)
			}
			return out
		}
	}
	if s, ok := stringVal(v); ok {
		if isTimestampKey(key) {
			return tsPlaceholder
		}
		if m := idx.mapForKey(key); m != nil {
			return lookup(m, s)
		}
		if key == "requestURI" || key == "selfLink" {
			// The namespace appears embedded in the path; replace it as a
			// substring so a unique per-run namespace does not churn the corpus.
			return idx.replaceNamespaces(s)
		}
	}
	return idx.transform(v)
}

// mapForKey returns the placeholder map a scalar key is rewritten through, or nil
// if the key is not a relational normalized leaf. It mirrors collector.orderedFor.
func (idx *indices) mapForKey(key string) map[string]string {
	switch {
	case key == "uid":
		return idx.uid
	case key == "resourceVersion":
		return idx.rv
	case key == "auditID":
		return idx.auditID
	case key == "namespace":
		return idx.ns
	case isIPKey(key):
		return idx.ip
	case key == "containerID":
		return idx.cid
	case key == "nodeName":
		return idx.node
	default:
		return nil
	}
}

// replaceNamespaces rewrites each collected namespace value, wherever it appears
// as a substring, to its placeholder — longest first so a shorter namespace that
// prefixes a longer one cannot partially match.
func (idx *indices) replaceNamespaces(s string) string {
	for _, ns := range idx.nsByLen {
		s = strings.ReplaceAll(s, ns, idx.ns[ns])
	}
	return s
}

// rewriteKey rewrites a map key, replacing any embedded namespace, IP, or UID
// value with its placeholder. Most keys contain none and pass through unchanged;
// the case that matters is a managedFields fieldsV1 association key such as
// k:{"ip":"10.42.3.14"} or k:{"uid":"<owner-uid>"} (an ownerReference is keyed by
// the owner's UID), where a volatile value is part of the key itself.
func (idx *indices) rewriteKey(k string) string {
	if !strings.HasPrefix(k, "k:{") {
		return k
	}
	k = idx.replaceNamespaces(k)
	for _, ip := range idx.ipByLen {
		k = strings.ReplaceAll(k, ip, idx.ip[ip])
	}
	for _, uid := range idx.uidByLen {
		k = strings.ReplaceAll(k, uid, idx.uid[uid])
	}
	return k
}

func (idx *indices) rewriteGeneratedName(v any, parent map[string]any) (string, bool) {
	gn, ok := stringVal(parent["generateName"])
	if !ok || gn == "" {
		return "", false
	}
	name, ok := stringVal(v)
	if !ok || !strings.HasPrefix(name, gn) || len(name) <= len(gn) {
		return "", false
	}
	suffix := name[len(gn):]
	if ph, ok := idx.rnd[suffix]; ok {
		return gn + ph, true
	}
	return "", false
}

func placeholder(prefix string, n int) string { return fmt.Sprintf("<%s-%d>", prefix, n) }

func lookup(m map[string]string, s string) any {
	if ph, ok := m[s]; ok {
		return ph
	}
	return s
}

func mapped(m map[string]string, v any) any {
	if s, ok := stringVal(v); ok {
		return lookup(m, s)
	}
	return v
}

// decimalBase is the radix for parsing resourceVersion integers.
const decimalBase = 10

func allIntegers(vals []string) (map[string]*big.Int, bool) {
	out := make(map[string]*big.Int, len(vals))
	for _, v := range vals {
		n, ok := new(big.Int).SetString(v, decimalBase)
		if !ok {
			return nil, false
		}
		out[v] = n
	}
	return out, true
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func stringVal(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}
