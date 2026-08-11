package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/suankan/pocket-advisor/internal/topicgraph"
)

// topicTimeline is deliberately the narrow, transport-independent graph
// operation. Keeping it behind an interface lets packet tests prove budget and
// evidence behaviour without a PostgreSQL graph fixture.
type topicTimeline interface {
	Timeline(context.Context, topicgraph.TimelineRequest) (*topicgraph.TimelineResult, error)
}

// GraphExpansion is an optional supplement to ordinary source-first packets.
// It contains no topic labels or generated summaries: every returned string is
// an exact cited source span. Normal packets remain the ranked answer and are
// never replaced or reordered by this derived view.
type GraphExpansion struct {
	GraphVersion string                        `json:"graph_version"`
	SnapshotAt   time.Time                     `json:"snapshot_at"`
	Nodes        []GraphEvidence               `json:"nodes"`
	Relations    []topicgraph.TimelineRelation `json:"relations"`
	Warnings     []string                      `json:"warnings"`
	OmittedNodes int                           `json:"omitted_nodes"`
}

// GraphEvidence is one graph mention represented only by its canonical source
// document and validated byte spans. Display labels are deliberately absent.
type GraphEvidence struct {
	MentionRef string      `json:"mention_ref"`
	Document   Document    `json:"document"`
	Spans      []GraphSpan `json:"spans"`
}

type GraphSpan struct {
	StartByte            int    `json:"start_byte"`
	EndByte              int    `json:"end_byte"`
	NormalizedTextSHA256 string `json:"normalized_text_sha256"`
	SliceSHA256          string `json:"slice_sha256"`
	Text                 string `json:"text"`
}

// expandTopicGraph is deliberately after buildPackets. The normal ranked
// packets and their lineage context always consume the aggregate allowance
// first, so graph expansion cannot displace ordinary retrieval evidence.
// Graph failure is a visible optional degradation, never a reason to fail an
// otherwise valid ordinary query.
func (s *Service) expandTopicGraph(ctx context.Context, picked []scored, budget *budgeter) (*GraphExpansion, []string) {
	if !s.cfg.TopicGraphExpansionEnabled || len(picked) == 0 || budget == nil {
		return nil, nil
	}
	if s.timeline == nil {
		return nil, []string{WarnTopicGraphUnavailable}
	}
	refs, err := s.topicGraphSeeds(ctx, picked)
	if err != nil {
		return nil, []string{WarnTopicGraphUnavailable}
	}
	if len(refs) == 0 {
		return nil, nil
	}

	limits := topicgraph.TimelineLimits{
		BackwardDepth: s.cfg.TopicGraphBackwardDepth,
		ForwardDepth:  s.cfg.TopicGraphForwardDepth,
		MaxNodes:      s.cfg.TopicGraphMaxNodes,
		MaxBytes:      minInt(s.cfg.TopicGraphMaxBytes, budget.remaining),
		MaxLatency:    2 * time.Second,
	}
	// The config defaults are valid. Treat a depleted shared allowance as a
	// bounded omission rather than handing an invalid zero-byte request to the
	// graph service.
	if limits.MaxBytes <= 0 {
		budget.truncated = true
		return &GraphExpansion{Nodes: []GraphEvidence{}, Relations: []topicgraph.TimelineRelation{}, Warnings: []string{topicgraph.WarnTimelineByteLimit}, OmittedNodes: len(refs)}, []string{topicgraph.WarnTimelineByteLimit}
	}
	result, err := s.timeline.Timeline(ctx, topicgraph.TimelineRequest{References: refs, Limits: limits})
	if err != nil {
		return nil, []string{WarnTopicGraphUnavailable}
	}
	return s.packGraphEvidence(ctx, result, budget)
}

// topicGraphSeeds admits only spans that overlap an ordinary selected hit and
// that belong to the one ACTIVE graph version with at least one supported
// high-confidence relation. Labels, subjects, embeddings, and graph-only
// mentions never create a seed.
func (s *Service) topicGraphSeeds(ctx context.Context, picked []scored) ([]string, error) {
	docIDs := make([]string, 0, len(picked))
	starts := make([]int, 0, len(picked))
	ends := make([]int, 0, len(picked))
	for _, hit := range picked {
		if hit.DocID == "" || hit.StartByte < 0 || hit.EndByte <= hit.StartByte {
			continue
		}
		docIDs = append(docIDs, hit.DocID)
		starts = append(starts, hit.StartByte)
		ends = append(ends, hit.EndByte)
	}
	if len(docIDs) == 0 {
		return nil, nil
	}
	limit := s.cfg.TopicGraphMaxSeeds
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.DB.Pool.Query(ctx, `
        SELECT DISTINCT tm.version_id::text, tm.mention_id::text
        FROM topic_graph_versions graph_version
        JOIN topic_mentions tm ON tm.workspace_id = graph_version.workspace_id
                             AND tm.version_id = graph_version.version_id
        JOIN topic_mention_spans span ON span.mention_id = tm.mention_id
                                   AND span.workspace_id = tm.workspace_id
                                   AND span.doc_id = tm.doc_id
        JOIN unnest($2::uuid[], $3::int[], $4::int[]) AS hit(doc_id, start_byte, end_byte)
          ON hit.doc_id = tm.doc_id
         AND span.start_byte < hit.end_byte
         AND span.end_byte > hit.start_byte
        WHERE version.workspace_id = $1 AND version.status = 'ACTIVE'
          AND EXISTS (
              SELECT 1 FROM topic_relation_edges edge
              WHERE edge.workspace_id = tm.workspace_id AND edge.version_id = tm.version_id
                AND (edge.earlier_mention_id = tm.mention_id OR edge.later_mention_id = tm.mention_id)
                AND edge.confidence >= $5)
        ORDER BY tm.version_id, tm.mention_id
        LIMIT $6`, s.workspace, docIDs, starts, ends, s.cfg.TopicGraphMinConfidence, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := make([]string, 0, limit)
	for rows.Next() {
		var versionID, mentionID string
		if err := rows.Scan(&versionID, &mentionID); err != nil {
			return nil, err
		}
		if ref := topicgraph.EncodeMentionReference(versionID, mentionID); ref != "" {
			refs = append(refs, ref)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return refs, nil
}

// packGraphEvidence converts validated timeline citations to source text and
// charges exactly the cited UTF-8 ranges. A stale or mismatched current source
// is omitted rather than repaired or substituted.
func (s *Service) packGraphEvidence(ctx context.Context, result *topicgraph.TimelineResult, budget *budgeter) (*GraphExpansion, []string) {
	if result == nil {
		return &GraphExpansion{Nodes: []GraphEvidence{}, Relations: []topicgraph.TimelineRelation{}, Warnings: []string{}}, []string{WarnTopicGraphUnavailable}
	}
	ids := make([]string, 0, len(result.Nodes))
	seen := make(map[string]struct{}, len(result.Nodes))
	for _, node := range result.Nodes {
		for _, citation := range node.Evidence {
			id, err := topicgraph.DocumentIDFromCitation(citation.DocumentRef)
			if err != nil {
				continue
			}
			if _, duplicate := seen[id]; !duplicate {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
	}
	docs, texts, err := s.loadDocuments(ctx, ids)
	if err != nil {
		out := graphExpansionHeader(result)
		out.Relations = []topicgraph.TimelineRelation{}
		out.OmittedNodes += len(result.Nodes)
		out.Warnings = addGraphWarning(out.Warnings, WarnTopicGraphUnavailable)
		return out, out.Warnings
	}
	return packGraphNodes(result, docs, texts, budget)
}

// packGraphNodes is the accounting boundary shared by PostgreSQL-backed and
// synthetic callers. It emits an entire node only when every exact source span
// validates and fits; relations are retained only when both cited endpoints
// remain present.
func packGraphNodes(result *topicgraph.TimelineResult, docs map[string]Document, texts map[string]string, budget *budgeter) (*GraphExpansion, []string) {
	out := graphExpansionHeader(result)
	if result == nil || budget == nil {
		out.Warnings = addGraphWarning(out.Warnings, WarnTopicGraphUnavailable)
		return out, out.Warnings
	}
	included := make(map[string]struct{}, len(result.Nodes))
	for _, node := range result.Nodes {
		item, valid := graphNodeFromCitations(node, docs, texts)
		if !valid {
			out.OmittedNodes++
			out.Warnings = addGraphWarning(out.Warnings, topicgraph.WarnTimelineEvidence)
			continue
		}
		bytes := 0
		for _, span := range item.Spans {
			bytes += len(span.Text)
		}
		if bytes > budget.remaining {
			budget.truncated = true
			out.OmittedNodes++
			out.Warnings = addGraphWarning(out.Warnings, topicgraph.WarnTimelineByteLimit)
			continue
		}
		for _, span := range item.Spans {
			// The aggregate check above guarantees success; retain take as the one
			// accounting authority shared with normal packets.
			if _, ok := budget.take(span.Text); !ok {
				budget.truncated = true
				out.OmittedNodes++
				out.Warnings = addGraphWarning(out.Warnings, topicgraph.WarnTimelineByteLimit)
				valid = false
				break
			}
		}
		if valid {
			out.Nodes = append(out.Nodes, item)
			included[node.MentionRef] = struct{}{}
		}
	}
	for _, relation := range result.Relations {
		if _, early := included[relation.EarlierMentionRef]; !early {
			continue
		}
		if _, late := included[relation.LaterMentionRef]; !late {
			continue
		}
		out.Relations = append(out.Relations, relation)
	}
	return out, out.Warnings
}

func graphExpansionHeader(result *topicgraph.TimelineResult) *GraphExpansion {
	out := &GraphExpansion{Nodes: []GraphEvidence{}, Relations: []topicgraph.TimelineRelation{}, Warnings: []string{}}
	if result == nil {
		return out
	}
	out.GraphVersion = result.GraphVersion
	out.SnapshotAt = result.SnapshotAt
	out.Warnings = append(out.Warnings, result.Warnings...)
	out.OmittedNodes = result.OmittedNodes
	return out
}

func graphNodeFromCitations(node topicgraph.TimelineNode, docs map[string]Document, texts map[string]string) (GraphEvidence, bool) {
	if node.MentionRef == "" || len(node.Evidence) == 0 {
		return GraphEvidence{}, false
	}
	item := GraphEvidence{MentionRef: node.MentionRef, Spans: make([]GraphSpan, 0, len(node.Evidence))}
	var documentID string
	for _, citation := range node.Evidence {
		id, err := topicgraph.DocumentIDFromCitation(citation.DocumentRef)
		if err != nil || (documentID != "" && id != documentID) {
			return GraphEvidence{}, false
		}
		documentID = id
		doc, found := docs[id]
		text, foundText := texts[id]
		if !found || !foundText || !validGraphCitation(text, citation) {
			return GraphEvidence{}, false
		}
		item.Document = doc
		item.Spans = append(item.Spans, GraphSpan{
			StartByte: citation.StartByte, EndByte: citation.EndByte,
			NormalizedTextSHA256: citation.NormalizedTextSHA256, SliceSHA256: citation.SliceSHA256,
			Text: text[citation.StartByte:citation.EndByte],
		})
	}
	return item, true
}

func validGraphCitation(text string, citation topicgraph.Citation) bool {
	if citation.StartByte < 0 || citation.EndByte <= citation.StartByte || citation.EndByte > len(text) ||
		!graphByteBoundary(text, citation.StartByte) || !graphByteBoundary(text, citation.EndByte) {
		return false
	}
	full := sha256.Sum256([]byte(text))
	slice := sha256.Sum256([]byte(text[citation.StartByte:citation.EndByte]))
	return citation.NormalizedTextSHA256 == hex.EncodeToString(full[:]) && citation.SliceSHA256 == hex.EncodeToString(slice[:])
}

func graphByteBoundary(text string, offset int) bool {
	return offset == 0 || offset == len(text) || (offset > 0 && offset < len(text) && text[offset]&0xc0 != 0x80)
}

func addGraphWarning(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
