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

package manifestedit

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// IndexDir recursively scans a folder for YAML manifests and builds an inventory.
// Paths in the inventory are relative to root. Symlinks are never followed: a
// symlinked file or directory is skipped, which avoids escaping the scan root and
// symlink cycles.
func IndexDir(root string) (Inventory, []Diagnostic) {
	var files []FileContent
	var diags []Diagnostic

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Record the walk error and keep scanning the rest of the tree.
			diags = append(diags, Diagnostic{Level: DiagWarning, Path: path, Message: err.Error()})
			return nil //nolint:nilerr // a per-entry error must not abort the whole scan
		}
		// Never follow symlinks, for files or directories.
		if d.Type()&fs.ModeSymlink != 0 {
			rel := relPath(root, path)
			diags = append(diags, Diagnostic{Level: DiagInfo, Path: rel, Message: "symlink skipped"})
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !isYAMLFile(path) {
			return nil
		}
		content, readErr := os.ReadFile(path) //nolint:gosec // scanning a user-pointed manifest folder is the feature
		if readErr != nil {
			diags = append(diags, Diagnostic{Level: DiagWarning, Path: relPath(root, path), Message: readErr.Error()})
			return nil //nolint:nilerr // an unreadable file must not abort the whole scan
		}
		files = append(files, FileContent{Path: relPath(root, path), Content: content})
		return nil
	})
	if walkErr != nil {
		diags = append(diags, Diagnostic{Level: DiagError, Path: root, Message: walkErr.Error()})
	}

	inv, indexDiags := IndexFiles(files)
	diags = append(diags, indexDiags...)
	return inv, diags
}

// isYAMLFile reports whether a path is a YAML manifest by extension.
func isYAMLFile(path string) bool {
	return strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")
}

// relPath returns path relative to root, falling back to path on error.
func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
