package mcp

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/suankan/pocket-advisor/internal/retrieval"
)

const (
	utf8ByteUnit              = "utf8_bytes"
	deliveryMoreAvailable     = "more_evidence_available"
	targetToolResultBytes     = 48 * 1024
	absoluteToolResponseBytes = 50 * 1024
	targetReadableLines       = 1800
	absoluteReadableLines     = 2000
)

// CallToolResult is the MCP result envelope. StructuredContent is omitted on
// tool errors because output schemas describe successful pages, while Content
// remains present for every supported client revision.
type CallToolResult struct {
	Content           []TextContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError"`
}

type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// EvidenceResult is the immutable, session-local snapshot produced from one
// retrieval result. It is never returned whole: EvidencePage exposes its
// compact index and admitted text through bounded MCP responses.
type EvidenceResult struct {
	ResultID   string
	Question   string
	SubQueries []string
	Packets    []EvidencePacket
	Warnings   []string
	Budget     EvidenceBudget
}

type EvidenceBudget struct {
	Used    int    `json:"used"`
	Allowed int    `json:"allowed"`
	Unit    string `json:"unit"`
}

type ResponseBudget struct {
	Used    int    `json:"used"`
	Allowed int    `json:"allowed"`
	Unit    string `json:"unit"`
}

type EvidencePacket struct {
	Reference   string
	Rank        int
	Document    EvidenceDocument
	Match       EvidenceMatch
	Text        *string
	TextOmitted bool
	Related     []EvidenceRelated
}

type EvidenceDocument struct {
	DocumentID       string  `json:"document_id"`
	ParentDocumentID *string `json:"parent_document_id"`
	ThreadID         *string `json:"thread_id"`
	DocumentType     string  `json:"document_type"`
	Title            string  `json:"title"`
	From             string  `json:"from"`
	To               string  `json:"to"`
	Date             *string `json:"date"`
	SourceSHA256     string  `json:"source_sha256"`
	Tier1URI         string  `json:"tier1_uri"`
	// WorkspaceRelativePath is the document's original file location relative
	// to the workspace's local staging directory, exactly as recorded at
	// upload time. It is informational only, for the human operator to open
	// the original file themselves — the agent must not treat it as
	// fetchable or resolve it on the agent's own filesystem. It is absent
	// when the document was never its own file on disk, such as an email
	// attachment.
	WorkspaceRelativePath *string `json:"workspace_relative_path"`
	// ContainerWorkspaceRelativePath is the staged file this document was
	// extracted from, present exactly when WorkspaceRelativePath is absent.
	// It exists so an extracted document can still be located honestly:
	// without it a caller can only guess from sibling documents, which is
	// how a confident wrong location gets reported.
	ContainerWorkspaceRelativePath *string `json:"container_workspace_relative_path"`
}

type EvidenceMatch struct {
	ChunkID    string  `json:"chunk_id"`
	Start      int     `json:"start"`
	End        int     `json:"end"`
	OffsetUnit string  `json:"offset_unit"`
	Score      float64 `json:"score"`
	Legs       string  `json:"legs"`
	SubQuery   *string `json:"sub_query"`
	Snippet    string  `json:"snippet"`
}

type EvidenceRelated struct {
	Relation    string
	Document    EvidenceDocument
	Text        *string
	TextOmitted bool
}

// EvidencePage is the durable MCP output contract shared by the search and
// evidence-reading tools. Search and index pages carry compact ranked packets;
// text pages carry one bounded segment of admitted source text.
type EvidencePage struct {
	Kind             string                `json:"kind"`
	ResultID         string                `json:"result_id"`
	Question         *string               `json:"question"`
	SubQueries       []string              `json:"sub_queries"`
	Packets          []EvidenceIndexPacket `json:"packets"`
	Segments         []EvidenceSegment     `json:"segments"`
	Warnings         []string              `json:"warnings"`
	DeliveryWarnings []string              `json:"delivery_warnings"`
	EvidenceBudget   EvidenceBudget        `json:"evidence_budget"`
	ResponseBudget   ResponseBudget        `json:"response_budget"`
	Complete         bool                  `json:"complete"`
	NextCursor       *string               `json:"next_cursor"`
	ContinuationTool *string               `json:"continuation_tool"`
}

type EvidenceIndexPacket struct {
	Reference            string           `json:"reference"`
	Rank                 int              `json:"rank"`
	Document             EvidenceDocument `json:"document"`
	Match                EvidenceMatch    `json:"match"`
	TextAvailable        bool             `json:"text_available"`
	TextOmitted          bool             `json:"text_omitted"`
	RelatedCount         int              `json:"related_count"`
	RelatedTextAvailable int              `json:"related_text_available"`
}

type EvidenceSegment struct {
	Reference    string           `json:"reference"`
	Relation     *string          `json:"relation"`
	Document     EvidenceDocument `json:"document"`
	Text         string           `json:"text"`
	Range        EvidenceRange    `json:"text_range"`
	TextComplete bool             `json:"text_complete"`
}

type EvidenceRange struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Unit  string `json:"unit"`
}

func newEvidenceResult(res *retrieval.Result, resultID string) (*EvidenceResult, error) {
	if res == nil {
		return nil, fmt.Errorf("nil retrieval result")
	}

	out := &EvidenceResult{
		ResultID:   resultID,
		Question:   res.Question,
		SubQueries: append([]string{}, res.SubQueries...),
		Packets:    make([]EvidencePacket, 0, len(res.Packets)),
		Warnings:   append([]string{}, res.Warnings...),
		Budget: EvidenceBudget{
			Used: res.Budget.BytesUsed, Allowed: res.Budget.BytesAllowed, Unit: utf8ByteUnit,
		},
	}
	for i, packet := range res.Packets {
		text, omitted := evidenceText(packet.Text, packet.CharCount)
		evidencePacket := EvidencePacket{
			Reference: resultReference(resultID, i+1),
			Rank:      i + 1,
			Document:  evidenceDocument(packet.Document),
			Match: EvidenceMatch{
				ChunkID: packet.Match.ChunkID, Start: packet.Match.StartByte, End: packet.Match.EndByte,
				OffsetUnit: utf8ByteUnit, Score: packet.Match.Score, Legs: packet.Match.Legs,
				SubQuery: nullableString(packet.Match.SubQuery), Snippet: packet.Match.Snippet,
			},
			Text: text, TextOmitted: omitted,
			Related: make([]EvidenceRelated, 0, len(packet.Related)),
		}
		for _, related := range packet.Related {
			relatedText, relatedOmitted := evidenceText(related.Text, related.CharCount)
			evidencePacket.Related = append(evidencePacket.Related, EvidenceRelated{
				Relation: string(related.Relation), Document: evidenceDocument(related.Document),
				Text: relatedText, TextOmitted: relatedOmitted,
			})
		}
		out.Packets = append(out.Packets, evidencePacket)
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

func evidenceDocument(document retrieval.Document) EvidenceDocument {
	var date *string
	if document.Date != nil {
		formatted := document.Date.Format(time.RFC3339Nano)
		date = &formatted
	}
	return EvidenceDocument{
		DocumentID: document.DocID, ParentDocumentID: nullableString(document.ParentID),
		ThreadID: nullableString(document.ThreadID), DocumentType: document.DocType,
		Title: document.Title, From: document.From, To: document.To, Date: date,
		SourceSHA256: document.SHA256, Tier1URI: document.RawURI,
		WorkspaceRelativePath:          nullableString(document.SourcePath),
		ContainerWorkspaceRelativePath: nullableString(document.ContainerPath),
	}
}

func evidenceText(text string, sourceLength int) (*string, bool) {
	if text == "" && sourceLength > 0 {
		return nil, true
	}
	copy := text
	return &copy, false
}

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

func resultReference(resultID string, rank int) string {
	return fmt.Sprintf("%s:E%d", resultID, rank)
}

// Validate enforces snapshot invariants that JSON Schema cannot express.
func (result *EvidenceResult) Validate() error {
	if result == nil || result.ResultID == "" || strings.TrimSpace(result.Question) == "" {
		return fmt.Errorf("evidence identity or question is empty")
	}
	if result.SubQueries == nil || result.Packets == nil || result.Warnings == nil {
		return fmt.Errorf("evidence collections must not be null")
	}
	if err := validateBudget(result.Budget); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(result.Packets))
	for i := range result.Packets {
		packet := &result.Packets[i]
		wantReference := resultReference(result.ResultID, i+1)
		if packet.Rank != i+1 || packet.Reference != wantReference {
			return fmt.Errorf("packet %d has unstable rank or reference", i+1)
		}
		if _, duplicate := seen[packet.Reference]; duplicate {
			return fmt.Errorf("duplicate packet reference %q", packet.Reference)
		}
		seen[packet.Reference] = struct{}{}
		if err := validateDocument(packet.Document); err != nil {
			return fmt.Errorf("packet %s: %w", packet.Reference, err)
		}
		if packet.Match.ChunkID == "" || packet.Match.Start < 0 || packet.Match.End < packet.Match.Start || packet.Match.OffsetUnit != utf8ByteUnit {
			return fmt.Errorf("packet %s has invalid match provenance", packet.Reference)
		}
		if math.IsNaN(packet.Match.Score) || math.IsInf(packet.Match.Score, 0) {
			return fmt.Errorf("packet %s has a non-finite score", packet.Reference)
		}
		if packet.Match.Legs != "dense" && packet.Match.Legs != "lexical" && packet.Match.Legs != "both" {
			return fmt.Errorf("packet %s has invalid search legs", packet.Reference)
		}
		if err := validateText(packet.Text, packet.TextOmitted); err != nil {
			return fmt.Errorf("packet %s: %w", packet.Reference, err)
		}
		if packet.Related == nil {
			return fmt.Errorf("packet %s related documents are null", packet.Reference)
		}
		for j := range packet.Related {
			related := &packet.Related[j]
			if !validRelation(related.Relation) {
				return fmt.Errorf("packet %s has invalid relation", packet.Reference)
			}
			if err := validateDocument(related.Document); err != nil {
				return fmt.Errorf("packet %s related document: %w", packet.Reference, err)
			}
			if err := validateText(related.Text, related.TextOmitted); err != nil {
				return fmt.Errorf("packet %s related document: %w", packet.Reference, err)
			}
		}
	}
	return nil
}

func validateBudget(budget EvidenceBudget) error {
	if budget.Unit != utf8ByteUnit || budget.Used < 0 || budget.Allowed < 0 || budget.Used > budget.Allowed {
		return fmt.Errorf("invalid evidence budget")
	}
	return nil
}

func validateDocument(document EvidenceDocument) error {
	if document.DocumentID == "" || document.DocumentType == "" || document.SourceSHA256 == "" || document.Tier1URI == "" {
		return fmt.Errorf("document provenance is incomplete")
	}
	return nil
}

func validateText(text *string, omitted bool) error {
	if omitted && text != nil {
		return fmt.Errorf("omitted text must be null")
	}
	if !omitted && text == nil {
		return fmt.Errorf("non-omitted text must be present")
	}
	return nil
}

func validRelation(relation string) bool {
	return relation == string(retrieval.RelationParent) ||
		relation == string(retrieval.RelationChild) ||
		relation == string(retrieval.RelationThreadPeer)
}

func (page *EvidencePage) Validate() error {
	if page == nil || page.ResultID == "" {
		return fmt.Errorf("page result identity is empty")
	}
	if page.Kind != "search" && page.Kind != "index" && page.Kind != "text" {
		return fmt.Errorf("invalid evidence page kind")
	}
	if page.SubQueries == nil || page.Packets == nil || page.Segments == nil || page.Warnings == nil || page.DeliveryWarnings == nil {
		return fmt.Errorf("page collections must not be null")
	}
	if err := validateBudget(page.EvidenceBudget); err != nil {
		return err
	}
	if page.ResponseBudget.Unit != utf8ByteUnit || page.ResponseBudget.Used <= 0 || page.ResponseBudget.Allowed != targetToolResultBytes || page.ResponseBudget.Used > page.ResponseBudget.Allowed {
		return fmt.Errorf("invalid response budget")
	}
	if page.Complete {
		if page.NextCursor != nil || page.ContinuationTool != nil || len(page.DeliveryWarnings) != 0 {
			return fmt.Errorf("complete page has continuation state")
		}
	} else {
		if page.NextCursor == nil || *page.NextCursor == "" || page.ContinuationTool == nil || *page.ContinuationTool == "" {
			return fmt.Errorf("incomplete page lacks continuation state")
		}
		if len(page.DeliveryWarnings) != 1 || page.DeliveryWarnings[0] != deliveryMoreAvailable {
			return fmt.Errorf("incomplete page lacks delivery warning")
		}
	}
	for _, packet := range page.Packets {
		if packet.Reference != resultReference(page.ResultID, packet.Rank) {
			return fmt.Errorf("index packet reference is outside result")
		}
		if err := validateDocument(packet.Document); err != nil {
			return err
		}
	}
	for _, segment := range page.Segments {
		if !strings.HasPrefix(segment.Reference, page.ResultID+":E") || segment.Text == "" || segment.Range.Unit != utf8ByteUnit || segment.Range.Start < 0 || segment.Range.End <= segment.Range.Start {
			return fmt.Errorf("invalid evidence segment")
		}
		if segment.Relation != nil && !validRelation(*segment.Relation) {
			return fmt.Errorf("invalid segment relation")
		}
		if err := validateDocument(segment.Document); err != nil {
			return err
		}
	}
	return nil
}

func indexPacket(packet EvidencePacket) EvidenceIndexPacket {
	relatedAvailable := 0
	for _, related := range packet.Related {
		if related.Text != nil && *related.Text != "" {
			relatedAvailable++
		}
	}
	return EvidenceIndexPacket{
		Reference: packet.Reference, Rank: packet.Rank, Document: packet.Document, Match: packet.Match,
		TextAvailable: packet.Text != nil && *packet.Text != "", TextOmitted: packet.TextOmitted,
		RelatedCount: len(packet.Related), RelatedTextAvailable: relatedAvailable,
	}
}

func finalizePage(page *EvidencePage) (CallToolResult, error) {
	page.ResponseBudget = ResponseBudget{Used: targetToolResultBytes, Allowed: targetToolResultBytes, Unit: utf8ByteUnit}
	var result CallToolResult
	for range 4 {
		result = CallToolResult{
			Content:           []TextContent{{Type: "text", Text: renderEvidencePage(page)}},
			StructuredContent: page,
			IsError:           false,
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return CallToolResult{}, fmt.Errorf("encode evidence page: %w", err)
		}
		used := len(encoded)
		if used == page.ResponseBudget.Used {
			break
		}
		page.ResponseBudget.Used = used
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return CallToolResult{}, fmt.Errorf("encode evidence page: %w", err)
	}
	if len(encoded) > targetToolResultBytes {
		return CallToolResult{}, &resultSizeError{limit: targetToolResultBytes}
	}
	if readableLines(result.Content[0].Text) > targetReadableLines {
		return CallToolResult{}, &resultLineError{limit: targetReadableLines}
	}
	if err := page.Validate(); err != nil {
		return CallToolResult{}, fmt.Errorf("validate evidence page: %w", err)
	}
	return result, nil
}

func pageFits(page *EvidencePage) bool {
	copy := *page
	_, err := finalizePage(&copy)
	return err == nil
}

func readableLines(value string) int {
	if value == "" {
		return 0
	}
	return strings.Count(value, "\n") + 1
}

func renderEvidencePage(page *EvidencePage) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Pocket Advisor evidence result %s.\n", page.ResultID)
	if len(page.Warnings) > 0 {
		fmt.Fprintf(&builder, "Retrieval warnings: %s\n", strings.Join(page.Warnings, ", "))
	}

	if len(page.Packets) > 0 {
		fmt.Fprintf(&builder, "\n%d ranked packet(s) on this %s page. Cite the complete references shown.\n", len(page.Packets), page.Kind)
		for _, packet := range page.Packets {
			fmt.Fprintf(&builder, "\n[%s] %s\n", packet.Reference, packet.Document.Title)
			metadata := []string{packet.Document.DocumentType}
			if packet.Document.Date != nil {
				metadata = append(metadata, displayDate(*packet.Document.Date))
			}
			if packet.Document.From != "" {
				metadata = append(metadata, "from "+packet.Document.From)
			}
			if packet.Document.To != "" {
				metadata = append(metadata, "to "+packet.Document.To)
			}
			fmt.Fprintln(&builder, strings.Join(metadata, " · "))
			fmt.Fprintf(&builder, "cite: %s (doc %s, UTF-8 bytes %d-%d, relevance %+.3f)\n",
				packet.Document.Tier1URI, shortID(packet.Document.DocumentID), packet.Match.Start, packet.Match.End, packet.Match.Score)
			if packet.Document.WorkspaceRelativePath != nil {
				fmt.Fprintf(&builder, "local file: %s (tell the user this location; do not fetch it yourself)\n",
					*packet.Document.WorkspaceRelativePath)
			} else if packet.Document.ContainerWorkspaceRelativePath != nil {
				fmt.Fprintf(&builder, "local file: none of its own; extracted from %s (report it this way; do not guess a path from other documents)\n",
					*packet.Document.ContainerWorkspaceRelativePath)
			}
			fmt.Fprintf(&builder, "matched snippet:\n%s\n", packet.Match.Snippet)
			switch {
			case packet.TextAvailable:
				builder.WriteString("Admitted full text is available through the continuation tool.\n")
			case packet.TextOmitted:
				builder.WriteString("Full text was omitted by the aggregate evidence budget.\n")
			default:
				builder.WriteString("The source has no non-empty admitted full text.\n")
			}
			if packet.RelatedCount > 0 {
				fmt.Fprintf(&builder, "%d related document(s); %d have admitted text available through continuation.\n", packet.RelatedCount, packet.RelatedTextAvailable)
			}
		}
	}

	for _, segment := range page.Segments {
		relation := "matched document"
		if segment.Relation != nil {
			relation = *segment.Relation
		}
		fmt.Fprintf(&builder, "\n[%s] %s — %s\n", segment.Reference, relation, segment.Document.Title)
		fmt.Fprintf(&builder, "UTF-8 bytes %d-%d of admitted document text; text_complete=%t\n",
			segment.Range.Start, segment.Range.End, segment.TextComplete)
		fmt.Fprintf(&builder, "source text:\n%s\n", segment.Text)
	}

	if page.Kind == "search" && len(page.Packets) == 0 && page.Complete {
		builder.WriteString("\nNo sources in this corpus match that question. Nothing was found to answer from; say so rather than answering from general knowledge.\n")
	}

	if !page.Complete {
		fmt.Fprintf(&builder, "\nMORE ADMITTED EVIDENCE IS AVAILABLE. Call %s with exactly {\"cursor\":%q}. Do not claim complete corpus coverage until a page returns complete=true.\n",
			*page.ContinuationTool, *page.NextCursor)
	} else {
		builder.WriteString("\nDelivery complete: all evidence admitted by this result's aggregate budget has been delivered.\n")
	}
	fmt.Fprintf(&builder, "Evidence budget: %d of %d UTF-8 bytes. Response budget: %d of %d UTF-8 bytes.\n",
		page.EvidenceBudget.Used, page.EvidenceBudget.Allowed, page.ResponseBudget.Used, page.ResponseBudget.Allowed)
	return builder.String()
}

func displayDate(value string) string {
	if len(value) >= len("2006-01-02") {
		return value[:len("2006-01-02")]
	}
	return value
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
