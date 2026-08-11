package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/suankan/pocket-advisor/internal/client/llm"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// sampledDoc holds a document row sampled from the workspace for fixture
// generation. Only the fields needed by the prompt are retained.
type sampledDoc struct {
	DocID    string
	Filename string // source_filename, used as fixture_id
	Snippet  string // a window of normalized text, see SnippetChars
	ThreadID string
	Source   string // collection / source type
}

// Generator produces evaluation case sets from a real workspace.
type Generator struct {
	DB          *postgres.DB
	LLM         *llm.Client
	WorkspaceID string
	Log         *slog.Logger
}

// NewGenerator creates a generator from infrastructure components.
func NewGenerator(
	db *postgres.DB,
	l *llm.Client,
	workspaceID string,
	log *slog.Logger,
) *Generator {
	return &Generator{
		DB:          db,
		LLM:         l,
		WorkspaceID: workspaceID,
		Log:         log,
	}
}

// SampleSize controls how many documents the generator samples.
// Exported so tests can override it.
//
// Raised once verification landed: most proposals are now rejected, so the
// sample has to be larger to yield a usable suite. Measured on a real corpus,
// roughly one proposal in three survives grounding and discrimination checks.
var SampleSize = 60

// SnippetChars is how much of each document the generator shows the model.
//
// Deliberately large, and deliberately not taken from the start. Extracted
// PDFs open with letterhead: measured on a real corpus, the first 300
// characters of a payslip were "PAID BY <company> Pty Ltd" and nothing else,
// so a generator reading only the opening could ask about nothing but company
// names and registration numbers — facts every document in a series repeats.
// That is where identifier-heavy, unanswerable fixtures came from.
var SnippetChars = 1200

// Generate samples documents from the workspace, generates evaluation
// questions via the LLM, and writes the resulting CaseSet to outputPath.
func (g *Generator) Generate(ctx context.Context, outputPath string) error {
	// 1. Sample documents.
	docs, err := g.sampleDocuments(ctx)
	if err != nil {
		return fmt.Errorf("sample documents: %w", err)
	}
	if len(docs) == 0 {
		return fmt.Errorf("no completed documents in workspace %s", g.WorkspaceID)
	}
	g.Log.Info("sampled documents", "count", len(docs))

	// 2. Generate questions via LLM.
	cases, err := g.generateCases(ctx, docs)
	if err != nil {
		return fmt.Errorf("generate cases: %w", err)
	}
	if len(cases) == 0 {
		return fmt.Errorf("LLM produced no cases")
	}
	g.Log.Info("generated cases", "count", len(cases))

	// 3. Build and write the CaseSet.
	cs := CaseSet{
		Version: CaseSchemaVersion,
		SetID:   "workspace-" + g.WorkspaceID,
		Cases:   cases,
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cs); err != nil {
		return fmt.Errorf("write case set: %w", err)
	}
	g.Log.Info("wrote case set", "path", outputPath, "cases", len(cases))
	return nil
}

// sampleDocuments queries the workspace for a diverse sample of completed
// documents. It uses ORDER BY RANDOM() with a limit to spread across the
// corpus; the database's random sampling is sufficient for evaluation fixture
// generation.
//
// The 300-character floor excludes near-empty documents (a one-line reply,
// a quoted forward) that cannot be told apart from near-duplicate siblings
// by content alone — real workspaces can hold long threads of dozens of
// short, topically identical messages, and a source that thin cannot ground
// a fair fixture regardless of retrieval quality.
//
// Long documents are sampled from a quarter of the way in rather than from
// the top, to get past letterhead and into what the document actually says.
// Short ones are shown whole, since there is no header to skip.
func (g *Generator) sampleDocuments(ctx context.Context) ([]sampledDoc, error) {
	rows, err := g.DB.Pool.Query(ctx, `
SELECT doc_id::text, COALESCE(NULLIF(source_filename, ''), ''),
       CASE WHEN LENGTH(normalized_text) > $2 * 2
            THEN SUBSTRING(normalized_text FROM (LENGTH(normalized_text) / 4) FOR $2)
            ELSE LEFT(normalized_text, $2) END AS snippet,
       COALESCE(thread_id, ''), COALESCE(collection_id, '')
FROM documents
WHERE processing_status = 'COMPLETED'
  AND normalized_text IS NOT NULL
  AND LENGTH(normalized_text) > 300
ORDER BY RANDOM()
LIMIT $1`, SampleSize, SnippetChars)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []sampledDoc
	for rows.Next() {
		var d sampledDoc
		if err := rows.Scan(&d.DocID, &d.Filename, &d.Snippet, &d.ThreadID, &d.Source); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// generateCases sends document snippets to the LLM, verifies what comes back
// against the corpus, and returns only the cases that survive.
//
// Yield is reported rather than left silent. Thirty documents at two or three
// questions each should propose sixty to ninety; runs producing a fraction of
// that used to look identical to healthy ones, because failed batches were
// skipped with nothing surfaced to the operator.
func (g *Generator) generateCases(ctx context.Context, docs []sampledDoc) ([]Case, error) {
	var proposed []candidateCase
	var batches, failed int

	const batchSize = 5
	for i := 0; i < len(docs); i += batchSize {
		end := i + batchSize
		if end > len(docs) {
			end = len(docs)
		}
		batches++
		batchCases, err := g.generateBatch(ctx, docs[i:end])
		if err != nil {
			failed++
			g.Log.Warn("batch generation failed, skipping", "batch", i/batchSize, "error", err)
			continue
		}
		proposed = append(proposed, batchCases...)
	}

	kept, stats := g.verify(ctx, proposed)
	g.Log.Info("fixture generation yield",
		"batches", batches, "batches_failed", failed,
		"proposed", len(proposed), "kept", len(kept),
		"dropped_unverifiable", stats.unverifiable,
		"dropped_not_grounded", stats.notGrounded,
		"dropped_ambiguous", stats.ambiguous,
		"dropped_duplicate", stats.duplicate,
		"expanded_to_multi_source", stats.expanded)

	for i := range kept {
		kept[i].ID = fmt.Sprintf("gen-%04d", i+1)
	}
	return kept, nil
}

// dropStats counts why candidates were rejected, so a thin case set is
// explainable rather than mysterious.
type dropStats struct {
	unverifiable, notGrounded, ambiguous, duplicate, expanded int
}

// AmbiguityCap is how many documents may share an answer before a question is
// considered unanswerable rather than merely multi-source.
//
// Quoted replies and near-identical documents mean a correct answer often
// appears in a handful of places. Naming one of them arbitrarily is a coin
// flip, but the question itself is fine — so up to this many, every document
// holding the answer is recorded as acceptable. Beyond it the fact is
// boilerplate and identifies nothing.
var AmbiguityCap = 4

// verifiableAnswer normalises a claimed answer and reports whether it is
// substantial enough to check. A bare year or a single word occurs throughout
// a corpus, so finding it proves nothing about where the answer lives.
func verifiableAnswer(raw string) (string, bool) {
	a := strings.Join(strings.Fields(raw), " ")
	return a, len(a) >= 12
}

// verify keeps only questions the corpus can actually settle.
//
// Two checks, both against corpus statistics and neither against the retrieval
// pipeline. Filtering by "can retrieval find this?" would keep only questions
// the system already answers and turn the suite into a tautology, so the
// answer span is checked against stored text directly.
//
// Grounded: the span the model quoted must really appear in the document it
// named. That rejects a question about correspondence answered by a cost
// agreement — a real case this suite once graded.
//
// Discriminating: the span must not appear in more documents than the case
// names as sources. An employer's registration number printed on all
// twenty-nine payslips identifies none of them, and a fixture naming one
// arbitrarily is decided by luck rather than retrieval quality. The same suite
// once asked one such question twice and scored it both right and wrong.
func (g *Generator) verify(ctx context.Context, cands []candidateCase) ([]Case, dropStats) {
	var (
		kept  []Case
		stats dropStats
		seen  = make(map[string]struct{}, len(cands))
	)
	for _, c := range cands {
		answer, ok := verifiableAnswer(c.Answer)
		if !ok {
			stats.unverifiable++
			continue
		}
		qkey := strings.ToLower(strings.Join(strings.Fields(c.Question), " "))
		if _, dup := seen[qkey]; dup {
			stats.duplicate++
			continue
		}

		rows, err := g.DB.Pool.Query(ctx, `
            SELECT doc_id::text, COALESCE(NULLIF(source_filename, ''), '')
            FROM documents
            WHERE normalized_text IS NOT NULL
              AND position($1 in regexp_replace(normalized_text, '\s+', ' ', 'g')) > 0
            LIMIT $2`, answer, AmbiguityCap+1)
		if err != nil {
			g.Log.Warn("verify failed, dropping candidate", "error", err)
			stats.unverifiable++
			continue
		}
		holders := map[string]string{}
		for rows.Next() {
			var id, filename string
			if err := rows.Scan(&id, &filename); err != nil {
				break
			}
			holders[id] = fixtureIDFromFilename(filename)
		}
		rows.Close()

		named := false
		for _, id := range c.DocIDs {
			if _, ok := holders[id]; ok {
				named = true
				break
			}
		}
		if !named {
			stats.notGrounded++
			continue
		}
		if len(holders) > AmbiguityCap {
			stats.ambiguous++
			continue
		}

		// Every document holding the answer is an acceptable source. Naming
		// only the one the model happened to be shown would mark a correct
		// retrieval wrong whenever a sibling surfaced first.
		if len(holders) > len(c.ExpectedSources) {
			grade := c.ExpectedSources[0].Grade
			c.ExpectedSources = c.ExpectedSources[:0]
			for _, fid := range holders {
				c.ExpectedSources = append(c.ExpectedSources,
					ExpectedSource{FixtureID: fid, Grade: grade})
			}
			sort.Slice(c.ExpectedSources, func(i, j int) bool {
				return c.ExpectedSources[i].FixtureID < c.ExpectedSources[j].FixtureID
			})
			stats.expanded++
		}

		seen[qkey] = struct{}{}
		kept = append(kept, c.Case)
	}
	return kept, stats
}

// llmQuestion is the JSON structure the LLM is expected to return per document.
type llmQuestion struct {
	Question  string   `json:"question"`
	Category  string   `json:"category"`
	DocLabels []string `json:"doc_labels"` // opaque per-batch labels, e.g. "DOC0"
	Answer    string   `json:"answer"`     // verbatim span the question is answered by
}

// candidateCase is a question the model proposed, still carrying the answer it
// claimed, so the claim can be checked against the corpus before the case is
// written. Nothing reaches a case set until that check passes.
type candidateCase struct {
	Case
	Answer string
	DocIDs []string // documents behind ExpectedSources, in the same order
}

// llmBatchResponse is the top-level structure from the LLM for a batch.
type llmBatchResponse struct {
	Questions []llmQuestion `json:"questions"`
}

// generateBatch sends a batch of documents to the LLM and returns cases.
//
// The LLM sees only an opaque per-batch label for each document, never its
// fixture_id — a fixture_id is the source filename stripped of extension,
// and for email sources that filename bakes in the sender, date, and
// subject, none of which appear in the indexed normalized_text. Handing the
// LLM that string invites it to write "exact-identifier" questions about
// metadata the retrieval system has no access to, which no amount of
// retrieval quality could ever satisfy. Labels are mapped back to real
// fixture_ids after the LLM responds, and any label it invents that wasn't
// actually offered in this batch is dropped rather than trusted.
func (g *Generator) generateBatch(ctx context.Context, docs []sampledDoc) ([]candidateCase, error) {
	labelToFixtureID := make(map[string]string, len(docs))
	labelToDocID := make(map[string]string, len(docs))
	var docEntries strings.Builder
	for i, d := range docs {
		label := fmt.Sprintf("DOC%d", i)
		labelToFixtureID[label] = fixtureIDFromFilename(d.Filename)
		labelToDocID[label] = d.DocID
		hasThread := "no"
		if d.ThreadID != "" {
			hasThread = "yes"
		}
		docEntries.WriteString(fmt.Sprintf(
			"[%s] source=%q has_thread=%s snippet=%s\n",
			label, d.Source, hasThread, truncate(d.Snippet, SnippetChars)))
	}

	prompt := fmt.Sprintf(`You are generating evaluation questions for a retrieval system that
searches only the document body text shown below. It has no access to
filenames, senders, recipients, dates, subject lines, or thread identifiers
— only the snippet text itself.

RULES:
- Generate 2-3 questions per document.
- Every question must be answerable using only facts, names, numbers, or
  statements that literally appear in the snippet text shown for that
  document. Never ask about a sender, recipient, date, filename, subject
  line, or thread identifier — unless that exact information is itself
  quoted inside the snippet text (e.g. a screenshot of an email header).
- Write each question the way a real user would ask it. Never mention a
  document's own [DOCn] label inside the question text — that label is
  only for the doc_labels field below, not for the question itself.
- Mix categories across the batch:
  * "exact-identifier": a specific fact, name, number, or detail stated in the snippet text itself
  * "paraphrase": rephrases a concept actually described in the snippet
  * "multi-topic": the question ITSELF must genuinely require combining
    information from 2+ documents to answer — never write a question fully
    answerable from one document and then attach a second, unrelated
    document's label just to satisfy this category. If a question only
    needs one document, categorize it as exact-identifier or paraphrase
    instead, even if other documents are available in this batch.
  * "thread": about the content of a document marked has_thread=yes, not its thread identifier
- Reference documents only by the label shown in brackets (e.g. DOC0).
  Never invent a label that is not listed below.

- Include the answer: copy the exact words from the snippet that answer the
  question. Copy them verbatim, do not paraphrase or summarise. If no span of
  the snippet answers the question, do not write the question at all.
- Prefer facts specific to this document over ones any similar document would
  repeat. A company name, registration number, address or phone number printed
  on every document in a series identifies none of them; a date, amount, name
  or statement particular to this one does.

Return ONLY valid JSON with this structure:
{"questions": [
  {"question": "...", "category": "...", "doc_labels": ["DOC0"], "answer": "..."}
]}

Valid categories: exact-identifier, paraphrase, multi-topic, thread.

Documents:
%s`, docEntries.String())

	// Answer spans made every response longer; at the old budget the JSON was
	// being truncated mid-array and whole batches were lost to a parse error.
	maxTokens := 4096
	resp, err := g.LLM.Complete(ctx, prompt, maxTokens)
	if err != nil {
		return nil, fmt.Errorf("LLM complete: %w", err)
	}

	questions, err := parseLLMQuestions(resp)
	if err != nil {
		return nil, fmt.Errorf("parse LLM response: %w", err)
	}

	var out []candidateCase
	for _, q := range questions {
		if q.Question == "" || q.Answer == "" {
			continue
		}
		cat := normalizeCategory(q.Category)
		if cat == "" {
			cat = CatParaphrase
		}

		// Resolve labels to real fixture_ids, dropping any label the LLM
		// invented that was never offered in this batch — a case grounded
		// in nothing real is worse than no case at all.
		var fixtureIDs, docIDs []string
		seen := make(map[string]struct{}, len(q.DocLabels))
		for _, label := range q.DocLabels {
			fid, ok := labelToFixtureID[label]
			if !ok {
				continue
			}
			if _, dup := seen[fid]; dup {
				continue
			}
			seen[fid] = struct{}{}
			fixtureIDs = append(fixtureIDs, fid)
			docIDs = append(docIDs, labelToDocID[label])
		}
		if len(fixtureIDs) == 0 {
			continue
		}

		c := candidateCase{
			Case:   Case{Category: cat, Question: q.Question},
			Answer: q.Answer,
			DocIDs: docIDs,
		}
		for _, fid := range fixtureIDs {
			c.ExpectedSources = append(c.ExpectedSources, ExpectedSource{FixtureID: fid})
		}

		// Assign grades for multi-topic cases.
		if cat == CatMultiTopic && len(c.ExpectedSources) > 1 {
			for i := range c.ExpectedSources {
				c.ExpectedSources[i].Grade = 2
			}
		} else {
			for i := range c.ExpectedSources {
				c.ExpectedSources[i].Grade = 3
			}
		}

		out = append(out, c)
	}

	return out, nil
}

// parseLLMQuestions extracts JSON from the LLM response, tolerating markdown
// fences or extra text.
func parseLLMQuestions(raw string) ([]llmQuestion, error) {
	// Strip markdown code fences if present.
	raw = strings.TrimSpace(raw)
	raw = regexp.MustCompile(`^`+"```"+`(?:json)?\s*`).ReplaceAllString(raw, "")
	raw = regexp.MustCompile(`\s*`+"```"+`$`).ReplaceAllString(raw, "")

	var resp llmBatchResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		// Try to find a JSON array in the response.
		start := strings.Index(raw, "[")
		end := strings.LastIndex(raw, "]")
		if start >= 0 && end > start {
			raw = raw[start : end+1]
			if err2 := json.Unmarshal([]byte(raw), &resp.Questions); err2 != nil {
				return nil, fmt.Errorf("parse JSON (raw error: %v, array error: %v)", err, err2)
			}
			return resp.Questions, nil
		}
		return nil, err
	}
	return resp.Questions, nil
}

// fixtureIDFromFilename derives a fixture_id from a source filename by
// stripping the extension, matching the evaluator's buildFixtureLookup logic.
func fixtureIDFromFilename(filename string) string {
	if filename == "" {
		return "unnamed"
	}
	if i := strings.LastIndex(filename, "."); i > 0 {
		return filename[:i]
	}
	return filename
}

// normalizeCategory maps LLM output to known category constants.
func normalizeCategory(cat string) string {
	switch strings.ToLower(strings.TrimSpace(cat)) {
	case "exact-identifier", "exact_identifier", "exactid":
		return CatExactIdentifier
	case "paraphrase":
		return CatParaphrase
	case "bilingual":
		return CatBilingual
	case "multi-topic", "multi_topic", "multitopic":
		return CatMultiTopic
	case "thread":
		return CatThread
	case "attachment":
		return CatAttachment
	case "off-domain", "off_domain", "offdomain":
		return CatOffDomain
	default:
		return ""
	}
}

// truncate shortens s to at most n runes, adding "…" if truncated.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// CasePath returns the convention-based path for evaluation cases for a
// workspace: workspaces/evaluation/<workspace-id>/cases.json.
func CasePath(workspaceID string) string {
	return filepath.Join("workspaces", "evaluation", workspaceID, "cases.json")
}

// GenerateFixtures is the top-level entry point called from the CLI. It
// resolves the output path and runs the generator.
func GenerateFixtures(
	ctx context.Context,
	db *postgres.DB,
	l *llm.Client,
	cfg config.Query,
	workspaceID string,
	outputPath string,
	log *slog.Logger,
) error {
	if outputPath == "" {
		outputPath = CasePath(workspaceID)
	}

	gen := NewGenerator(db, l, workspaceID, log)
	return gen.Generate(ctx, outputPath)
}

// SeedRandom is a no-op stub for API compatibility. The generator uses
// SQL ORDER BY RANDOM() for sampling; Go-side randomness is not involved.
func SeedRandom(_ int64) {}
