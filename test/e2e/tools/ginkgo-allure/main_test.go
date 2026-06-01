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

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

func fixtureReport() []types.Report {
	start := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	return []types.Report{{
		SuitePath:        "/workspaces/gitops-reverser/test/e2e",
		SuiteDescription: "E2E Suite",
		StartTime:        start,
		SuiteConfig: types.SuiteConfig{
			RandomSeed: 12345,
		},
		SpecReports: []types.SpecReport{
			{
				LeafNodeType: types.NodeTypeBeforeSuite,
				LeafNodeText: "BeforeSuite",
			},
			{
				ContainerHierarchyTexts:    []string{"Manager", "Smoke"},
				ContainerHierarchyLabels:   [][]string{{"manager"}},
				LeafNodeType:               types.NodeTypeIt,
				LeafNodeText:               "passes",
				LeafNodeLabels:             []string{"smoke"},
				State:                      types.SpecStatePassed,
				StartTime:                  start,
				EndTime:                    start.Add(2 * time.Second),
				RunTime:                    2 * time.Second,
				ParallelProcess:            2,
				CapturedGinkgoWriterOutput: "useful output\n",
			},
			{
				ContainerHierarchyTexts: []string{"Manager"},
				LeafNodeType:            types.NodeTypeIt,
				LeafNodeText:            "fails",
				State:                   types.SpecStateFailed,
				StartTime:               start.Add(3 * time.Second),
				RunTime:                 time.Second,
				Failure: types.Failure{
					Message: "expected success",
					Location: types.CodeLocation{
						FileName:       "test/e2e/example_test.go",
						LineNumber:     42,
						FullStackTrace: "stack trace",
					},
				},
			},
			{
				ContainerHierarchyTexts: []string{"Manager"},
				LeafNodeType:            types.NodeTypeIt,
				LeafNodeText:            "times out",
				State:                   types.SpecStateTimedout,
			},
			{
				ContainerHierarchyTexts: []string{"Manager"},
				LeafNodeType:            types.NodeTypeIt,
				LeafNodeText:            "skips",
				State:                   types.SpecStateSkipped,
			},
		},
	}}
}

func TestRun_ConvertsGinkgoJSONToAllureResults(t *testing.T) {
	reportPath := writeFixtureReport(t, fixtureReport())
	outputDir := filepath.Join(t.TempDir(), "allure-results")

	count, err := run(outputDir, []string{reportPath})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if count != 4 {
		t.Fatalf("count: got %d, want 4", count)
	}

	results := readAllureResults(t, outputDir)
	if len(results) != 4 {
		t.Fatalf("results: got %d, want 4", len(results))
	}

	assertPassingResult(t, outputDir, findResult(t, results, "passes"))
	assertFailedResult(t, findResult(t, results, "fails"))

	timedOut := findResult(t, results, "times out")
	if timedOut.Status != "broken" {
		t.Errorf("timed out status: got %q", timedOut.Status)
	}

	skipped := findResult(t, results, "skips")
	if skipped.Status != "skipped" {
		t.Errorf("skipped status: got %q", skipped.Status)
	}
}

func assertPassingResult(t *testing.T, outputDir string, result allureResult) {
	t.Helper()
	if result.Status != "passed" {
		t.Errorf("passed status: got %q", result.Status)
	}
	if result.FullName != "Manager Smoke passes" {
		t.Errorf("fullName: got %q", result.FullName)
	}
	assertHasLabel(t, result, "framework", "ginkgo")
	assertHasLabel(t, result, "language", "go")
	assertHasLabel(t, result, "thread", "ginkgo-process-2")
	assertHasLabel(t, result, "suite", "E2E Suite")
	assertHasLabel(t, result, "package", "e2e")
	assertHasLabel(t, result, "parentSuite", "Manager")
	assertHasLabel(t, result, "subSuite", "Smoke")
	assertHasLabel(t, result, "tag", "manager")
	assertHasLabel(t, result, "tag", "smoke")
	assertHasParameter(t, result, "ginkgo.parallel_process", "2")
	assertHasParameter(t, result, "ginkgo.random_seed", "12345")
	if result.Start == 0 || result.Stop == 0 || result.Stop <= result.Start {
		t.Errorf("bad timing: start=%d stop=%d", result.Start, result.Stop)
	}
	if len(result.Attachments) != 1 {
		t.Fatalf("attachments: got %d, want 1", len(result.Attachments))
	}
	attachment, err := os.ReadFile(filepath.Join(outputDir, result.Attachments[0].Source))
	if err != nil {
		t.Fatalf("read attachment: %v", err)
	}
	if string(attachment) != "useful output\n" {
		t.Errorf("attachment: got %q", string(attachment))
	}
}

func assertFailedResult(t *testing.T, result allureResult) {
	t.Helper()
	if result.Status != "failed" {
		t.Errorf("failed status: got %q", result.Status)
	}
	if result.StatusDetails == nil || result.StatusDetails.Message != "expected success" {
		t.Fatalf("status details missing failure message: %#v", result.StatusDetails)
	}
	if result.StatusDetails.Trace != "stack trace" {
		t.Errorf("trace: got %q", result.StatusDetails.Trace)
	}
}

func TestMainRun_ExitCodes(t *testing.T) {
	good := writeFixtureReport(t, fixtureReport())
	cases := []struct {
		name     string
		args     []string
		wantCode int
	}{
		{name: "no args", args: nil, wantCode: exitUsage},
		{name: "empty output dir", args: []string{"--output-dir", "", good}, wantCode: exitUsage},
		{name: "bad flag", args: []string{"-not-a-flag"}, wantCode: exitUsage},
		{name: "missing report", args: []string{"--output-dir", t.TempDir(), "/no/such/file"}, wantCode: 1},
		{name: "success", args: []string{"--output-dir", t.TempDir(), good}, wantCode: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			got := mainRun(tc.args, &stdout, &stderr)
			if got != tc.wantCode {
				t.Errorf("mainRun: code=%d, want %d (stderr=%s)", got, tc.wantCode, stderr.String())
			}
		})
	}
}

func TestRun_BadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write bad fixture: %v", err)
	}
	_, err := run(t.TempDir(), []string{path})
	if err == nil {
		t.Fatal("expected invalid JSON error")
	}
}

func TestRun_IgnoresDryRunReports(t *testing.T) {
	reports := fixtureReport()
	reports[0].SuiteConfig.DryRun = true
	reportPath := writeFixtureReport(t, reports)
	outputDir := filepath.Join(t.TempDir(), "allure-results")

	count, err := run(outputDir, []string{reportPath})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if count != 0 {
		t.Fatalf("count: got %d, want 0", count)
	}
	if results := readAllureResults(t, outputDir); len(results) != 0 {
		t.Fatalf("results: got %d, want 0", len(results))
	}
}

func TestRun_IgnoresSpecsSkippedByLabelFilter(t *testing.T) {
	reports := fixtureReport()
	reports[0].SuiteConfig.LabelFilter = "smoke"
	reports[0].SpecReports = []types.SpecReport{
		{
			ContainerHierarchyTexts: []string{"Manager"},
			LeafNodeType:            types.NodeTypeIt,
			LeafNodeText:            "runs",
			State:                   types.SpecStatePassed,
			StartTime:               reports[0].StartTime,
			EndTime:                 reports[0].StartTime.Add(time.Second),
			RunTime:                 time.Second,
			ParallelProcess:         1,
		},
		{
			ContainerHierarchyTexts: []string{"Manager"},
			LeafNodeType:            types.NodeTypeIt,
			LeafNodeText:            "filtered out",
			State:                   types.SpecStateSkipped,
			StartTime:               reports[0].StartTime.Add(time.Second),
			ParallelProcess:         1,
		},
		{
			ContainerHierarchyTexts: []string{"Manager"},
			LeafNodeType:            types.NodeTypeIt,
			LeafNodeText:            "skips intentionally",
			State:                   types.SpecStateSkipped,
			StartTime:               reports[0].StartTime.Add(2 * time.Second),
			Failure: types.Failure{
				Message: "operator skipped",
			},
			ParallelProcess: 1,
		},
	}
	reportPath := writeFixtureReport(t, reports)
	outputDir := filepath.Join(t.TempDir(), "allure-results")

	count, err := run(outputDir, []string{reportPath})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if count != 2 {
		t.Fatalf("count: got %d, want 2", count)
	}
	results := readAllureResults(t, outputDir)
	if len(results) != 2 {
		t.Fatalf("results: got %d, want 2", len(results))
	}
	findResult(t, results, "runs")
	findResult(t, results, "skips intentionally")
	for _, result := range results {
		if result.Name == "filtered out" {
			t.Fatal("filtered-out spec was converted")
		}
	}
}

func TestRun_OutputDirCreateError(t *testing.T) {
	reportPath := writeFixtureReport(t, fixtureReport())
	outputDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(outputDir, []byte("file"), 0o600); err != nil {
		t.Fatalf("write output placeholder: %v", err)
	}
	_, err := run(outputDir, []string{reportPath})
	if err == nil {
		t.Fatal("expected output dir creation error")
	}
}

func TestConvertSpec_FallbacksAndFailureDetails(t *testing.T) {
	spec := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		State:        types.SpecStatePanicked,
		Failure: types.Failure{
			Message: "boom",
			Location: types.CodeLocation{
				FileName:   "test/e2e/example_test.go",
				LineNumber: 42,
			},
			ForwardedPanic: "panic payload",
		},
	}

	result := convertSpec("/tmp/ginkgo-report-empty.json", 0, 7, types.Report{}, spec)

	if result.FullName != "ginkgo-report-empty.json#7" {
		t.Errorf("fullName: got %q", result.FullName)
	}
	if result.Name != "" {
		t.Errorf("name: got %q, want empty fallback", result.Name)
	}
	if result.Status != "broken" {
		t.Errorf("status: got %q", result.Status)
	}
	if result.Start != 0 || result.Stop != 0 {
		t.Errorf("timing: start=%d stop=%d, want zero values", result.Start, result.Stop)
	}
	if result.StatusDetails == nil {
		t.Fatal("missing status details")
	}
	if result.StatusDetails.Message != "boom" {
		t.Errorf("message: got %q", result.StatusDetails.Message)
	}
	if result.StatusDetails.Trace != "test/e2e/example_test.go:42\npanic payload" {
		t.Errorf("trace: got %q", result.StatusDetails.Trace)
	}
}

func TestWriteResult_NoAttachment(t *testing.T) {
	outputDir := t.TempDir()
	result := allureResult{
		UUID:   "no-attachment",
		Name:   "passes",
		Status: "passed",
		Stage:  "finished",
	}
	if err := writeResult(outputDir, result, " \n\t"); err != nil {
		t.Fatalf("writeResult: %v", err)
	}

	results := readAllureResults(t, outputDir)
	if len(results) != 1 {
		t.Fatalf("results: got %d, want 1", len(results))
	}
	if len(results[0].Attachments) != 0 {
		t.Errorf("attachments: got %#v, want none", results[0].Attachments)
	}
}

func TestStatusMappings(t *testing.T) {
	cases := map[types.SpecState]string{
		types.SpecStatePassed:      "passed",
		types.SpecStateSkipped:     "skipped",
		types.SpecStatePending:     "skipped",
		types.SpecStateFailed:      "failed",
		types.SpecStatePanicked:    "broken",
		types.SpecStateAborted:     "broken",
		types.SpecStateInterrupted: "broken",
		types.SpecStateTimedout:    "broken",
		types.SpecStateInvalid:     "unknown",
	}
	for state, want := range cases {
		if got := allureStatus(state); got != want {
			t.Errorf("allureStatus(%s) = %q, want %q", state, got, want)
		}
	}
}

func writeFixtureReport(t *testing.T, reports []types.Report) string {
	t.Helper()
	data, err := json.Marshal(reports)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ginkgo-report.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write report: %v", err)
	}
	return path
}

func readAllureResults(t *testing.T, outputDir string) []allureResult {
	t.Helper()
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	results := make([]allureResult, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "-result.json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(outputDir, entry.Name()))
		if err != nil {
			t.Fatalf("read result: %v", err)
		}
		var result allureResult
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatalf("parse result: %v", err)
		}
		results = append(results, result)
	}
	return results
}

func findResult(t *testing.T, results []allureResult, name string) allureResult {
	t.Helper()
	for _, result := range results {
		if result.Name == name {
			return result
		}
	}
	t.Fatalf("missing result %q in %#v", name, results)
	return allureResult{}
}

func assertHasLabel(t *testing.T, result allureResult, name, value string) {
	t.Helper()
	for _, label := range result.Labels {
		if label.Name == name && label.Value == value {
			return
		}
	}
	t.Errorf("missing label %s=%s in %#v", name, value, result.Labels)
}

func assertHasParameter(t *testing.T, result allureResult, name, value string) {
	t.Helper()
	for _, parameter := range result.Parameters {
		if parameter.Name == name && parameter.Value == value {
			return
		}
	}
	t.Errorf("missing parameter %s=%s in %#v", name, value, result.Parameters)
}
