// SPDX-License-Identifier: Apache-2.0

// ts prefixes each stdin line with the elapsed seconds since startup
// and the current wallclock, then writes it to stdout. Useful when
// decorating long-running e2e logs.
package main

import (
	"bufio"
	"fmt"
	"os"
	"time"
)

const maxLineBytes = 1 << 20

func main() {
	start := time.Now()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, maxLineBytes), maxLineBytes)
	for scanner.Scan() {
		now := time.Now()
		fmt.Printf("%7.3fs | %s | %s\n", now.Sub(start).Seconds(), now.Format("15:04:05"), scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "ts:", err)
		os.Exit(1)
	}
}
