package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// settledOrder is the order outcomes are listed in; PENDING and PROCESSING
// documents are ones the run ended before.
var settledOrder = []struct {
	status domain.Status
	label  string
}{
	{domain.StatusCompleted, "completed"},
	{domain.StatusSkipped, "skipped"},
	{domain.StatusFailed, "failed"},
	{domain.StatusProcessing, "unfinished"},
	{domain.StatusPending, "unfinished"},
}

// printRunSummary writes what an ingestion run did and what remains undone,
// naming the documents only the owner can resolve.
func printRunSummary(w io.Writer, workspaceID string, elapsed time.Duration, s postgres.RunSummary) {
	fmt.Fprintf(w, "\ningest summary: workspace %s, %s\n", workspaceID, elapsed.Round(time.Second))
	fmt.Fprintf(w, "  sources    %d %s observed, %d new\n", s.Files, plural(s.Files, "file", "files"), s.NewOriginals)

	total := 0
	for _, tallies := range s.Settled {
		total += sum(tallies)
	}
	if total == 0 {
		fmt.Fprintf(w, "  this run   nothing new to process\n")
	} else {
		fmt.Fprintf(w, "  this run   %d %s\n", total, plural(total, "document", "documents"))
		for _, o := range settledOrder {
			tallies := s.Settled[o.status]
			if len(tallies) == 0 {
				continue
			}
			line := fmt.Sprintf("%15d %s", sum(tallies), o.label)
			if reasons := listReasons(tallies); reasons != "" {
				line = fmt.Sprintf("%-26s %s", line, reasons)
			}
			fmt.Fprintln(w, line)
		}
	}

	fmt.Fprintf(w, "  not done   %d unfinished, %d waiting for a source\n", s.Unfinished, s.Waiting)
	if failed := sum(s.FailedThisRun) + sum(s.FailedEarlier); failed > 0 {
		var parts []string
		if n := sum(s.FailedThisRun); n > 0 {
			parts = append(parts, fmt.Sprintf("%d this run (%s)", n, listReasons(s.FailedThisRun)))
		}
		if n := sum(s.FailedEarlier); n > 0 {
			parts = append(parts, fmt.Sprintf("%d earlier (%s)", n, listReasons(s.FailedEarlier)))
		}
		fmt.Fprintf(w, "             %d failed: %s\n", failed, strings.Join(parts, ", "))
	}
	if s.Locked > 0 {
		fmt.Fprintf(w, "             %d password-protected, %d of them found this run\n", s.Locked, s.LockedThisRun)
	}

	printUnsupported(w, s.Unsupported)

	if len(s.Attention) == 0 {
		return
	}
	fmt.Fprintf(w, "  needs you\n")
	for _, a := range s.Attention {
		when := ""
		if a.ThisRun {
			when = "  [this run]"
		}
		fmt.Fprintf(w, "    %-8s %-18s %s%s\n", strings.ToLower(string(a.Status)), a.Reason, a.Location, when)
	}
	if more := s.AttentionTotal - len(s.Attention); more > 0 {
		fmt.Fprintf(w, "    ... and %d more\n", more)
	}
	if sum(s.FailedThisRun)+sum(s.FailedEarlier) > 0 {
		fmt.Fprintf(w, "  --doctor lists failures by reason; --recover retries those that can be.\n")
	}
}

// maxTypes bounds how many unsupported types a run summary lists.
const maxTypes = 15

// printUnsupported lists what the store holds that no handler reads, by type
// and largest first, with one example of each: the formats support could be
// added for, or ignore rules written for.
func printUnsupported(w io.Writer, groups []postgres.TypeGroup) {
	if len(groups) == 0 {
		return
	}
	total := 0
	for _, g := range groups {
		total += g.Count
	}
	fmt.Fprintf(w, "  unsupported %d %s skipped, by type, across all runs\n", total, plural(total, "file", "files"))
	for i, g := range groups {
		if i == maxTypes {
			fmt.Fprintf(w, "    ... and %d more %s\n", len(groups)-maxTypes, plural(len(groups)-maxTypes, "type", "types"))
			break
		}
		line := fmt.Sprintf("%8d  %s", g.Count, typeLabel(g.MimeType, g.Extension))
		if len(g.Locations) > 0 {
			line = fmt.Sprintf("%-52s e.g. %s", line, g.Locations[0])
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintf(w, "  --doctor --json lists every unsupported file; ignore rules keep out what you do not want.\n")
}

// typeLabel names a group of unsupported files: its detected type and the
// extension its files carry.
func typeLabel(mimeType, ext string) string {
	if ext == "" {
		ext = "no extension"
	}
	return mimeType + " (" + ext + ")"
}

func sum(tallies []postgres.Tally) int {
	n := 0
	for _, t := range tallies {
		n += t.Count
	}
	return n
}

// listReasons renders tallies as "REASON n, REASON n", leaving out the empty
// reason a completed document carries.
func listReasons(tallies []postgres.Tally) string {
	var parts []string
	for _, t := range tallies {
		if t.Reason != "" {
			parts = append(parts, fmt.Sprintf("%s %d", t.Reason, t.Count))
		}
	}
	return strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
