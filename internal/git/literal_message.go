// SPDX-License-Identifier: Apache-2.0

package git

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// ValidateLiteralCommitMessage checks a literal override without altering its bytes.
// Empty means omission internally; the API schema rejects an explicitly empty field.
func ValidateLiteralCommitMessage(message string) error {
	if message == "" {
		return nil
	}
	if !utf8.ValidString(message) || utf8.RuneCountInString(message) > 1024 {
		return errors.New("commit message must contain at most 1024 valid Unicode characters")
	}
	if strings.TrimSpace(message) == "" {
		return errors.New("commit message must contain a non-whitespace character")
	}
	for _, r := range message {
		if (r < 0x20 && r != '\n') || r == 0x7f {
			return errors.New("commit message must not contain ASCII control characters other than newline")
		}
	}
	return nil
}
