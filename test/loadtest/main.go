// SPDX-License-Identifier: Apache-2.0
//
// Copyright 2025 ConfigButler
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"os"
	"sort"
	"sync"
	"time"
)

// ── Kubernetes API types ──────────────────────────────────────────────────────

const (
	apiGroup   = "examples.configbutler.ai"
	apiVersion = "v1alpha1"
)

type answer struct {
	QuestionID   string   `json:"questionId"`
	SingleChoice string   `json:"singleChoice,omitempty"`
	MultiChoice  []string `json:"multiChoice,omitempty"`
	Number       *float64 `json:"number,omitempty"`
	FreeText     string   `json:"freeText,omitempty"`
}

type sessionRef struct {
	Name string `json:"name"`
}

type submissionSpec struct {
	SessionRef  sessionRef `json:"sessionRef"`
	SubmittedAt string     `json:"submittedAt"`
	Answers     []answer   `json:"answers"`
}

type objectMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type quizSubmission struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   objectMeta     `json:"metadata"`
	Spec       submissionSpec `json:"spec"`
}

// ── Realistic answer pools ────────────────────────────────────────────────────

var freeTextPool = []string{
	"More live demos!",
	"Loved the kubectl integration.",
	"Could use more time on RBAC.",
	"Great talk, very hands-on.",
	"Would love a follow-up on APF.",
	"Show the full Flux workflow end-to-end.",
	"Brilliant use of CRDs.",
	"More Prometheus integration examples please.",
	"Very practical, loved it.",
	"The QR code login was a great idea.",
	"More depth on GitOps tooling.",
	"Excellent speaker, clear explanations.",
	"Would be great to see this in production.",
	"Loved how simple the auth flow ended up being.",
}

func floatPtr(f float64) *float64 { return &f }

// clamp rounds and clamps v to [lo, hi].
func clamp(v, lo, hi float64) float64 {
	v = math.Round(v)
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func randomSubmission(rng *rand.Rand, userID int, runID, session, namespace string) quizSubmission {
	// q1 singleChoice — weighted 70% Yes / 20% Somewhat / 10% No
	q1Pool := []string{
		"Yes", "Yes", "Yes", "Yes", "Yes", "Yes", "Yes",
		"Somewhat", "Somewhat",
		"No",
	}
	q1 := q1Pool[rng.Intn(len(q1Pool))]

	// q2 multiChoice — random non-empty subset of tools
	tools := []string{"Kubernetes", "Traefik", "Prometheus", "Terraform"}
	rng.Shuffle(len(tools), func(i, j int) { tools[i], tools[j] = tools[j], tools[i] })
	q2 := tools[:1+rng.Intn(len(tools))]

	// q3 scale0to10 — Gaussian centered on 7, clamped [0,10]
	q3 := clamp(rng.NormFloat64()*1.5+7, 0, 10)

	// q4 number — realistic cluster count [1, 50]
	q4 := float64(1 + rng.Intn(50))

	// q5 freeText — include userID to ensure uniqueness across submissions
	q5 := fmt.Sprintf("%s [loadtest-user-%d]", freeTextPool[rng.Intn(len(freeTextPool))], userID)

	return quizSubmission{
		APIVersion: apiGroup + "/" + apiVersion,
		Kind:       "QuizSubmission",
		Metadata: objectMeta{
			Name:      fmt.Sprintf("%s-%d", runID, userID),
			Namespace: namespace,
		},
		Spec: submissionSpec{
			SessionRef:  sessionRef{Name: session},
			SubmittedAt: time.Now().UTC().Format(time.RFC3339),
			Answers: []answer{
				{QuestionID: "q1", SingleChoice: q1},
				{QuestionID: "q2", MultiChoice: q2},
				{QuestionID: "q3", Number: floatPtr(q3)},
				{QuestionID: "q4", Number: floatPtr(q4)},
				{QuestionID: "q5", FreeText: q5},
			},
		},
	}
}

// ── Per-user simulation ───────────────────────────────────────────────────────

type userResult struct {
	userID        int
	loginStatus   int
	submitStatus  int
	loginLatency  time.Duration
	submitLatency time.Duration
	err           string
	ok            bool
}

func simulateUser(
	ctx context.Context,
	userID int,
	baseURL, code, runID, session, namespace string,
	rng *rand.Rand,
) userResult {
	res := userResult{userID: userID}

	// Each user gets their own cookie jar so sessions are completely isolated.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:     jar,
		Timeout: 15 * time.Second,
	}

	// ── Step 1: Login — establish session cookie via join code ────────────────
	// Mirrors what the real browser does: GET /auth/session-info with the join
	// code. The ForwardAuth middleware validates the code and sets the
	// auth_session cookie; the handler also verifies the QuizSession is live.
	loginReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/auth/session-info", nil)
	if err != nil {
		res.err = fmt.Sprintf("build login request: %v", err)
		return res
	}
	loginReq.Header.Set("X-Join-Code", code)

	t0 := time.Now()
	loginResp, err := client.Do(loginReq)
	res.loginLatency = time.Since(t0)
	if err != nil {
		res.err = fmt.Sprintf("login: %v", err)
		return res
	}
	io.Copy(io.Discard, loginResp.Body) //nolint:errcheck
	loginResp.Body.Close()
	res.loginStatus = loginResp.StatusCode

	if loginResp.StatusCode != http.StatusOK {
		res.err = fmt.Sprintf("login HTTP %d", loginResp.StatusCode)
		return res
	}

	// ── Think time — realistic pause while reading the questions ─────────────
	thinkMS := 1000 + rng.Intn(3000)
	select {
	case <-ctx.Done():
		res.err = "context cancelled during think time"
		return res
	case <-time.After(time.Duration(thinkMS) * time.Millisecond):
	}

	// ── Step 2: Submit quiz answers ───────────────────────────────────────────
	sub := randomSubmission(rng, userID, runID, session, namespace)
	body, err := json.Marshal(sub)
	if err != nil {
		res.err = fmt.Sprintf("marshal submission: %v", err)
		return res
	}

	submitURL := fmt.Sprintf("%s/apis/%s/%s/namespaces/%s/quizsubmissions",
		baseURL, apiGroup, apiVersion, namespace)
	submitReq, err := http.NewRequestWithContext(ctx, http.MethodPost, submitURL, bytes.NewReader(body))
	if err != nil {
		res.err = fmt.Sprintf("build submit request: %v", err)
		return res
	}
	submitReq.Header.Set("Content-Type", "application/json")

	t1 := time.Now()
	submitResp, err := client.Do(submitReq)
	res.submitLatency = time.Since(t1)
	if err != nil {
		res.err = fmt.Sprintf("submit: %v", err)
		return res
	}
	io.Copy(io.Discard, submitResp.Body) //nolint:errcheck
	submitResp.Body.Close()
	res.submitStatus = submitResp.StatusCode

	if submitResp.StatusCode == http.StatusCreated {
		res.ok = true
	} else {
		res.err = fmt.Sprintf("submit HTTP %d", submitResp.StatusCode)
	}
	return res
}

// ── Reporting ─────────────────────────────────────────────────────────────────

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func printReport(results []userResult, wall time.Duration) {
	var succeeded, failed int
	var loginMs, submitMs []float64
	statusCounts := map[int]int{}
	var failures []userResult

	for _, r := range results {
		if r.ok {
			succeeded++
		} else {
			failed++
			failures = append(failures, r)
		}
		if r.loginLatency > 0 {
			loginMs = append(loginMs, float64(r.loginLatency.Milliseconds()))
		}
		if r.submitLatency > 0 {
			submitMs = append(submitMs, float64(r.submitLatency.Milliseconds()))
			statusCounts[r.submitStatus]++
		}
	}

	sort.Float64s(loginMs)
	sort.Float64s(submitMs)

	fmt.Println()
	fmt.Println("═══════════════════════════════")
	fmt.Println("  Load Test Results")
	fmt.Println("═══════════════════════════════")
	fmt.Printf("  Wall time:   %s\n", wall.Round(time.Millisecond))
	fmt.Printf("  Total:       %d\n", len(results))
	fmt.Printf("  Succeeded:   %d\n", succeeded)
	fmt.Printf("  Failed:      %d\n", failed)

	if len(loginMs) > 0 {
		fmt.Printf("\n  Login latency (ms)\n")
		fmt.Printf("    p50=%.0f  p95=%.0f  p99=%.0f  max=%.0f\n",
			pct(loginMs, 50), pct(loginMs, 95), pct(loginMs, 99), loginMs[len(loginMs)-1])
	}
	if len(submitMs) > 0 {
		fmt.Printf("\n  Submit latency (ms)\n")
		fmt.Printf("    p50=%.0f  p95=%.0f  p99=%.0f  max=%.0f\n",
			pct(submitMs, 50), pct(submitMs, 95), pct(submitMs, 99), submitMs[len(submitMs)-1])
	}

	if len(statusCounts) > 0 {
		// Sort status codes for deterministic output.
		codes := make([]int, 0, len(statusCounts))
		for c := range statusCounts {
			codes = append(codes, c)
		}
		sort.Ints(codes)
		fmt.Println("\n  Submit HTTP status codes")
		for _, c := range codes {
			fmt.Printf("    %d: %d\n", c, statusCounts[c])
		}
	}

	if len(failures) > 0 {
		fmt.Printf("\n  Failures (%d)\n", len(failures))
		for _, r := range failures {
			fmt.Printf("    user %3d: %s\n", r.userID, r.err)
		}
	}
	fmt.Println("═══════════════════════════════")
}

// ── Main ──────────────────────────────────────────────────────────────────────

type config struct {
	baseURL      string
	code         string
	session      string
	namespace    string
	users        int
	rampDuration time.Duration
	timeout      time.Duration
}

func parseConfig() config {
	baseURL := flag.String("base-url", "https://vote.reversegitops.dev",
		"base URL of the voter app")
	code := flag.String("code", "",
		"join code shown on the presenter screen (required)")
	session := flag.String("session", "kubecon-2026",
		"quiz session name (QuizSession CR name)")
	namespace := flag.String("namespace", "vote",
		"Kubernetes namespace where the session lives")
	users := flag.Int("users", 100,
		"number of simulated participants")
	rampDuration := flag.Duration("ramp-duration", 30*time.Second,
		"window over which participant arrivals are spread")
	timeout := flag.Duration("timeout", 5*time.Minute,
		"hard deadline for the whole test")
	flag.Parse()

	if *code == "" {
		fmt.Fprintln(
			os.Stderr,
			"error: --code is required (grab it from the presenter screen or kubectl -n vote logs deploy/vote-auth-service | tail -1)",
		)
		flag.Usage()
		os.Exit(1)
	}

	return config{
		baseURL:      *baseURL,
		code:         *code,
		session:      *session,
		namespace:    *namespace,
		users:        *users,
		rampDuration: *rampDuration,
		timeout:      *timeout,
	}
}

func generateRunID() string {
	// Generate a short alphanumeric run ID (e.g. "q1g3") shared across all users
	// so names look like "q1g3-0", "q1g3-1", … and are stable within one run.
	const runIDChars = "abcdefghijklmnopqrstuvwxyz0123456789"
	runIDRng := rand.New(rand.NewSource(time.Now().UnixNano()))
	runIDb := make([]byte, 4)
	for i := range runIDb {
		runIDb[i] = runIDChars[runIDRng.Intn(len(runIDChars))]
	}
	return string(runIDb)
}

func printStart(cfg config, runID string) {
	fmt.Printf("Starting load test\n")
	fmt.Printf("  Base URL:      %s\n", cfg.baseURL)
	fmt.Printf("  Session:       %s/%s\n", cfg.namespace, cfg.session)
	fmt.Printf("  Join code:     %s\n", cfg.code)
	fmt.Printf("  Run ID:        %s\n", runID)
	fmt.Printf("  Participants:  %d\n", cfg.users)
	fmt.Printf("  Ramp duration: %s\n", cfg.rampDuration)
	fmt.Println()
}

func runLoadTest(ctx context.Context, cfg config, runID string) []userResult {
	resultCh := make(chan userResult, cfg.users)
	var wg sync.WaitGroup

	for i := range cfg.users {
		wg.Add(1)
		go func(userID int) {
			defer wg.Done()
			// Seed per goroutine so answers are not identical across users.
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(userID)*997))
			// Staggered arrival: uniform random delay within the ramp window.
			delay := time.Duration(rng.Float64() * float64(cfg.rampDuration))
			select {
			case <-ctx.Done():
				resultCh <- userResult{userID: userID, err: "cancelled before start"}
				return
			case <-time.After(delay):
			}
			resultCh <- simulateUser(ctx, userID, cfg.baseURL, cfg.code, runID, cfg.session, cfg.namespace, rng)
		}(i)
	}

	// Close the channel once all goroutines finish.
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Collect and live-print results as they arrive.
	results := make([]userResult, 0, cfg.users)
	done := 0
	for r := range resultCh {
		done++
		results = append(results, r)
		if r.ok {
			fmt.Printf("  [%3d/%d] user %3d  login=%4dms submit=%4dms  OK\n",
				done, cfg.users, r.userID,
				r.loginLatency.Milliseconds(), r.submitLatency.Milliseconds())
		} else {
			fmt.Printf("  [%3d/%d] user %3d  FAIL: %s\n",
				done, cfg.users, r.userID, r.err)
		}
	}

	return results
}

func main() {
	cfg := parseConfig()
	runID := generateRunID()
	printStart(cfg, runID)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	start := time.Now()
	results := runLoadTest(ctx, cfg, runID)
	printReport(results, time.Since(start))
}
