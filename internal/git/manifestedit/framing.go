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

import "strings"

const byteOrderMark = "\ufeff"

// reskinDocument re-applies the original document's framing to freshly encoded
// content. The encoder always emits LF, no BOM, a single trailing newline, and
// no document-end marker; this restores the original BOM, line-ending style,
// trailing-newline presence, and a trailing "..." marker so those survive an
// edit to another field in the same document.
func reskinDocument(original, encoded string) string {
	out := encoded

	if hasDocEndMarker(original) {
		out += "...\n"
	}
	if !strings.HasSuffix(original, "\n") {
		out = strings.TrimRight(out, "\n")
	}
	if usesCRLF(original) {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if strings.HasPrefix(original, byteOrderMark) {
		out = byteOrderMark + out
	}
	return out
}

// usesCRLF reports whether the original document uses Windows line endings.
func usesCRLF(original string) bool {
	return strings.Contains(original, "\r\n")
}

// hasDocEndMarker reports whether the document ends with a "..." marker line.
func hasDocEndMarker(original string) bool {
	lines := strings.Split(original, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimRight(lines[i], " \t\r")
		if line == "" {
			continue
		}
		return line == "..."
	}
	return false
}
