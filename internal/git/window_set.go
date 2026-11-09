// SPDX-License-Identifier: Apache-2.0

package git

import (
	"slices"
	"time"
)

// commitWindows is the set of commit windows a branch worker holds open, and what they are charged.
// Every path that reads a window says which one it means, or all of them.
//
// Several windows may be open side by side, one per author on the GitTarget, while their footprints
// are proven not to overlap (footprint.go, docs/design/concurrent-author-windows.md). A write that
// cannot be admitted beside a window closes that window first (blocking). Windows that one trigger
// closes close oldest first, by the sequence number each took when it opened, never by map order or
// by time.
type commitWindows struct {
	// windows are the open windows, in the order they opened.
	windows []*openWindow
	// windowBytes is what the open windows are charged: their events, and the message of the request
	// attached to each. It is the sum of every open window's charge.
	windowBytes int64
	// windowSeq numbers the windows in the order they opened. It resets with the worker.
	windowSeq uint64
}

// all returns the open windows, oldest first. The slice is the caller's: closing a window while
// ranging over it is safe.
func (s *commitWindows) all() []*openWindow {
	return slices.Clone(s.windows)
}

// anyOpen reports whether a window is open.
func (s *commitWindows) anyOpen() bool {
	return len(s.windows) > 0
}

// count is how many windows are open.
func (s *commitWindows) count() int {
	return len(s.windows)
}

// bytes is what the open windows are charged.
func (s *commitWindows) bytes() int64 {
	return s.windowBytes
}

// isOpen reports whether w is still one of the open windows.
func (s *commitWindows) isOpen(w *openWindow) bool {
	return w != nil && slices.Contains(s.windows, w)
}

// open opens a window for the write e at now, numbered after every window opened before it. The
// caller has closed every window e cannot be admitted beside (blocking).
func (s *commitWindows) open(e Event, writer eventContentWriter, now time.Time) *openWindow {
	s.windowSeq++
	w := newOpenWindow(e, writer)
	w.seq = s.windowSeq
	w.openedAt = now
	s.windows = append(s.windows, w)
	return w
}

// forWrite returns the open window the write e joins, or nil when it must open one.
func (s *commitWindows) forWrite(e Event) *openWindow {
	for _, w := range s.windows {
		if w.canAppend(e) {
			return w
		}
	}
	return nil
}

// others returns the open windows other than w, oldest first.
func (s *commitWindows) others(w *openWindow) []*openWindow {
	var out []*openWindow
	for _, other := range s.windows {
		if other != w {
			out = append(out, other)
		}
	}
	return out
}

// forRequest returns the open window the request may attach to, or nil.
func (s *commitWindows) forRequest(p *pendingCommitRequest) *openWindow {
	for _, w := range s.windows {
		if p.matchesWindow(w) {
			return w
		}
	}
	return nil
}

// foreignTo returns the oldest open window the request may not attach to, while none it may attach
// to is open, or nil. A window of the request's own author stands in no other author's way.
func (s *commitWindows) foreignTo(p *pendingCommitRequest) *openWindow {
	if s.forRequest(p) != nil {
		return nil
	}
	for _, w := range s.windows {
		if !p.matchesWindow(w) {
			return w
		}
	}
	return nil
}

// carrying returns the open window the request is attached to, or nil.
func (s *commitWindows) carrying(id commitRequestID) *openWindow {
	for _, w := range s.windows {
		if w.pendingCR != nil && *w.pendingCR == id {
			return w
		}
	}
	return nil
}

// charge adds n bytes, which may be negative, to w's charge and to the set's.
func (s *commitWindows) charge(w *openWindow, n int64) {
	w.bytes += n
	s.windowBytes += n
}

// remove forgets w, and releases what it was charged.
func (s *commitWindows) remove(w *openWindow) {
	i := slices.Index(s.windows, w)
	if i < 0 {
		return
	}
	s.windows = slices.Delete(s.windows, i, i+1)
	s.windowBytes -= w.bytes
}
