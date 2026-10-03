// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport"
)

// Every call that reaches a Git server is bounded, because the branch worker makes them
// synchronously on its event loop: a server that accepts a connection and then stalls would
// otherwise hold every target on the branch, its saves, withdrawals, timers and later writes,
// for as long as the TCP connection lives. Shutdown waits on that loop too.
//
// Measured against go-git v6.0.0-alpha.5 (docs/design/gittarget-branch-worker-log.md, step 6):
//
//   - HTTP honors the context in every phase (advertisement, upload-pack, receive-pack) when the
//     context-taking API is used. Remote.List and Repository.Fetch take none, and over the default
//     HTTP client they hung indefinitely.
//   - SSH uses the context only to dial. The SSH handshake and every read of the git protocol
//     ignore it, so a context alone hangs on a server that never sends its banner, and on one
//     that completes the handshake and then says nothing.
//
// So a bounded call needs both: the context-taking API, and a connection that closes when the
// context ends (boundToContext). Closing the socket fails the blocked read on the calling
// goroutine; nothing runs the operation on another goroutine, so nothing outlives the call with
// the checkout in hand.

//nolint:gochecknoglobals // vars, not consts, so a test can shorten them.
var (
	// gitCallTimeout bounds one round trip to a Git server: an advertisement, a fetch, or one push
	// session. A depth-1 fetch of a large repository over a slow link is the long case it must cover.
	gitCallTimeout = 2 * time.Minute
	// gitPublishTimeout bounds one whole push cycle, contention retries and their replays included,
	// so three slow attempts cannot each start a fresh budget.
	gitPublishTimeout = 5 * time.Minute
)

// boundGitCall derives the context for one call to a Git server. A tighter deadline the caller
// already carries (a push cycle's) wins.
func boundGitCall(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, gitCallTimeout)
}

// boundToContext returns auth with a dialer whose connections close when ctx ends. It matters for
// SSH (and git://), whose transports read without consulting the context; HTTP ignores the dialer
// option and honors the context itself.
func boundToContext(ctx context.Context, auth []gitclient.Option) []gitclient.Option {
	return append(slices.Clip(auth), gitclient.WithDialer(contextBoundDialer(ctx)))
}

// contextBoundDialer dials with the transport's own dial context, then ties the connection to the
// operation's context. The dial context is not the one to tie it to: go-git cancels it as soon as
// the dial returns.
func contextBoundDialer(operation context.Context) transport.DialContextFunc {
	return func(dialCtx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(dialCtx, network, address)
		if err != nil {
			return nil, err
		}
		stop := context.AfterFunc(operation, func() { _ = conn.Close() })
		return &contextBoundConn{Conn: conn, stop: stop}, nil
	}
}

// contextBoundConn is a connection closed by its operation's context. Closing it normally
// releases that tie.
type contextBoundConn struct {
	net.Conn

	stop func() bool
}

func (c *contextBoundConn) Close() error {
	c.stop()
	return c.Conn.Close()
}

// boundedCallError names the bound when it is what ended a call. A connection closed under an SSH
// session surfaces as EOF or a closed-connection error, which says nothing about why.
func boundedCallError(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil || errors.Is(err, ctx.Err()) {
		return err
	}
	return fmt.Errorf("%w: %w", ctx.Err(), err)
}
