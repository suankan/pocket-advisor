package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/suankan/pocket-advisor/internal/retrieval"
)

// QueryTool exposes retrieval.Query.
//
// The workspace is fixed at startup rather than passed per call. Each
// workspace is its own database with its own role, and the service asserts on
// connect that it holds exactly one — a per-call workspace would need a pool
// per workspace and would undermine that assertion
// (workspace-isolation.md §2.1).
type QueryTool struct {
	Service   *retrieval.Service
	Workspace string
}

// toolName is deliberately specific. An agent picks tools by name and
// description, and "search" or "query" would invite use for things this cannot
// do — it searches one ingested corpus, not the web or the filesystem.
const toolName = "search_case_documents"

// Describe returns the tool definition sent in tools/list.
//
// The description tells the agent two things it cannot infer: that results are
// source passages rather than an answer, and that every claim it makes from
// them should carry the citation shipped alongside. Retrieval's whole value is
// traceability to bytes; an agent that paraphrases without citing discards it.
func (t *QueryTool) Describe() map[string]any {
	return map[string]any{
		"name": toolName,
		"description": "Search the ingested case corpus for '" + t.Workspace + "' " +
			"(emails, letters, bank statements, PDFs) and return the source passages " +
			"that match, with citations.\n\n" +
			"Returns evidence, not an answer: read the passages and write the answer " +
			"yourself, citing each claim as [n]. Quote or attribute only what the " +
			"passages actually say — attributing a statement to the wrong person is " +
			"the failure that matters most for this corpus.\n\n" +
			"If it reports no matching sources, say so rather than answering from " +
			"general knowledge. The corpus is bilingual (English and Russian); ask in " +
			"either language.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{
					"type": "string",
					"description": "A natural-language question. Multi-topic questions are " +
						"split automatically, so ask the whole thing rather than " +
						"pre-splitting it.",
				},
				"top_k": map[string]any{
					"type":        "integer",
					"description": "Maximum sources to return (default 15).",
					"minimum":     1,
					"maximum":     50,
				},
			},
			"required": []string{"question"},
		},
	}
}

type callParams struct {
	Name      string `json:"name"`
	Arguments struct {
		Question string `json:"question"`
		TopK     int    `json:"top_k"`
	} `json:"arguments"`
}

// Call runs a search and renders it for an agent.
func (t *QueryTool) Call(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var p callParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("invalid tool arguments: %w", err)
	}
	if p.Name != "" && p.Name != toolName {
		return nil, fmt.Errorf("unknown tool %q", p.Name)
	}
	question := strings.TrimSpace(p.Arguments.Question)
	if question == "" {
		return nil, fmt.Errorf("question is required")
	}

	res, err := t.Service.Query(ctx, retrieval.Request{
		Question: question,
		TopK:     p.Arguments.TopK,
	})
	if err != nil {
		return nil, err
	}
	return textResult(render(res)), nil
}

// render lays out packets for a model to read and cite.
//
// Full document text is included, not snippets: the agent is the generation
// stage and cannot answer from previews. The shared budget already bounds this
// (§5.3), and what it dropped is stated rather than silently missing.
func render(res *retrieval.Result) string {
	var b strings.Builder

	if len(res.SubQueries) > 1 {
		fmt.Fprintf(&b, "Searched as %d separate queries: %s\n",
			len(res.SubQueries), strings.Join(res.SubQueries, " | "))
	} else {
		fmt.Fprintf(&b, "Searched for: %s\n", res.Question)
	}
	if len(res.Warnings) > 0 {
		fmt.Fprintf(&b, "Search notes: %s\n", strings.Join(res.Warnings, ", "))
	}

	if len(res.Packets) == 0 {
		b.WriteString("\nNo sources in this corpus match that question. " +
			"Nothing was found to answer from — say so rather than answering " +
			"from general knowledge.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "\n%d source(s), most relevant first. Cite as [n].\n", len(res.Packets))

	for i, p := range res.Packets {
		fmt.Fprintf(&b, "\n─────────────────────────────────────────\n[%d] %s\n", i+1, p.Title)

		meta := []string{p.DocType}
		if p.Date != nil {
			meta = append(meta, p.Date.Format("2006-01-02"))
		}
		if p.From != "" {
			meta = append(meta, "from "+p.From)
		}
		if p.To != "" {
			meta = append(meta, "to "+p.To)
		}
		fmt.Fprintf(&b, "%s\n", strings.Join(meta, " · "))
		fmt.Fprintf(&b, "cite: %s (doc %s, chars %d-%d, relevance %+.3f)\n",
			p.RawURI, shortID(p.DocID), p.Match.StartChar, p.Match.EndChar, p.Match.Score)

		// The matched passage is named explicitly so the agent can quote the
		// part that actually matched rather than anything in the document.
		fmt.Fprintf(&b, "\nmatched passage:\n%s\n", p.Match.Snippet)

		if p.Text != "" {
			fmt.Fprintf(&b, "\nfull document:\n%s\n", p.Text)
		} else {
			fmt.Fprintf(&b, "\nfull document: omitted, over the context budget — "+
				"fetch via the citation above if needed.\n")
		}

		if len(p.Related) > 0 {
			fmt.Fprintf(&b, "\nrelated (%s):\n", relationSummary(p.Related))
			for _, r := range p.Related {
				line := fmt.Sprintf("  · %s — %s", r.Relation, r.Title)
				if r.Date != nil {
					line += " (" + r.Date.Format("2006-01-02") + ")"
				}
				if r.From != "" {
					line += " from " + r.From
				}
				fmt.Fprintln(&b, line)
				if r.Text != "" {
					fmt.Fprintf(&b, "    %s\n", indent(r.Text))
				}
			}
		}
	}

	fmt.Fprintf(&b, "\n─────────────────────────────────────────\n"+
		"Context used: %d of %d characters.\n", res.Budget.CharsUsed, res.Budget.CharsAllowed)
	return b.String()
}

// relationSummary keeps deterministic relations distinguishable. A message
// that merely shares a thread is never described as a reply (§5.4).
func relationSummary(rel []retrieval.Related) string {
	counts := map[retrieval.Relation]int{}
	for _, r := range rel {
		counts[r.Relation]++
	}
	var parts []string
	for _, k := range []retrieval.Relation{
		retrieval.RelationParent, retrieval.RelationChild, retrieval.RelationThreadPeer,
	} {
		if n := counts[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	return strings.Join(parts, ", ")
}

func indent(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func textResult(text string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
}

func errorResult(err error) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{
			"type": "text",
			"text": "The corpus search failed: " + err.Error() +
				"\nDo not answer from general knowledge; report that the search was unavailable.",
		}},
		"isError": true,
	}
}
