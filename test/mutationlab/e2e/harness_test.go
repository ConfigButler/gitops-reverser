//go:build mutationlab_e2e

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
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/corpus"
)

// scenarioLabel is stamped on every object a scenario creates, so its records are
// attributed by label rather than by namespace. That excludes the objects
// Kubernetes auto-creates in a fresh namespace (kube-root-ca.crt, the default
// ServiceAccount), which would otherwise pollute namespace-scoped capture.
const scenarioLabel = "mutationlab.configbutler.ai/scenario"

// harness holds the live-cluster handles a lab scenario needs: a Kubernetes
// client to drive writes, a dynamic client for CRDs/custom resources and
// aggregated-API objects, and the lab's records API to read what was captured.
type harness struct {
	kube   kubernetes.Interface
	dyn    dynamic.Interface
	apiURL string
}

// newHarness builds the harness from the environment and waits until the capture
// path is live, skipping the test when the lab API is not configured.
func newHarness(t *testing.T) *harness {
	t.Helper()
	apiURL := os.Getenv("LAB_API_URL")
	if apiURL == "" {
		t.Skip("LAB_API_URL not set; run via `task lab-e2e`")
	}
	cfg, err := restConfig()
	if err != nil {
		t.Fatalf("kube config: %v", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	h := &harness{kube: kube, dyn: dyn, apiURL: apiURL}
	h.ensureCaptureLive(t)
	return h
}

func restConfig() (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kc := os.Getenv("LAB_KUBECONFIG"); kc != "" {
		rules.ExplicitPath = kc
	}
	overrides := &clientcmd.ConfigOverrides{}
	if ctxName := os.Getenv("LAB_KUBECONTEXT"); ctxName != "" {
		overrides.CurrentContext = ctxName
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}

// scenario bundles a stable id — the label value used to attribute and filter
// records — with a unique namespace, which the normalizer collapses to <ns-1> so
// the corpus stays diff-free across runs.
type scenario struct {
	id string
	ns string
}

func runID() string { return strconv.FormatInt(time.Now().UnixNano()%1_000_000, 36) }

// newScenario creates a unique ephemeral namespace and clears prior records so the
// scenario starts from a clean, serial slate. Objects created with meta() carry
// the scenario label; auto-created namespace objects do not, and are excluded.
func (h *harness) newScenario(ctx context.Context, t *testing.T, id string) scenario {
	t.Helper()
	ns := fmt.Sprintf("lab-%s-%s", id, runID())
	h.createNamespace(ctx, t, ns)
	h.clearRecords(t)
	return scenario{id: id, ns: ns}
}

func (s scenario) meta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: s.ns,
		Labels:    map[string]string{scenarioLabel: s.id},
	}
}

func (h *harness) createNamespace(ctx context.Context, t *testing.T, ns string) {
	t.Helper()
	obj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if _, err := h.kube.CoreV1().Namespaces().Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace %s: %v", ns, err)
	}
	t.Cleanup(func() {
		_ = h.kube.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
	})
}

// ensureCaptureLive creates a labeled probe ConfigMap and waits until its
// admission record reaches the lab, confirming the webhook endpoint has finished
// cutting over to the lab pod after the image swap (the apiserver routes to the
// Service, whose endpoints lag a rollout). Without this, the first real scenario
// can silently miss its admission record.
func (h *harness) ensureCaptureLive(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	s := h.newScenario(ctx, t, "warmup")
	deadline := time.Now().Add(90 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		name := fmt.Sprintf("warmup-%d", attempt)
		cm := &corev1.ConfigMap{ObjectMeta: s.meta(name), Data: map[string]string{"k": "v"}}
		_, _ = h.kube.CoreV1().ConfigMaps(s.ns).Create(ctx, cm, metav1.CreateOptions{})
		time.Sleep(time.Second)
		if hasSource(h.mustFetch(t, s.id), mutationlab.SourceAdmission) {
			h.clearRecords(t)
			return
		}
	}
	t.Fatal("capture never went live: no admission record within 90s of the image swap")
}

func hasSource(records []mutationlab.Record, src mutationlab.Source) bool {
	for _, r := range records {
		if r.Source == src {
			return true
		}
	}
	return false
}

type drainSpec struct {
	minCount int
	settle   time.Duration
	timeout  time.Duration
	// until, when set, is an extra gate: the drain will not return until it holds
	// (e.g. "a watch DELETED has arrived"), so a settle window shorter than the
	// event spacing cannot return mid-sequence. Row 7's terminal DELETED arrives
	// only after the grace period, well after the deletion-pending MODIFIED.
	until func([]mutationlab.Record) bool
	// alsoNamespace, when set, unions in records attributed to that namespace key,
	// not just the scenario id. A named subresource audit event (e.g. a /scale
	// patch, whose Scale body carries no labels) attributes to the namespace
	// rather than the scenario id, so the scenario must read both keys.
	alsoNamespace string
}

// drain polls the lab records API for one scenario until at least minCount records
// have arrived, the optional until gate holds, and the count has been quiet for
// the settle window, or it fails on timeout. This rejects a stray cross-scenario
// event rather than averaging it into the corpus, and awaits a slow audit batch
// rather than missing it.
func (h *harness) drain(t *testing.T, id string, spec drainSpec) []mutationlab.Record {
	t.Helper()
	deadline := time.Now().Add(spec.timeout)
	var last []mutationlab.Record
	stableSince := time.Now()
	prevCount := -1
	for time.Now().Before(deadline) {
		records := h.fetchScenario(t, id, spec.alsoNamespace)
		if len(records) != prevCount {
			prevCount = len(records)
			stableSince = time.Now()
		}
		last = records
		gated := spec.until == nil || spec.until(records)
		if len(records) >= spec.minCount && gated && time.Since(stableSince) >= spec.settle {
			return records
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("drain timed out for scenario %s: got %d records, want >= %d", id, len(last), spec.minCount)
	return nil
}

// fetchScenario returns the records attributed to the scenario id, optionally
// unioned (deduped by record ID) with those attributed to a namespace key.
func (h *harness) fetchScenario(t *testing.T, id, namespace string) []mutationlab.Record {
	t.Helper()
	records := h.mustFetch(t, id)
	if namespace == "" {
		return records
	}
	seen := map[string]bool{}
	for _, r := range records {
		seen[r.ID] = true
	}
	for _, r := range h.mustFetch(t, namespace) {
		if !seen[r.ID] {
			records = append(records, r)
		}
	}
	return records
}

func (h *harness) mustFetch(t *testing.T, id string) []mutationlab.Record {
	t.Helper()
	records, err := h.fetchRecords(id)
	if err != nil {
		t.Fatalf("fetch records: %v", err)
	}
	return records
}

func (h *harness) fetchRecords(id string) ([]mutationlab.Record, error) {
	body, status, err := h.do(http.MethodGet, "/records?scenario="+url.QueryEscape(id))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("records API status %d: %s", status, body)
	}
	var records []mutationlab.Record
	if err := json.Unmarshal(body, &records); err != nil {
		return nil, err
	}
	return records, nil
}

func (h *harness) clearRecords(t *testing.T) {
	t.Helper()
	if _, _, err := h.do(http.MethodDelete, "/records"); err != nil {
		t.Fatalf("clear records: %v", err)
	}
}

// do issues an HTTP request to the lab API, retrying transient connection errors.
// The API sits behind a kubectl port-forward that can drop and is restarted by a
// watchdog in the lab-e2e task, so a brief connection failure is expected, not
// fatal.
func (h *harness) do(method, path string) ([]byte, int, error) {
	var lastErr error
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(method, h.apiURL+path, nil)
		if err != nil {
			return nil, 0, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			time.Sleep(500 * time.Millisecond)
			continue
		}
		return body, resp.StatusCode, nil
	}
	return nil, 0, fmt.Errorf("lab API %s %s unreachable: %w", method, path, lastErr)
}

// syncCorpus normalizes the records into the golden tree under
// test/mutationlab/corpus/<relDir>, comparing by default or rewriting when
// MUTATIONLAB_UPDATE is set.
func (h *harness) syncCorpus(t *testing.T, relDir string, records []mutationlab.Record) {
	t.Helper()
	moments, err := corpus.Build(sortRecords(records))
	if err != nil {
		t.Fatalf("build corpus: %v", err)
	}
	dir := filepath.Join("..", "corpus", relDir)
	if os.Getenv("MUTATIONLAB_UPDATE") != "" {
		if err := corpus.Write(dir, moments); err != nil {
			t.Fatalf("write corpus: %v", err)
		}
		t.Logf("updated corpus %s (%d moments)", relDir, len(moments))
		return
	}
	if err := corpus.Compare(dir, moments); err != nil {
		t.Fatalf("corpus mismatch (run `task lab-corpus-update` to accept):\n%v", err)
	}
}

// sortRecords imposes a canonical, arrival-order-independent ordering so the
// shared normalization placeholder space is deterministic: admission (the
// attempt) before audit (the request log) before watch (the result), then by
// stable identity. Without this, a late audit batch would reshuffle <uid-N>.
func sortRecords(in []mutationlab.Record) []mutationlab.Record {
	out := append([]mutationlab.Record(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := sourceRank(a.Source), sourceRank(b.Source); ra != rb {
			return ra < rb
		}
		if a.Key.Name != b.Key.Name {
			return a.Key.Name < b.Key.Name
		}
		if a.Summary.WatchType != b.Summary.WatchType {
			return a.Summary.WatchType < b.Summary.WatchType
		}
		if a.Key.ResourceVersion != b.Key.ResourceVersion {
			return rvLess(a.Key.ResourceVersion, b.Key.ResourceVersion)
		}
		// Operation orders two same-object records a shared RV cannot separate — a
		// finalizer delete's audit `delete` vs `patch`, or its admission DELETE vs
		// UPDATE, all reference the same deletion-pending resourceVersion (Row 8).
		// Without this the order would fall through to the random auditID, flipping
		// the <auditID-N> placeholder assignment run to run.
		if a.Summary.Operation != b.Summary.Operation {
			return a.Summary.Operation < b.Summary.Operation
		}
		return a.Summary.AuditID < b.Summary.AuditID
	})
	return out
}

// rvLess orders two resourceVersions numerically when both parse as integers (the
// common case — apiserver RVs are monotonic integers), falling back to a string
// compare for opaque/non-numeric values. A plain lexicographic compare would
// mis-order across digit boundaries (e.g. "10000" < "9999"), reversing exactly the
// progression the corpus normalization exists to preserve.
func rvLess(a, b string) bool {
	if ai, aerr := strconv.ParseUint(a, 10, 64); aerr == nil {
		if bi, berr := strconv.ParseUint(b, 10, 64); berr == nil {
			return ai < bi
		}
	}
	return a < b
}

func sourceRank(s mutationlab.Source) int {
	switch s {
	case mutationlab.SourceAdmission:
		return 0
	case mutationlab.SourceConversion:
		// Conversion happens during the write, between admission and persistence.
		return 1
	case mutationlab.SourceAudit:
		return 2
	case mutationlab.SourceAuditAdditional:
		return 3
	case mutationlab.SourceWatch:
		return 4
	default:
		return 5
	}
}
