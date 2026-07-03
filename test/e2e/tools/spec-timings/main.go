// SPDX-License-Identifier: Apache-2.0

// spec-timings reads a Ginkgo JSON report and prints a duration-sorted
// table of specs with each spec's share of the total wallclock.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

const (
	exitUsage     = 2
	percentScalar = 100.0
	usageInstruct = "usage: spec-timings <ginkgo-report.json>"
)

func main() {
	os.Exit(mainRun(os.Args[1:], os.Stdout, os.Stderr))
}

func mainRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("spec-timings", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, usageInstruct)
		return exitUsage
	}
	if err := run(fs.Arg(0), stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

type specRow struct {
	name     string
	duration time.Duration
}

func run(path string, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	rows, total, err := parseReport(f)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	writeTable(out, rows, total)
	return nil
}

func parseReport(r io.Reader) ([]specRow, time.Duration, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, 0, err
	}
	var reports []types.Report
	if err := json.Unmarshal(data, &reports); err != nil {
		return nil, 0, err
	}

	var rows []specRow
	var total time.Duration
	for _, rep := range reports {
		for _, sr := range rep.SpecReports {
			if sr.LeafNodeType != types.NodeTypeIt {
				continue
			}
			if sr.State == types.SpecStateSkipped || sr.State == types.SpecStatePending {
				continue
			}
			parts := append([]string{}, sr.ContainerHierarchyTexts...)
			parts = append(parts, sr.LeafNodeText)
			rows = append(rows, specRow{
				name:     strings.Join(dedupeConsecutive(parts), " "),
				duration: sr.RunTime,
			})
			total += sr.RunTime
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].duration > rows[j].duration })
	return rows, total, nil
}

func writeTable(out io.Writer, rows []specRow, total time.Duration) {
	fmt.Fprintln(out, "  Duration     %  Spec")
	for _, row := range rows {
		pct := 0.0
		if total > 0 {
			pct = percentScalar * float64(row.duration) / float64(total)
		}
		fmt.Fprintf(out, "%8.1f s  %4.1f  %s\n", row.duration.Seconds(), pct, row.name)
	}
	fmt.Fprintf(out, "%8.1f s  total\n", total.Seconds())
}

// dedupeConsecutive collapses adjacent equal entries — nested Describes
// with the same text otherwise print as "Manager Manager should ...".
func dedupeConsecutive(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if len(out) > 0 && out[len(out)-1] == s {
			continue
		}
		out = append(out, s)
	}
	return out
}
