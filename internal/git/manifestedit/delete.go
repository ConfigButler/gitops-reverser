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

// DeleteDocument removes one document from a file, leaving the other documents
// byte-for-byte intact. Removing the only document reports FileEmpty so the
// caller can delete the file. This serves both resource deletion and pruning a
// duplicate loser.
func DeleteDocument(content []byte, documentIndex int) (DeleteResult, []Diagnostic) {
	docs := splitDocuments(string(content))

	if documentIndex < 0 || documentIndex >= len(docs) {
		return DeleteResult{Content: content, Mode: EditSkipped},
			[]Diagnostic{{Level: DiagError, DocumentIndex: documentIndex, Message: "document index out of range"}}
	}

	if len(docs) == 1 {
		return DeleteResult{FileEmpty: true, Mode: EditDeleted}, nil
	}

	docs = append(docs[:documentIndex], docs[documentIndex+1:]...)
	// If the first document was removed, the new first document may carry a
	// leading "---" separator. Drop it so the file does not start with one.
	if documentIndex == 0 {
		docs[0].sep = ""
	}

	return DeleteResult{Content: []byte(joinDocuments(docs)), Mode: EditDeleted}, nil
}
