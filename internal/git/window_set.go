// SPDX-License-Identifier: Apache-2.0

package git

import "time"

// commitWindows is the set of commit windows a branch worker holds open, and what they are charged.
// Every path that reads a window says which one it means, or all of them.
//
// It allows one window. A write that cannot join the open window closes it first, with close reason
// identity_change, before it opens its own, so commits land in the order their windows opened.
// docs/design/concurrent-author-windows.md lets several stay open side by side; windows that one
// trigger closes then close oldest first, by the sequence number each took when it opened, never by
// map order or by time.
//
// The loop embeds the set, so loop.openWindow still names the one window it holds.
type commitWindows struct {
	// openWindow is the open window, or nil.
	openWindow *openWindow
	// windowBytes is what the open windows are charged: their events, and the message of the request
	// attached to each. It is the sum of every open window's charge.
	windowBytes int64
	// windowSeq numbers the windows in the order they opened.
	windowSeq uint64
}

// all returns the open windows, oldest first.
func (s *commitWindows) all() []*openWindow {
	if s.openWindow == nil {
		return nil
	}
	return []*openWindow{s.openWindow}
}

// anyOpen reports whether a window is open.
func (s *commitWindows) anyOpen() bool {
	return s.openWindow != nil
}

// isOpen reports whether w is still one of the open windows.
func (s *commitWindows) isOpen(w *openWindow) bool {
	return w != nil && s.openWindow == w
}

// open opens a window for the write e at now, numbered after every window opened before it. The
// caller has closed every window e cannot be admitted beside (blocking), so the set is empty here.
func (s *commitWindows) open(e Event, writer eventContentWriter, now time.Time) *openWindow {
	s.windowSeq++
	w := newOpenWindow(e, writer)
	w.seq = s.windowSeq
	w.openedAt = now
	s.openWindow = w
	return w
}

// forWrite returns the open window the write e joins, or nil when it must open one.
func (s *commitWindows) forWrite(e Event) *openWindow {
	for _, w := range s.all() {
		if w.canAppend(e) {
			return w
		}
	}
	return nil
}

// blocking returns the open windows the write e cannot be admitted beside, oldest first: every
// window it cannot join.
func (s *commitWindows) blocking(e Event) []*openWindow {
	var out []*openWindow
	for _, w := range s.all() {
		if !w.canAppend(e) {
			out = append(out, w)
		}
	}
	return out
}

// forRequest returns the open window the request may attach to, or nil.
func (s *commitWindows) forRequest(p *pendingCommitRequest) *openWindow {
	for _, w := range s.all() {
		if p.matchesWindow(w) {
			return w
		}
	}
	return nil
}

// foreignTo returns the oldest open window the request may not attach to, or nil.
func (s *commitWindows) foreignTo(p *pendingCommitRequest) *openWindow {
	for _, w := range s.all() {
		if !p.matchesWindow(w) {
			return w
		}
	}
	return nil
}

// carrying returns the open window the request is attached to, or nil.
func (s *commitWindows) carrying(id commitRequestID) *openWindow {
	for _, w := range s.all() {
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
	if !s.isOpen(w) {
		return
	}
	s.openWindow = nil
	s.windowBytes -= w.bytes
}
