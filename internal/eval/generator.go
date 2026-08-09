package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
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
	Snippet  string // first 500 chars of normalized text
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
var SampleSize = 30

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
func (g *Generator) sampleDocuments(ctx context.Context) ([]sampledDoc, error) {
	rows, err := g.DB.Pool.Query(ctx, `
SELECT doc_id::text, COALESCE(NULLIF(source_filename, ''), ''),
       LEFT(normalized_text, 500) as snippet,
       COALESCE(thread_id, ''), COALESCE(collection_id, '')
FROM documents
WHERE processing_status = 'COMPLETED'
  AND normalized_text IS NOT NULL
  AND LENGTH(normalized_text) > 50
ORDER BY RANDOM()
LIMIT $1`, SampleSize)
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

// generateCases sends document snippets to the LLM and parses structured
// case output.
func (g *Generator) generateCases(ctx context.Context, docs []sampledDoc) ([]Case, error) {
	var allCases []Case
	caseNum := 0

	// Process documents in batches to avoid overwhelming the LLM.
	const batchSize = 5
	for i := 0; i < len(docs); i += batchSize {
		end := i + batchSize
		if end > len(docs) {
			end = len(docs)
		}
		batch := docs[i:end]

		batchCases, err := g.generateBatch(ctx, batch, caseNum)
		if err != nil {
			g.Log.Warn("batch generation failed, skipping", "batch", i/batchSize, "error", err)
			continue
		}
		allCases = append(allCases, batchCases...)
		caseNum += len(batchCases)
	}

	return allCases, nil
}

// llmQuestion is the JSON structure the LLM is expected to return per document.
type llmQuestion struct {
	Question   string   `json:"question"`
	Category   string   `json:"category"`
	FixtureIDs []string `json:"fixture_ids"` // source filenames without extension
}

// llmBatchResponse is the top-level structure from the LLM for a batch.
type llmBatchResponse struct {
	Questions []llmQuestion `json:"questions"`
}

// generateBatch sends a batch of documents to the LLM and returns cases.
func (g *Generator) generateBatch(ctx context.Context, docs []sampledDoc, startNum int) ([]Case, error) {
	var docEntries strings.Builder
	for i, d := range docs {
		fid := fixtureIDFromFilename(d.Filename)
		docEntries.WriteString(fmt.Sprintf(
			"[DOC %d] fixture_id=%q source=%q thread_id=%q snippet=%s\n",
			i, fid, d.Source, d.ThreadID, truncate(d.Snippet, 300)))
	}

	prompt := fmt.Sprintf(`You are generating evaluation questions for a retrieval system.
Given document snippets, produce questions that test whether the system can find the right documents.

RULES:
- Generate 2-3 questions per document.
- Mix categories across the batch:
  * "exact-identifier": questions about specific facts, names, dates, or identifiers in the document
  * "paraphrase": questions that rephrase concepts from the document
  * "multi-topic": questions that span multiple documents (use fixture_ids from multiple docs)
  * "thread": questions about email threads (only for docs with non-empty thread_id)
- Questions must require reading the document to answer.
- Each question lists which documents (by fixture_id) contain the answer.
- Multi-topic questions must list fixture_ids from at least 2 different documents.

Return ONLY valid JSON with this structure:
{"questions": [
  {"question": "...", "category": "...", "fixture_ids": ["fixture-id-1"]}
]}

Valid categories: exact-identifier, paraphrase, multi-topic, thread.

Documents:
%s`, docEntries.String())

	maxTokens := 2048
	resp, err := g.LLM.Complete(ctx, prompt, maxTokens)
	if err != nil {
		return nil, fmt.Errorf("LLM complete: %w", err)
	}

	questions, err := parseLLMQuestions(resp)
	if err != nil {
		return nil, fmt.Errorf("parse LLM response: %w", err)
	}

	var cases []Case
	batchCaseNum := 0
	for _, q := range questions {
		if q.Question == "" {
			continue
		}
		cat := normalizeCategory(q.Category)
		if cat == "" {
			cat = CatParaphrase
		}

		// Determine primary fixture_id (the one with the most information).
		fixtureIDs := q.FixtureIDs
		if len(fixtureIDs) == 0 {
			// Fall back to first doc in batch.
			if len(docs) > 0 {
				fixtureIDs = []string{fixtureIDFromFilename(docs[0].Filename)}
			}
		}

		batchCaseNum++
		c := Case{
			ID:       fmt.Sprintf("gen-%04d", startNum+batchCaseNum),
			Category: cat,
			Question: q.Question,
		}
		for _, fid := range fixtureIDs {
			c.ExpectedSources = append(c.ExpectedSources, ExpectedSource{
				FixtureID: fid,
			})
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

		cases = append(cases, c)
	}

	return cases, nil
}

// parseLLMQuestions extracts JSON from the LLM response, tolerating markdown
// fences or extra text.
func parseLLMQuestions(raw string) ([]llmQuestion, error) {
	// Strip markdown code fences if present.
	raw = strings.TrimSpace(raw)
	raw = regexp.MustCompile(`^` + "```" + `(?:json)?\s*`).ReplaceAllString(raw, "")
	raw = regexp.MustCompile(`\s*` + "```" + `$`).ReplaceAllString(raw, "")

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
