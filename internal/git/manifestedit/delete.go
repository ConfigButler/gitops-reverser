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

// EditDeleted means a document was removed from the file.
const EditDeleted EditMode = "deleted"

// DeleteResult is the outcome of removing one document from a file.
type DeleteResult struct {
	// Content is the file content after removal. It is nil when FileEmpty is true.
	Content []byte
	// FileEmpty is true when the removed document was the only one, so the caller
	// should delete the file rather than write empty content.
	FileEmpty bool
	Mode      EditMode
}

// DeleteDocument removes one document from a file, leaving every surviving
// document's content byte-for-byte intact. Removing the only document reports
// FileEmpty so the caller can delete the file. This serves both resource deletion
// and pruning a duplicate loser.
//
// It is a thin wrapper over Decide + Apply with Desired == nil. Deletion is the
// content-agnostic cell of the comparison: it never decrypts or merges, so an
// encrypted document, a disallowed-construct document, or a duplicate loser can
// always be pruned. No renderer is needed.
//
// When the first document is removed, the new first document's leading "---"
// separator is dropped so the file does not start with a stray separator. Only
// the separator is affected; the document content is unchanged. We deliberately
// prefer a clean leading document over preserving a now-pointless separator.
func DeleteDocument(content []byte, documentIndex int) (DeleteResult, []Diagnostic) {
	git, _ := NewDocument(content, documentIndex)
	c := Comparison{Git: git, Desired: nil}
	res, diags := Apply(c, Decide(c))

	if res.Mode != EditDeleted {
		return DeleteResult{Content: content, Mode: res.Mode}, diags
	}
	if len(res.Content) == 0 {
		return DeleteResult{FileEmpty: true, Mode: EditDeleted}, diags
	}
	return DeleteResult{Content: res.Content, Mode: EditDeleted}, diags
}
