//go:build mutationlab_e2e

// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
)

// The rows of test/mutationlab/corpus/CLUSTER.md this test owns. The prose around the table is
// left alone.
var (
	imageRow    = regexp.MustCompile("(?m)^\\| k3d image \\| `([^`]*)` \\|$")
	versionRow  = regexp.MustCompile("(?m)^\\| Server version \\| `([^`]*)` \\(([^)]*)\\) \\|$")
	capturedRow = regexp.MustCompile(`(?m)^\| Captured at \| ([^|]*) \|$`)
)

// TestClusterProvenance keeps test/mutationlab/corpus/CLUSTER.md and the cluster that produced the
// corpus in agreement. With MUTATIONLAB_UPDATE it records the cluster's k3s image, server version,
// and the date; otherwise it fails when the cluster runs another version than the corpus was
// captured on, because a corpus diff is then a Kubernetes change, and is accepted with `task
// lab-corpus-update`, not read as a regression.
func TestClusterProvenance(t *testing.T) {
	if os.Getenv("LAB_API_URL") == "" {
		t.Skip("LAB_API_URL not set; run via `task lab-capture`")
	}
	image := os.Getenv("LAB_K3S_IMAGE")
	if image == "" {
		t.Fatal("LAB_K3S_IMAGE not set; run via `task lab-capture`")
	}
	cfg, err := restConfig()
	if err != nil {
		t.Fatalf("kube config: %v", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}
	info, err := kube.Discovery().ServerVersion()
	if err != nil {
		t.Fatalf("server version: %v", err)
	}
	// rancher/k3s:v1.36.4-k3s1 serves v1.36.4+k3s1. A mismatch means the image recorded in the
	// stamps is not what the cluster runs, and writing either into CLUSTER.md would be wrong.
	if want := k3sServerVersion(image); want != info.GitVersion {
		t.Fatalf("the lab cluster serves %s, but its stamps say it was created from %s", info.GitVersion, image)
	}

	path := filepath.Join("..", "corpus", "CLUSTER.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc := string(raw)
	if os.Getenv("MUTATIONLAB_UPDATE") != "" {
		updated, err := recordProvenance(doc, image, info.GitVersion, info.Platform, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("recorded %s (%s) in %s", info.GitVersion, image, path)
		return
	}
	m := versionRow.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("%s has no Server version row", path)
	}
	if m[1] != info.GitVersion {
		t.Fatalf("the corpus was captured on %s and the lab cluster serves %s: "+
			"run `task lab-corpus-update` and review the corpus diff as that version's changes", m[1], info.GitVersion)
	}
}

// k3sServerVersion maps a k3s image to the version its API server reports.
func k3sServerVersion(image string) string {
	tag := image[strings.LastIndex(image, ":")+1:]
	return strings.Replace(tag, "-k3s", "+k3s", 1)
}

// recordProvenance rewrites the three provenance rows of CLUSTER.md, and fails rather than append
// a row it cannot find, since the table's shape is what `task lab-bootstrap` reads the default
// k3s image from.
func recordProvenance(doc, image, version, platform string, now time.Time) (string, error) {
	rows := []struct {
		re  *regexp.Regexp
		row string
	}{
		{imageRow, fmt.Sprintf("| k3d image | `%s` |", image)},
		{versionRow, fmt.Sprintf("| Server version | `%s` (%s) |", version, platform)},
		{capturedRow, fmt.Sprintf("| Captured at | %s |", now.Format(time.DateOnly))},
	}
	for _, r := range rows {
		if !r.re.MatchString(doc) {
			return "", fmt.Errorf("CLUSTER.md has no row matching %s", r.re)
		}
		doc = r.re.ReplaceAllLiteralString(doc, r.row)
	}
	return doc, nil
}
