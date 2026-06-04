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

package manifestanalyzer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// TestScan_FullPlanAndRefusals proves the dry-run shows the full plan (a create and
// a patch) alongside an acceptance refusal, computing both and writing nothing.
func TestScan_FullPlanAndRefusals(t *testing.T) {
	fsys := fstest.MapFS{
		"deploy.yaml": {Data: []byte(deployYAML)},      // resolved; differs from desired → patch
		"secret.yaml": {Data: []byte(plainSecretYAML)}, // served but unwatched → refusal
	}
	desired := []DesiredResource{desiredDeployWeb(3), desiredConfigMap("new")} // patch + create
	result := Scan(context.Background(), fsys, snapMapper(), desired, ScanPolicy{})

	if result.Acceptance.Accepted {
		t.Fatalf("the unwatched Secret should refuse the folder")
	}
	if countAcceptance(result.Acceptance, IssueUnwatchedAPIKRM) != 1 {
		t.Errorf("want one unwatched-api-krm refusal, got %+v", result.Acceptance.Issues)
	}

	counts := result.Plan.Counts()
	if counts[PlanPatch] != 1 || counts[PlanCreate] != 1 || len(result.Plan.Actions) != 2 {
		t.Fatalf("plan = %+v, want one patch and one create", result.Plan.Actions)
	}
	if result.Store == nil {
		t.Errorf("scan should return the built store")
	}
}

// TestScan_StructureOnly proves the no-cluster dry-run: an empty plan plus the
// structural refusals (non-KRM YAML), with the allowlist applied.
func TestScan_StructureOnly(t *testing.T) {
	fsys := fstest.MapFS{
		"deploy.yaml":        {Data: []byte(deployYAML)},
		"values.yaml":        {Data: []byte(plainYAML)},      // non-KRM → refusal
		"kustomization.yaml": {Data: []byte(kustomizationY)}, // allowlisted → retained
	}
	policy := ScanPolicy{Acceptance: AcceptancePolicy{Allowlist: DefaultAllowlist()}}
	result := Scan(context.Background(), fsys, nil, nil, policy)

	if len(result.Plan.Actions) != 0 {
		t.Fatalf("structure-only scan must not plan anything, got %+v", result.Plan.Actions)
	}
	if countAcceptance(result.Acceptance, IssueNonKRM) != 1 {
		t.Errorf("values.yaml should refuse as non-KRM, got %+v", result.Acceptance.Issues)
	}
	if len(result.Acceptance.Retained) != 1 {
		t.Errorf("kustomization.yaml should be retained, got %+v", result.Acceptance.Retained)
	}
}

func TestScan_AcceptedInSync(t *testing.T) {
	fsys := fstest.MapFS{
		"deploy.yaml": {Data: []byte(deployYAML)},
		"cm.yaml":     {Data: []byte(configMapsYAML)},
	}
	desired := []DesiredResource{desiredDeployWeb(1), desiredConfigMap("a"), desiredConfigMap("b")}
	result := Scan(context.Background(), fsys, snapMapper(), desired, ScanPolicy{})

	if !result.Acceptance.Accepted {
		t.Fatalf("clean in-sync folder should be accepted: %+v", result.Acceptance.Issues)
	}
	if len(result.Plan.Actions) != 0 {
		t.Errorf("in-sync folder should plan no changes, got %+v", result.Plan.Actions)
	}
}

func TestScanDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deploy.yaml"), []byte(deployYAML), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	result, err := ScanDir(context.Background(), dir, nil, nil, ScanPolicy{})
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	if result.Store.Root != dir {
		t.Errorf("root = %q, want %q", result.Store.Root, dir)
	}
}

func TestScanDir_Errors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := ScanDir(context.Background(), missing, nil, nil, ScanPolicy{}); err == nil {
		t.Error("expected error for a missing directory")
	}
	file := filepath.Join(t.TempDir(), "f.yaml")
	if err := os.WriteFile(file, []byte(deployYAML), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := ScanDir(context.Background(), file, nil, nil, ScanPolicy{}); err == nil {
		t.Error("expected error when root is not a directory")
	}
}
