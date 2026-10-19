// SPDX-License-Identifier: Apache-2.0

/*
Package repourl decides whether two spellings of a Git repository URL name the same repository.

One repository has many spellings: GitHub hands out https://github.com/acme/platform.git,
git@github.com:acme/platform.git and https://github.com/acme/platform in the same payload, and a
GitProvider's spec.url can be any of them. Comparing the strings would call those three different
repositories, so a relay that names the repository one way would be ignored for a provider that
spells it another.

The comparison is deliberately loose about transport and strict about identity: scheme, userinfo,
default ports, a trailing ".git" and case are dropped, while the host, a non-default port and the
path are kept. Case is dropped from the path too, because the hosts this is used with (GitHub,
GitLab, Gitea) resolve repository paths case-insensitively.
*/
package repourl

import (
	"net"
	"net/url"
	"strings"
)

// isDefaultPort reports whether a URL can name port without changing which server it reaches: SSH,
// HTTPS and HTTP. Any other port is part of the identity, because it can be a different server.
func isDefaultPort(port string) bool {
	switch port {
	case "22", "443", "80":
		return true
	default:
		return false
	}
}

// Normalize returns the canonical "host[:port]/path" form of a repository URL, or "" for an empty
// one. It accepts URL forms (https://, http://, ssh://, git://) and the scp-like SSH form
// "[user@]host:path". A string it cannot parse is returned lowercased and trimmed, so it can still
// only ever equal itself.
func Normalize(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	host, path, ok := splitSCPLike(raw)
	if !ok {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return strings.ToLower(raw)
		}
		host, path = u.Host, u.Path
	}

	return canonicalHost(host) + "/" + canonicalPath(path)
}

// Same reports whether a and b name the same repository. Two empty URLs are not the same
// repository: an absent value is no evidence of a match.
func Same(a, b string) bool {
	na := Normalize(a)
	return na != "" && na == Normalize(b)
}

// splitSCPLike recognizes the scp-like SSH form git uses, "[user@]host:path", and returns its host
// and path. It is the form only when there is no scheme and the first colon comes before the first
// slash: "host:org/repo" is scp-like, "./a:b" is a local path.
func splitSCPLike(raw string) (string, string, bool) {
	if strings.Contains(raw, "://") {
		return "", "", false
	}
	colon := strings.Index(raw, ":")
	if colon <= 0 {
		return "", "", false
	}
	if slash := strings.Index(raw, "/"); slash >= 0 && slash < colon {
		return "", "", false
	}
	host := raw[:colon]
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	return host, raw[colon+1:], true
}

// canonicalHost lowercases a host, drops any userinfo, and drops the port when it is a default one.
func canonicalHost(host string) string {
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	host = strings.ToLower(host)
	if h, port, err := net.SplitHostPort(host); err == nil && isDefaultPort(port) {
		if strings.Contains(h, ":") {
			return "[" + h + "]" // an IPv6 literal keeps the brackets it has without a port
		}
		return h
	}
	return host
}

// canonicalPath lowercases a repository path and strips the decoration that does not change which
// repository it names: surrounding slashes and a trailing ".git".
func canonicalPath(path string) string {
	path = strings.ToLower(strings.Trim(path, "/"))
	path = strings.TrimSuffix(path, ".git")
	return strings.TrimRight(path, "/")
}
