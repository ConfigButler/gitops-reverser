// SPDX-License-Identifier: Apache-2.0

package git

// The request ledger answers one question the rest of the test suite cannot: how many times does a
// given operation talk to the Git host, and does each of those conversations transfer anything?
//
// Counts alone are not enough. A `SmartFetch` against an unmoved remote opens the same two
// upload-pack conversations as one that pulls a whole tree, and only the second is expensive. So
// every row carries request and response bytes beside its count, and the golden file publishes a
// coarse size class for them — see sizeClass for why the class rather than the number.
//
// See docs/design/push-notification-and-reconcile-trigger.md §1.6, which specifies this ledger.

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// gitRequestKind is one of the four things a smart-HTTP git conversation can be. The pairing is
// the point: an `info/refs` GET is a conversation opened, and the POST beside it is objects
// actually moved, so a fetch that transfers nothing shows as a GET with no POST.
type gitRequestKind string

const (
	// infoRefsUploadPack is a fetch conversation opened.
	infoRefsUploadPack gitRequestKind = "GET /info/refs?service=git-upload-pack"
	// uploadPackPost is objects actually requested.
	uploadPackPost gitRequestKind = "POST /git-upload-pack"
	// infoRefsReceivePack is a push conversation opened: the advertisement the compare-and-swap
	// reads.
	infoRefsReceivePack gitRequestKind = "GET /info/refs?service=git-receive-pack"
	// receivePackPost is a packfile sent.
	receivePackPost gitRequestKind = "POST /git-receive-pack"
)

// gitRequestKinds is the fixed report order, chosen so a fetch conversation and its transfer sit
// next to each other.
var gitRequestKinds = []gitRequestKind{
	infoRefsUploadPack,
	uploadPackPost,
	infoRefsReceivePack,
	receivePackPost,
}

// gitRequestStat is one kind's tally.
type gitRequestStat struct {
	Count     int64
	ReqBytes  int64
	RespBytes int64
}

// gitRequestLedger tallies every request canonical git's http-backend is asked to serve.
//
// It is written from the server's goroutines and read from the test's, so it carries a mutex
// rather than the atomics the multi-ack counter uses: a snapshot has to be internally consistent
// across all four kinds, which four independent atomics cannot promise.
type gitRequestLedger struct {
	mu   sync.Mutex
	rows map[gitRequestKind]gitRequestStat
}

func newGitRequestLedger() *gitRequestLedger {
	return &gitRequestLedger{rows: map[gitRequestKind]gitRequestStat{}}
}

// classify names the conversation a request belongs to, or "" for anything the git protocol does
// not define (a dumb-HTTP object GET, say, which our clients never issue).
func classifyGitRequest(method, path, rawQuery string) gitRequestKind {
	switch {
	case method == http.MethodGet && strings.HasSuffix(path, "/info/refs"):
		switch {
		case strings.Contains(rawQuery, "service=git-upload-pack"):
			return infoRefsUploadPack
		case strings.Contains(rawQuery, "service=git-receive-pack"):
			return infoRefsReceivePack
		}
	case method == http.MethodPost && strings.HasSuffix(path, "/git-upload-pack"):
		return uploadPackPost
	case method == http.MethodPost && strings.HasSuffix(path, "/git-receive-pack"):
		return receivePackPost
	}
	return ""
}

func (l *gitRequestLedger) record(kind gitRequestKind, reqBytes, respBytes int64) {
	if kind == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	row := l.rows[kind]
	row.Count++
	row.ReqBytes += reqBytes
	row.RespBytes += respBytes
	l.rows[kind] = row
}

// snapshot copies the tally. Every measurement in the ledger tests is a DELTA between two
// snapshots, never an absolute: the worker's own setup (a clone, a bootstrap) is traffic the
// operation under test did not cause.
func (l *gitRequestLedger) snapshot() gitRequestSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := gitRequestSnapshot{}
	for kind, row := range l.rows {
		out[kind] = row
	}
	return out
}

// gitRequestSnapshot is a ledger reading at one instant.
type gitRequestSnapshot map[gitRequestKind]gitRequestStat

// since returns what happened between base and this reading.
func (s gitRequestSnapshot) since(base gitRequestSnapshot) gitRequestSnapshot {
	out := gitRequestSnapshot{}
	for _, kind := range gitRequestKinds {
		now, was := s[kind], base[kind]
		out[kind] = gitRequestStat{
			Count:     now.Count - was.Count,
			ReqBytes:  now.ReqBytes - was.ReqBytes,
			RespBytes: now.RespBytes - was.RespBytes,
		}
	}
	return out
}

// connections is the number this whole design optimizes: one request to the Git host is one
// connection, so the total across all four kinds is the cost of the operation.
func (s gitRequestSnapshot) connections() int64 {
	var total int64
	for _, kind := range gitRequestKinds {
		total += s[kind].Count
	}
	return total
}

// exactBytes renders the raw byte counts. It is for a human reading test output, never for the
// golden file — see sizeClass.
func (s gitRequestSnapshot) exactBytes() string {
	parts := make([]string, 0, len(gitRequestKinds))
	for _, kind := range gitRequestKinds {
		row := s[kind]
		if row.Count == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s x%d req=%dB resp=%dB", kind, row.Count, row.ReqBytes, row.RespBytes))
	}
	if len(parts) == 0 {
		return "(silent)"
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// sizeClass buckets a byte count for the golden file.
//
// The exact numbers are not reproducible and must not be asserted on: a packfile is zlib-compressed
// content whose commit objects embed timestamps and parent hashes, so the same logical operation
// encodes to a slightly different length on every run. The question the bytes exist to answer is
// categorical anyway — did this conversation transfer a tree, or only a handshake — and a class
// answers it without pinning a number that would flake.
//
// The boundaries are deliberately far from the measured values, so a few bytes of compression
// drift cannot move a row between classes.
func sizeClass(bytes int64) string {
	switch {
	case bytes == 0:
		return "-"
	case bytes < 1<<10:
		return "<1K"
	case bytes < 1<<12:
		return "<4K"
	case bytes < 1<<14:
		return "<16K"
	case bytes < 1<<16:
		return "<64K"
	default:
		return ">=64K"
	}
}

// countingReadCloser tallies the bytes a handler reads from a request body, without buffering it.
type countingReadCloser struct {
	inner io.ReadCloser
	n     int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReadCloser) Close() error { return c.inner.Close() }

// countingResponseWriter tallies the body bytes written back to the client.
//
// It forwards Flush because git's http-backend streams a packfile and the CGI host flushes as it
// goes; swallowing that would change the protocol's timing, not just the measurement.
type countingResponseWriter struct {
	http.ResponseWriter

	n int64
}

func (c *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

func (c *countingResponseWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
