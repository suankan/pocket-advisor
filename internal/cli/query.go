package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/client/llm"
	"github.com/suankan/pocket-advisor/internal/client/reranking"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/retrieval"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
	"github.com/suankan/pocket-advisor/internal/telemetry"
)

// runQuery is a thin adapter over internal/retrieval — it parses flags,
// renders a result, and holds no retrieval logic of its own. An MCP tool or
// HTTP handler is a sibling adapter over the same Query call
// (retrieval-design.md §7).
func runQuery(o *Options, cfg *config.Config, logs *telemetry.Logs) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	log := logs.Logger(telemetry.RoleApp)

	// The read path needs Postgres and the model endpoints, and nothing else:
	// no RustFS, no NATS, no worker pools.
	dsn, err := cfg.WorkspacePostgresDSN(o.WorkspaceID)
	if err != nil {
		return err
	}
	db, err := postgres.Connect(ctx, dsn, cfg.Postgres.MaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	svc := retrieval.New(db,
		embedding.New(cfg.Embedding),
		reranking.New(cfg.Reranking),
		llm.New(cfg.LLM),
		cfg.Query, o.WorkspaceID, log)

	if err := svc.AssertScope(ctx); err != nil {
		return err
	}

	req := retrieval.Request{Question: o.Query, TopK: o.TopK}
	if o.NoRerank {
		off := false
		req.Rerank = &off
	}
	if o.NoDecompose {
		off := false
		req.Decompose = &off
	}

	started := time.Now()
	res, err := svc.Query(ctx, req)
	if err != nil {
		return err
	}

	if o.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	renderResult(res, time.Since(started))
	return nil
}

// renderResult prints what a reader of an evidence corpus needs: what was
// actually searched, anything that silently reduced quality, and a citation on
// every result. A result that cannot be traced back to a file is not a result.
func renderResult(res *retrieval.Result, elapsed time.Duration) {
	searched := res.Question
	if len(res.SubQueries) > 1 {
		searched = strings.Join(res.SubQueries, "  ·  ")
	}
	note := "not decomposed"
	if len(res.SubQueries) > 1 {
		note = fmt.Sprintf("%d sub-queries", len(res.SubQueries))
	}

	fmt.Printf("searched as:  %s   [%s]\n", searched, note)
	if len(res.Warnings) > 0 {
		fmt.Printf("warnings:     %s\n", strings.Join(res.Warnings, ", "))
	} else {
		fmt.Printf("warnings:     none\n")
	}
	fmt.Printf("budget:       %d / %d chars · %.1fs\n\n",
		res.Budget.CharsUsed, res.Budget.CharsAllowed, elapsed.Seconds())

	if len(res.Packets) == 0 {
		fmt.Println("No sources in this workspace answer that question.")
		return
	}

	for i, p := range res.Packets {
		fmt.Printf("%d. %s\n", i+1, p.Title)
		meta := p.DocType
		if p.Date != nil {
			meta += " · " + p.Date.Format("2006-01-02")
		}
		if p.From != "" {
			meta += " · " + p.From
		}
		fmt.Printf("   %s\n", meta)
		fmt.Printf("   score %+.3f · %s · chars %d-%d\n",
			p.Match.Score, p.Match.Legs, p.Match.StartChar, p.Match.EndChar)
		if p.Match.SubQuery != "" {
			fmt.Printf("   via: %s\n", p.Match.SubQuery)
		}
		fmt.Printf("   cite: %s\n", p.RawURI)
		fmt.Printf("   \"%s\"\n", p.Match.Snippet)
		if n := len(p.Related); n > 0 {
			fmt.Printf("   related: %s\n", summariseRelated(p.Related))
		}
		fmt.Println()
	}
}

func summariseRelated(rel []retrieval.Related) string {
	counts := map[retrieval.Relation]int{}
	withheld := 0
	for _, r := range rel {
		counts[r.Relation]++
		if r.Text == "" {
			withheld++
		}
	}
	var parts []string
	for _, k := range []retrieval.Relation{
		retrieval.RelationParent, retrieval.RelationChild, retrieval.RelationThreadPeer,
	} {
		if n := counts[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	s := strings.Join(parts, ", ")
	if withheld > 0 {
		// Omitted text is not omitted provenance: the doc_id and URI are still
		// on the packet so a reader can pull it manually.
		s += fmt.Sprintf(" (%d over budget, citations kept)", withheld)
	}
	return s
}
