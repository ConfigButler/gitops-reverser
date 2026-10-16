// SPDX-License-Identifier: Apache-2.0

package repourl

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalize(t *testing.T) {
	tests := map[string]struct {
		in   string
		want string
	}{
		"https with .git":            {"https://github.com/acme/platform.git", "github.com/acme/platform"},
		"https without .git":         {"https://github.com/acme/platform", "github.com/acme/platform"},
		"trailing slash":             {"https://github.com/acme/platform/", "github.com/acme/platform"},
		"trailing slash after .git":  {"https://github.com/acme/platform.git/", "github.com/acme/platform"},
		"scp-like ssh":               {"git@github.com:acme/platform.git", "github.com/acme/platform"},
		"scp-like without user":      {"github.com:acme/platform", "github.com/acme/platform"},
		"ssh url":                    {"ssh://git@github.com/acme/platform.git", "github.com/acme/platform"},
		"ssh default port":           {"ssh://git@github.com:22/acme/platform.git", "github.com/acme/platform"},
		"https default port":         {"https://github.com:443/acme/platform", "github.com/acme/platform"},
		"http default port":          {"http://gitea.local:80/acme/platform", "gitea.local/acme/platform"},
		"non-default port is kept":   {"ssh://git@gitea.local:2222/acme/platform", "gitea.local:2222/acme/platform"},
		"userinfo dropped":           {"https://user:secret@github.com/acme/platform", "github.com/acme/platform"},
		"case folded":                {"https://GitHub.com/Acme/Platform.GIT", "github.com/acme/platform"},
		"surrounding whitespace":     {"  https://github.com/acme/platform  ", "github.com/acme/platform"},
		"nested group path":          {"https://gitlab.com/acme/team/platform.git", "gitlab.com/acme/team/platform"},
		"ipv6 with default port":     {"ssh://git@[::1]:22/acme/platform", "[::1]/acme/platform"},
		"ipv6 without port":          {"ssh://git@[::1]/acme/platform", "[::1]/acme/platform"},
		"empty":                      {"", ""},
		"whitespace only":            {"   ", ""},
		"unparseable stays itself":   {"Not A URL", "not a url"},
		"relative path is not a url": {"./a:b", "./a:b"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, Normalize(tc.in))
		})
	}
}

func TestSame(t *testing.T) {
	tests := map[string]struct {
		a, b string
		want bool
	}{
		"https and scp-like":   {"https://github.com/acme/platform.git", "git@github.com:acme/platform", true},
		"scp-like and ssh url": {"git@github.com:acme/platform.git", "ssh://git@github.com/acme/platform", true},
		"case and suffix": {
			"https://GITHUB.com/acme/Platform",
			"https://github.com/acme/platform.git",
			true,
		},
		"different repository":       {"https://github.com/acme/platform", "https://github.com/acme/other", false},
		"different host":             {"https://github.com/acme/platform", "https://gitlab.com/acme/platform", false},
		"different non-default port": {"ssh://git@h:2222/acme/p", "ssh://git@h:2223/acme/p", false},
		"both empty":                 {"", "", false},
		"one empty":                  {"", "https://github.com/acme/platform", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, Same(tc.a, tc.b))
			assert.Equal(t, tc.want, Same(tc.b, tc.a), "Same must be symmetric")
		})
	}
}
