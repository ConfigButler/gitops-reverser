// SPDX-License-Identifier: Apache-2.0

package giteaclient

import (
	"context"
	"fmt"
	"net/http"
)

// listPaginated walks every page of a Gitea list endpoint and returns the concatenated items.
//
// Two rules, both of which a hand-rolled loop tends to get wrong:
//
//   - ONLY an empty page terminates. A short page is NOT necessarily the last page: Gitea's
//     ToCorrectPageSize clamps the requested limit down to MAX_RESPONSE_ITEMS, which operators
//     configure, so against a server whose cap is below pageSize EVERY page is short. Stopping
//     on a short page would stop after page 1 and silently return a truncated list — the exact
//     failure mode pagination is here to prevent. The cost is one extra round trip.
//   - The loop is capped. A server that ignores the `page` parameter serves a full first page
//     forever; without a cap this spins until the caller's context deadline and reports
//     `context deadline exceeded`, naming neither the endpoint nor the cause.
//
// page is 1-indexed (Gitea routers/api/v1/utils/page.go, models/db/list.go).
func listPaginated[T any](
	ctx context.Context,
	c *Client,
	base string,
	pageSize, maxPages int,
) ([]T, error) {
	var all []T
	for page := 1; page <= maxPages; page++ {
		var items []T
		path := fmt.Sprintf("%s?page=%d&limit=%d", base, page, pageSize)
		code, raw, err := c.Do(ctx, http.MethodGet, path, nil, &items)
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, unexpectedStatus(http.MethodGet, path, code, raw)
		}
		if len(items) == 0 {
			return all, nil
		}
		all = append(all, items...)
	}
	return nil, fmt.Errorf(
		"listing %s did not terminate within %d pages of %d: the server appears to ignore the "+
			"page parameter", base, maxPages, pageSize)
}
