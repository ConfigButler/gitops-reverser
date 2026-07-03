// SPDX-License-Identifier: Apache-2.0

package manifestedit

import "strings"

// rawDoc is one YAML document carved out of a file as exact bytes. The file is
// reconstructed by concatenating sep+body of every rawDoc in order, so unrelated
// documents survive an edit byte-for-byte.
type rawDoc struct {
	// sep is the leading separator segment ("" for the first document, "---\n"
	// or similar otherwise), preserved verbatim.
	sep string
	// body is the document content, preserved verbatim.
	body string
}

// splitDocuments carves content into its YAML documents without losing a byte.
// It is block-scalar aware so a "---" line inside a literal block is treated as
// content, not as a document separator — matching real YAML semantics, where a
// less-indented "---" would end the block anyway.
func splitDocuments(content string) []rawDoc {
	lines := splitLinesKeepEnds(content)

	var docs []rawDoc
	cur := rawDoc{}
	inBlock := false
	blockIndent := 0

	addBody := func(line string) { cur.body += line }

	for _, line := range lines {
		if inBlock {
			if isBlankLine(line) || leadingSpaces(line) > blockIndent {
				addBody(line)
				continue
			}
			inBlock = false // less indented: the block ended; reprocess this line
		}

		if isSeparatorLine(line) {
			// A separator with nothing at all before it just opens the first
			// document; it does not create a spurious empty document 0.
			if len(docs) == 0 && cur.sep == "" && cur.body == "" {
				cur.sep = line
				continue
			}
			docs = append(docs, cur)
			cur = rawDoc{sep: line}
			continue
		}

		if ind, ok := opensBlockScalar(line); ok {
			inBlock = true
			blockIndent = ind
		}
		addBody(line)
	}

	docs = append(docs, cur)
	return docs
}

// DocumentCount reports how many non-empty YAML documents a file holds. It is
// block-scalar aware (it reuses the byte-faithful splitter), and ignores empty
// documents such as a trailing "---". Callers use it to refuse a single-document
// wholesale write that would silently drop the other documents in a shared file.
func DocumentCount(content []byte) int {
	n := 0
	for _, d := range splitDocuments(string(content)) {
		if strings.TrimSpace(d.body) != "" {
			n++
		}
	}
	return n
}

// joinDocuments reassembles documents into file content.
func joinDocuments(docs []rawDoc) string {
	var b strings.Builder
	for _, d := range docs {
		b.WriteString(d.sep)
		b.WriteString(d.body)
	}
	return b.String()
}

// splitLinesKeepEnds splits content into lines, each retaining its line ending
// (\n or \r\n). The final line keeps whatever ending it had, possibly none.
func splitLinesKeepEnds(content string) []string {
	var lines []string
	start := 0
	for i := range len(content) {
		if content[i] == '\n' {
			lines = append(lines, content[start:i+1])
			start = i + 1
		}
	}
	if start < len(content) {
		lines = append(lines, content[start:])
	}
	return lines
}

// stripLineEnding removes a trailing \n and \r from a line.
func stripLineEnding(line string) string {
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line
}

// isBlankLine reports whether a line is empty or only whitespace.
func isBlankLine(line string) bool {
	return strings.TrimSpace(line) == ""
}

// leadingSpaces counts the indentation of a line. Tabs are not valid YAML
// indentation, so a tab is treated as a large indent to keep such lines inside a
// surrounding block rather than misreading them as structure.
func leadingSpaces(line string) int {
	n := 0
	for _, c := range line {
		switch c {
		case ' ':
			n++
		case '\t':
			n += 8
		default:
			return n
		}
	}
	return n
}

// isSeparatorLine reports whether a line is a YAML document separator ("---").
func isSeparatorLine(line string) bool {
	s := strings.TrimRight(stripLineEnding(line), " \t")
	return s == "---" || strings.HasPrefix(s, "--- ")
}

// opensBlockScalar reports whether a line introduces a literal/folded block
// scalar (value is |, >, optionally with chomping/indent indicators), returning
// the indentation of the introducing line.
func opensBlockScalar(line string) (int, bool) {
	indent := leadingSpaces(line)
	s := stripTrailingComment(strings.TrimRight(stripLineEnding(line), " \t"))
	s = strings.TrimRight(s, " \t")

	var val string
	switch {
	case strings.Contains(s, ": "):
		val = strings.TrimSpace(s[strings.LastIndex(s, ": ")+1:])
	case strings.HasSuffix(s, ":"):
		val = ""
	default:
		// sequence entry like "- |"
		val = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "-"))
	}

	if val == "" || (val[0] != '|' && val[0] != '>') {
		return 0, false
	}
	for _, c := range val[1:] {
		if c != '+' && c != '-' && (c < '0' || c > '9') {
			return 0, false
		}
	}
	return indent, true
}

// stripTrailingComment removes a trailing " #..." comment from a line fragment.
// It is a heuristic: it only triggers on a hash preceded by whitespace, which is
// enough for the block-scalar indicator detection it supports.
func stripTrailingComment(s string) string {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return s[:i]
		}
	}
	return s
}
