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

func fixtureReports() []types.Report {
	return []types.Report{{
		SpecReports: []types.SpecReport{
			{
				LeafNodeType: types.NodeTypeBeforeSuite,
				LeafNodeText: "BeforeSuite",
				RunTime:      time.Second,
			},
			{
				LeafNodeType:            types.NodeTypeIt,
				ContainerHierarchyTexts: []string{"Manager", "Manager"},
				LeafNodeText:            "fast spec",
				State:                   types.SpecStatePassed,
				RunTime:                 300 * time.Millisecond,
			},
			{
				LeafNodeType:            types.NodeTypeIt,
				ContainerHierarchyTexts: []string{"Restart Snapshot Safety"},
				LeafNodeText:            "long spec",
				State:                   types.SpecStateFailed,
				RunTime:                 90 * time.Second,
			},
			{
				LeafNodeType:            types.NodeTypeIt,
				ContainerHierarchyTexts: []string{"Audit Redis Queue"},
				LeafNodeText:            "middle spec",
				State:                   types.SpecStatePassed,
				RunTime:                 10 * time.Second,
			},
			{
				LeafNodeType:            types.NodeTypeIt,
				ContainerHierarchyTexts: []string{"Should Be Skipped"},
				LeafNodeText:            "skipped",
				State:                   types.SpecStateSkipped,
				RunTime:                 0,
			},
			{
				LeafNodeType:            types.NodeTypeIt,
				ContainerHierarchyTexts: []string{"Should Be Pending"},
				LeafNodeText:            "pending",
				State:                   types.SpecStatePending,
				RunTime:                 0,
			},
		},
	}}
}

func TestParseReport_FiltersAndSortsAndDedupes(t *testing.T) {
	data, err := json.Marshal(fixtureReports())
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	rows, total, err := parseReport(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parseReport: %v", err)
	}

	wantNames := []string{
		"Restart Snapshot Safety long spec",
		"Audit Redis Queue middle spec",
		"Manager fast spec",
	}
	if len(rows) != len(wantNames) {
		t.Fatalf("rows: want %d, got %d (%v)", len(wantNames), len(rows), rows)
	}
	for i, want := range wantNames {
		if rows[i].name != want {
			t.Errorf("row[%d]: want %q, got %q", i, want, rows[i].name)
		}
	}

	wantTotal := 90*time.Second + 10*time.Second + 300*time.Millisecond
	if total != wantTotal {
		t.Errorf("total: want %v, got %v", wantTotal, total)
	}
}

func TestWriteTable_FormatAndPercentages(t *testing.T) {
	rows := []specRow{
		{name: "Restart Snapshot Safety long spec", duration: 90 * time.Second},
		{name: "Audit Redis Queue middle spec", duration: 10 * time.Second},
		{name: "Manager fast spec", duration: 300 * time.Millisecond},
	}
	total := 90*time.Second + 10*time.Second + 300*time.Millisecond

	var buf bytes.Buffer
	writeTable(&buf, rows, total)

	got := buf.String()
	wantLines := []string{
		"  Duration     %  Spec",
		"    90.0 s  89.7  Restart Snapshot Safety long spec",
		"    10.0 s  10.0  Audit Redis Queue middle spec",
		"     0.3 s   0.3  Manager fast spec",
		"   100.3 s  total",
	}
	for _, line := range wantLines {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("output missing line %q\nfull output:\n%s", line, got)
		}
	}
}

func TestWriteTable_ZeroTotalAvoidsDivision(t *testing.T) {
	var buf bytes.Buffer
	writeTable(&buf, nil, 0)
	got := buf.String()
	if !strings.Contains(got, "  Duration     %  Spec") {
		t.Errorf("missing header: %s", got)
	}
	if !strings.Contains(got, "     0.0 s  total") {
		t.Errorf("missing total row: %s", got)
	}
}

func TestDedupeConsecutive(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{nil, []string{}},
		{[]string{"a"}, []string{"a"}},
		{[]string{"a", "a"}, []string{"a"}},
		{[]string{"a", "a", "b", "b", "a"}, []string{"a", "b", "a"}},
		{[]string{"Manager", "Manager", "spec"}, []string{"Manager", "spec"}},
	}
	for _, tc := range cases {
		got := dedupeConsecutive(tc.in)
		if !equalStrings(got, tc.want) {
			t.Errorf("dedupeConsecutive(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRun_EndToEndWithFixtureFile(t *testing.T) {
	data, err := json.Marshal(fixtureReports())
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var buf bytes.Buffer
	if err := run(path, &buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "Restart Snapshot Safety long spec") {
		t.Errorf("missing top spec in output: %s", got)
	}
	if !strings.Contains(got, "100.3 s  total") {
		t.Errorf("missing total in output: %s", got)
	}
}

func TestRun_MissingFile(t *testing.T) {
	err := run(filepath.Join(t.TempDir(), "does-not-exist.json"), &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestRun_BadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	err := run(path, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestMainRun_ExitCodes(t *testing.T) {
	good := filepath.Join(t.TempDir(), "report.json")
	data, err := json.Marshal(fixtureReports())
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(good, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cases := []struct {
		name     string
		args     []string
		wantCode int
	}{
		{name: "no args", args: nil, wantCode: exitUsage},
		{name: "too many args", args: []string{"a", "b"}, wantCode: exitUsage},
		{name: "bad flag", args: []string{"-not-a-flag"}, wantCode: exitUsage},
		{name: "missing file", args: []string{"/no/such/file"}, wantCode: 1},
		{name: "success", args: []string{good}, wantCode: 0},
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
