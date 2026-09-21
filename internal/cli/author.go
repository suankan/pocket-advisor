package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

// runCorpusSearch finds documents whose canonical text contains a literal
// span, going straight to the corpus rather than through retrieval.
//
// This is the authoring counterpart to --query, and the difference is the
// whole point. --query answers "what would the ranker return"; this answers
// "what does the corpus actually contain". A golden case needs the second,
// because a case whose expected answer came from the ranker agrees with
// current behaviour by construction and can never fail for the one reason
// that matters most: retrieval missing a document entirely.
func runCorpusSearch(o *Options, cfg *config.Config, logs *telemetry.Logs, span string) error {
	span = strings.TrimSpace(span)
	if span == "" {
		return fmt.Errorf("--corpus-search needs the text to look for")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn, err := cfg.WorkspacePostgresDSN(o.WorkspaceID)
	if err != nil {
		return err
	}
	db, err := postgres.Connect(ctx, dsn, 4)
	if err != nil {
		return err
	}
	defer db.Close()

	started := time.Now()
	hits, err := postgres.NewDocumentRepo(db).FindSpans(ctx,
		[]postgres.SpanQuery{{Span: span}}, 240)
	if err != nil {
		return err
	}

	if o.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(hits)
	}

	// Only real conversations count. An empty thread id is "not part of a
	// conversation", and folding those together would report four unrelated
	// PDFs as one thread, which is the opposite of what the number is for.
	threads := map[string]struct{}{}
	standalone := 0
	for _, h := range hits {
		if h.ThreadID == "" {
			standalone++
			continue
		}
		threads[h.ThreadID] = struct{}{}
	}
	where := ""
	switch {
	case len(threads) > 0 && standalone > 0:
		where = fmt.Sprintf(", across %d conversation(s) plus %d standalone document(s)", len(threads), standalone)
	case len(threads) > 0:
		where = fmt.Sprintf(", across %d conversation(s)", len(threads))
	}
	fmt.Printf("%d document(s) contain that text%s, scanned in %s\n\n",
		len(hits), where, time.Since(started).Round(time.Second))

	for _, h := range hits {
		date := "no date"
		if h.Date != nil {
			date = h.Date.Format("2006-01-02")
		}
		where := h.SourcePath
		if where == "" {
			where = "(not its own file)"
		}
		fmt.Printf("%s  %-7s  %s\n", h.RawSHA256[:12], h.DocType, date)
		fmt.Printf("    %s\n", truncate(h.Title, 96))
		fmt.Printf("    %s\n", truncate(where, 96))
		fmt.Printf("    …%s…\n\n", truncate(collapse(h.Context), 300))
	}

	if len(hits) == 0 {
		fmt.Println("Nothing matched. Whitespace runs are collapsed before comparing, so")
		fmt.Println("layout spacing is not the cause; try a shorter or differently worded span.")
	}
	return nil
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
