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

package corpus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

func watchRec(watchType, name, uid, rv string) mutationlab.Record {
	raw := `{"type":"` + watchType + `","object":{"metadata":{"name":"` + name +
		`","uid":"` + uid + `","resourceVersion":"` + rv + `"}}}`
	return mutationlab.Record{
		Source:  mutationlab.SourceWatch,
		Key:     mutationlab.ObjectKey{Name: name},
		Summary: mutationlab.RecordSummary{WatchType: watchType},
		Raw:     json.RawMessage(raw),
	}
}

func TestBuild_NamesAndNormalizes(t *testing.T) {
	records := []mutationlab.Record{
		watchRec("ADDED", "cm-a", "u1", "10"),
		{
			Source:  mutationlab.SourceAudit,
			Summary: mutationlab.RecordSummary{Operation: "create", AuditID: "x"},
			Raw:     json.RawMessage(`{"auditID":"x","verb":"create"}`),
		},
	}
	moments, err := Build(records)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if moments[0].Name != "watch.added.yaml" {
		t.Errorf("name[0] = %q, want watch.added.yaml", moments[0].Name)
	}
	if moments[1].Name != "audit.create.yaml" {
		t.Errorf("name[1] = %q, want audit.create.yaml", moments[1].Name)
	}
	// Volatile fields are normalized before they reach the file.
	if !strings.Contains(string(moments[0].Content), "<uid-1>") ||
		!strings.Contains(string(moments[0].Content), "<rv-1>") {
		t.Errorf("watch moment not normalized:\n%s", moments[0].Content)
	}
	if !strings.Contains(string(moments[1].Content), "<auditID-1>") {
		t.Errorf("audit moment not normalized:\n%s", moments[1].Content)
	}
}

func TestBuild_FanoutDiscriminatedByName(t *testing.T) {
	records := []mutationlab.Record{
		watchRec("DELETED", "cm-a", "ua", "10"),
		watchRec("DELETED", "cm-b", "ub", "11"),
		watchRec("DELETED", "cm-c", "uc", "12"),
	}
	moments, err := Build(records)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := []string{moments[0].Name, moments[1].Name, moments[2].Name}
	want := []string{"watch.deleted.cm-a.yaml", "watch.deleted.cm-b.yaml", "watch.deleted.cm-c.yaml"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("name[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuild_CollisionWithoutNamesUsesOrdinal(t *testing.T) {
	mk := func() mutationlab.Record {
		return mutationlab.Record{
			Source:  mutationlab.SourceAudit,
			Summary: mutationlab.RecordSummary{Operation: "deletecollection"},
			Raw:     json.RawMessage(`{"verb":"deletecollection"}`),
		}
	}
	moments, err := Build([]mutationlab.Record{mk(), mk()})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if moments[0].Name != "audit.deletecollection.1.yaml" || moments[1].Name != "audit.deletecollection.2.yaml" {
		t.Errorf("ordinal discriminator wrong: %q, %q", moments[0].Name, moments[1].Name)
	}
}

func TestWriteThenCompare_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	moments, err := Build([]mutationlab.Record{watchRec("ADDED", "cm-a", "u1", "10")})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := Write(dir, moments); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Compare(dir, moments); err != nil {
		t.Fatalf("Compare after Write should pass: %v", err)
	}
}

func TestCompare_MissingFile(t *testing.T) {
	dir := t.TempDir()
	moments, _ := Build([]mutationlab.Record{watchRec("ADDED", "cm-a", "u1", "10")})
	err := Compare(dir, moments)
	if err == nil || !strings.Contains(err.Error(), "missing corpus file") {
		t.Fatalf("expected missing-file error, got %v", err)
	}
}

func TestCompare_DriftProducesDiff(t *testing.T) {
	dir := t.TempDir()
	moments, _ := Build([]mutationlab.Record{watchRec("ADDED", "cm-a", "u1", "10")})
	if err := Write(dir, moments); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Mutate the committed file so Compare sees drift.
	path := filepath.Join(dir, moments[0].Name)
	if err := os.WriteFile(path, []byte("type: TAMPERED\n"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err := Compare(dir, moments)
	if err == nil || !strings.Contains(err.Error(), "corpus drift") {
		t.Fatalf("expected drift error, got %v", err)
	}
}

func TestCompare_StrayFile(t *testing.T) {
	dir := t.TempDir()
	moments, _ := Build([]mutationlab.Record{watchRec("ADDED", "cm-a", "u1", "10")})
	if err := Write(dir, moments); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "watch.stray.yaml"), []byte("x: 1\n"), 0o644); err != nil {
		t.Fatalf("stray: %v", err)
	}
	err := Compare(dir, moments)
	if err == nil || !strings.Contains(err.Error(), "stray corpus files") {
		t.Fatalf("expected stray error, got %v", err)
	}
}

func TestWrite_RemovesStaleFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "watch.old.yaml"), []byte("x: 1\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	moments, _ := Build([]mutationlab.Record{watchRec("ADDED", "cm-a", "u1", "10")})
	if err := Write(dir, moments); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "watch.old.yaml")); !os.IsNotExist(err) {
		t.Fatalf("stale file should be removed, stat err = %v", err)
	}
}
