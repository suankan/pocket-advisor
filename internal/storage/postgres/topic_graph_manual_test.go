//go:build manual

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/topicgraph"
)

// Database-backed rehearsal of the topic mention substrate. It uses the same
// disposable EMAIL_DSN convention as email_manual_test.go:
//
//	EMAIL_DSN=postgres://<role>@localhost:5432/<disposable-db> \
//	  go test -tags manual ./internal/storage/postgres/ -run TopicGraph -v
func TestTopicGraphSchemaFreshAndUpgrade(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		legacy bool
	}{{"topicfresh", false}, {"topicupgrade", true}} {
		t.Run(tc.name, func(t *testing.T) {
			db, schema := scratch(t, tc.name)
			if tc.legacy {
				if _, err := db.Pool.Exec(ctx, fmt.Sprintf(coreSchemaSQL, stageBDim)); err != nil {
					t.Fatalf("apply legacy core: %v", err)
				}
				if _, err := db.Pool.Exec(ctx, `
                    INSERT INTO schema_metadata (id, embed_model, embed_dim, truncated_dim)
                    VALUES (TRUE, $1, $2, FALSE)`, "stage-b-manual", stageBDim); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
				t.Fatalf("apply schema: %v", err)
			}
			for _, table := range []string{"topic_graph_versions", "topic_mentions", "topic_mention_spans", "topic_relation_candidates", "topic_relation_candidate_supports", "topic_relation_edges", "topic_episodes", "topic_episode_memberships"} {
				if !relationExists(t, db, schema, table) {
					t.Errorf("table %s missing", table)
				}
			}
			for _, index := range []string{"topic_graph_versions_one_active_idx", "topic_mentions_source_idx", "topic_mention_spans_source_idx", "topic_relation_candidates_mentions_idx", "topic_relation_edges_forward_idx", "topic_episode_memberships_mention_idx"} {
				if !indexExists(t, db, schema, index) {
					t.Errorf("index %s missing", index)
				}
			}
			if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
				t.Fatalf("repeat apply: %v", err)
			}
		})
	}
}

func topicDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func topicMention(docID, text string) topicgraph.Mention {
	return topicgraph.Mention{
		DocID: docID, DisplayLabel: "review", ExtractionVersion: "extract-v1",
		Spans: []topicgraph.SourceSpan{{
			DocID: docID, StartByte: 0, EndByte: len("café"), NormalizedTextSHA256: topicDigest(text),
			SliceSHA256: topicDigest(text[:len("café")]),
		}},
	}
}

func topicSpec(id string) topicgraph.VersionSpec {
	return topicgraph.VersionSpec{
		ID: id, ExtractionVersion: "extract-v1", ConfigVersion: "topic-config-v1",
		Limits: topicgraph.DefaultLimits(),
	}
}

func TestTopicGraphReplacementAndLifecycle(t *testing.T) {
	db, _ := scratch(t, "topicrepo")
	ctx := context.Background()
	if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
		t.Fatal(err)
	}
	emailRepo := NewEmailRepo(db)
	topicRepo := NewTopicGraphRepo(db)
	text := "café follow-up"
	docID := document(t, db, strings.Repeat("e", 64))
	if _, err := db.Pool.Exec(ctx, `UPDATE documents SET normalized_text = $1 WHERE doc_id = $2`, text, docID); err != nil {
		t.Fatal(err)
	}
	if _, err := emailRepo.SaveEmailMessage(ctx, message(docID, "topic@mail.example.test", "Topic", "")); err != nil {
		t.Fatal(err)
	}

	firstID := "11111111-1111-5111-8111-111111111111"
	if err := topicRepo.CreateBuilding(ctx, stageBWorkspace, topicSpec(firstID)); err != nil {
		t.Fatalf("create: %v", err)
	}
	request := topicgraph.ReplaceRequest{
		VersionID: firstID, TargetDocIDs: []string{docID}, Mentions: []topicgraph.Mention{topicMention(docID, text)},
	}
	if err := topicRepo.ReplaceMentions(ctx, stageBWorkspace, request); err != nil {
		t.Fatalf("replace: %v", err)
	}
	// At-least-once delivery must converge to one deterministic mention/span.
	if err := topicRepo.ReplaceMentions(ctx, stageBWorkspace, request); err != nil {
		t.Fatalf("repeat replacement: %v", err)
	}
	var mentions, spans int
	if err := db.Pool.QueryRow(ctx, `
        SELECT (SELECT count(*) FROM topic_mentions),
               (SELECT count(*) FROM topic_mention_spans)`).Scan(&mentions, &spans); err != nil {
		t.Fatal(err)
	}
	if mentions != 1 || spans != 1 {
		t.Fatalf("stored %d mentions and %d spans, want one each", mentions, spans)
	}
	request.Mentions = nil
	if err := topicRepo.ReplaceMentions(ctx, stageBWorkspace, request); err != nil {
		t.Fatalf("empty replacement: %v", err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM topic_mentions`).Scan(&mentions); err != nil || mentions != 0 {
		t.Fatalf("empty replacement leaves %d mentions: %v", mentions, err)
	}
	if err := topicRepo.Finalize(ctx, stageBWorkspace, firstID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if err := topicRepo.ReplaceMentions(ctx, stageBWorkspace, request); !errors.Is(err, topicgraph.ErrNotBuilding) {
		t.Fatalf("replace after finalization = %v, want ErrNotBuilding", err)
	}
	if err := topicRepo.Promote(ctx, stageBWorkspace, firstID); err != nil {
		t.Fatalf("initial promotion: %v", err)
	}

	secondID := "22222222-2222-5222-8222-222222222222"
	if err := topicRepo.CreateBuilding(ctx, stageBWorkspace, topicSpec(secondID)); err != nil {
		t.Fatal(err)
	}
	if err := topicRepo.Finalize(ctx, stageBWorkspace, secondID); err != nil {
		t.Fatal(err)
	}
	if err := topicRepo.Promote(ctx, stageBWorkspace, secondID); err != nil {
		t.Fatalf("replacement promotion: %v", err)
	}
	var active, retired string
	if err := db.Pool.QueryRow(ctx, `
        SELECT status FROM topic_graph_versions WHERE version_id = $1`, secondID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `
        SELECT status FROM topic_graph_versions WHERE version_id = $1`, firstID).Scan(&retired); err != nil {
		t.Fatal(err)
	}
	if active != string(topicgraph.StatusActive) || retired != string(topicgraph.StatusRetired) {
		t.Fatalf("statuses second=%s first=%s, want ACTIVE/RETIRED", active, retired)
	}

	// An extracted child cannot become a message-topic source merely because a
	// malformed writer attached email metadata to it.
	childID := domain.NewDocID()
	if _, err := db.Pool.Exec(ctx, `
        INSERT INTO documents (doc_id, parent_doc_id, workspace_id,
                               processing_status, doc_type, normalized_text, raw_sha256)
        VALUES ($1, $2, $3, 'COMPLETED', 'email', $4, $5)`,
		childID, docID, stageBWorkspace, text, strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := emailRepo.SaveEmailMessage(ctx, message(childID, "child@mail.example.test", "Child", "")); err != nil {
		t.Fatal(err)
	}
	thirdID := "33333333-3333-5333-8333-333333333333"
	if err := topicRepo.CreateBuilding(ctx, stageBWorkspace, topicSpec(thirdID)); err != nil {
		t.Fatal(err)
	}
	childRequest := topicgraph.ReplaceRequest{
		VersionID: thirdID, TargetDocIDs: []string{childID}, Mentions: []topicgraph.Mention{topicMention(childID, text)},
	}
	if err := topicRepo.ReplaceMentions(ctx, stageBWorkspace, childRequest); !errors.Is(err, topicgraph.ErrInvalidRequest) {
		t.Fatalf("child source replacement = %v, want ErrInvalidRequest", err)
	}
}

func TestTopicGraphRelationsAndEpisodes(t *testing.T) {
	db, _ := scratch(t, "topicrelations")
	ctx := context.Background()
	if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
		t.Fatal(err)
	}
	repo := NewTopicGraphRepo(db)
	emailRepo := NewEmailRepo(db)
	versionID := "44444444-4444-5444-8444-444444444444"
	if err := repo.CreateBuilding(ctx, stageBWorkspace, topicSpec(versionID)); err != nil {
		t.Fatal(err)
	}

	text := "café topic"
	docIDs := []string{
		document(t, db, strings.Repeat("1", 64)),
		document(t, db, strings.Repeat("2", 64)),
		document(t, db, strings.Repeat("3", 64)),
	}
	mentions := make([]topicgraph.Mention, 0, len(docIDs))
	for i, docID := range docIDs {
		if _, err := db.Pool.Exec(ctx, `UPDATE documents SET normalized_text = $1 WHERE doc_id = $2`, text, docID); err != nil {
			t.Fatal(err)
		}
		m := message(docID, fmt.Sprintf("topic-%d@mail.example.test", i), "Topic", "")
		m.SentAt = time.Date(2026, 1, 7+i, 8, 12, 30, 0, time.UTC)
		if _, err := emailRepo.SaveEmailMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		mentions = append(mentions, topicMention(docID, text))
		if err := repo.ReplaceMentions(ctx, stageBWorkspace, topicgraph.ReplaceRequest{
			VersionID: versionID, TargetDocIDs: []string{docID}, Mentions: []topicgraph.Mention{mentions[i]},
		}); err != nil {
			t.Fatal(err)
		}
	}
	mentionIDs := []string{
		topicgraph.MentionID(versionID, mentions[0]),
		topicgraph.MentionID(versionID, mentions[1]),
		topicgraph.MentionID(versionID, mentions[2]),
	}
	request := topicgraph.ReplaceRelationCandidatesRequest{VersionID: versionID, Candidates: []topicgraph.RelationCandidate{
		{EarlierMentionID: mentionIDs[0], LaterMentionID: mentionIDs[1], Type: topicgraph.RelationAddresses,
			Confidence: .9, SupportingMentionIDs: []string{mentionIDs[0], mentionIDs[1]}, Method: "exact-reference", MethodVersion: "v1", Supported: true},
		{EarlierMentionID: mentionIDs[1], LaterMentionID: mentionIDs[2], Type: topicgraph.RelationPossiblyRelated,
			Confidence: .2, SupportingMentionIDs: []string{mentionIDs[1], mentionIDs[2]}, Method: "exact-reference", MethodVersion: "v1", Supported: false},
	}}
	if err := repo.ReplaceRelationCandidates(ctx, stageBWorkspace, request); err != nil {
		t.Fatalf("persist relations: %v", err)
	}
	var candidates, edges, episodes, memberships, supports int
	if err := db.Pool.QueryRow(ctx, `
        SELECT (SELECT count(*) FROM topic_relation_candidates),
               (SELECT count(*) FROM topic_relation_edges),
               (SELECT count(*) FROM topic_episodes),
               (SELECT count(*) FROM topic_episode_memberships),
               (SELECT count(*) FROM topic_relation_candidate_supports)`).
		Scan(&candidates, &edges, &episodes, &memberships, &supports); err != nil {
		t.Fatal(err)
	}
	if candidates != 2 || edges != 1 || episodes != 1 || memberships != 2 || supports != 4 {
		t.Fatalf("candidates=%d edges=%d episodes=%d memberships=%d supports=%d, want 2/1/1/2/4", candidates, edges, episodes, memberships, supports)
	}

	// Reversing the exact sent_at order is rejected before it can clear the
	// existing component or create a backward edge.
	request.Candidates = []topicgraph.RelationCandidate{request.Candidates[0]}
	request.Candidates[0].EarlierMentionID, request.Candidates[0].LaterMentionID = mentionIDs[1], mentionIDs[0]
	if err := repo.ReplaceRelationCandidates(ctx, stageBWorkspace, request); !errors.Is(err, topicgraph.ErrRelationChronology) {
		t.Fatalf("backward chronology = %v, want ErrRelationChronology", err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM topic_relation_edges`).Scan(&edges); err != nil || edges != 1 {
		t.Fatalf("failed replacement changed edges=%d: %v", edges, err)
	}
}

// TestTopicTimelineRead exercises the bounded transport-independent timeline
// service against the real version, mention, edge, and episode tables. It is
// manual because ApplySchema needs a disposable PostgreSQL/pgvector database;
// all messages and source text below are synthetic.
func TestTopicTimelineRead(t *testing.T) {
	ctx := context.Background()
	db, _ := scratch(t, "topictimeline")
	if err := db.ApplySchema(ctx, stageBMeta()); err != nil {
		t.Fatal(err)
	}
	repo := NewTopicGraphRepo(db)
	emails := NewEmailRepo(db)
	versionID := "55555555-5555-5555-8555-555555555555"
	if err := repo.CreateBuilding(ctx, stageBWorkspace, topicSpec(versionID)); err != nil {
		t.Fatal(err)
	}

	texts := []string{"café raised", "café addressed", "café resolved"}
	docIDs := []string{document(t, db, strings.Repeat("a", 64)), document(t, db, strings.Repeat("b", 64)), document(t, db, strings.Repeat("c", 64))}
	mentions := make([]topicgraph.Mention, len(docIDs))
	for i, docID := range docIDs {
		if _, err := db.Pool.Exec(ctx, `UPDATE documents SET normalized_text = $1 WHERE doc_id = $2`, texts[i], docID); err != nil {
			t.Fatal(err)
		}
		m := message(docID, fmt.Sprintf("timeline-%d@mail.example.test", i), "Timeline", "")
		m.SentAt = time.Date(2026, 2, 1+i, 0, 0, 0, 0, time.UTC)
		if _, err := emails.SaveEmailMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		mentions[i] = topicMention(docID, texts[i])
		if err := repo.ReplaceMentions(ctx, stageBWorkspace, topicgraph.ReplaceRequest{VersionID: versionID, TargetDocIDs: []string{docID}, Mentions: []topicgraph.Mention{mentions[i]}}); err != nil {
			t.Fatal(err)
		}
	}
	ids := []string{topicgraph.MentionID(versionID, mentions[0]), topicgraph.MentionID(versionID, mentions[1]), topicgraph.MentionID(versionID, mentions[2])}
	if err := repo.ReplaceRelationCandidates(ctx, stageBWorkspace, topicgraph.ReplaceRelationCandidatesRequest{VersionID: versionID, Candidates: []topicgraph.RelationCandidate{
		{EarlierMentionID: ids[0], LaterMentionID: ids[1], Type: topicgraph.RelationAddresses, Confidence: .9, SupportingMentionIDs: []string{ids[0], ids[1]}, Method: "manual", MethodVersion: "v1", Supported: true},
		{EarlierMentionID: ids[1], LaterMentionID: ids[2], Type: topicgraph.RelationStatesResolution, Confidence: .8, SupportingMentionIDs: []string{ids[1], ids[2]}, Method: "manual", MethodVersion: "v1", Supported: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Finalize(ctx, stageBWorkspace, versionID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Promote(ctx, stageBWorkspace, versionID); err != nil {
		t.Fatal(err)
	}

	service, err := topicgraph.NewTimelineService(NewTopicTimelineStore(db), stageBWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	out, err := service.Timeline(ctx, topicgraph.TimelineRequest{Reference: topicgraph.EncodeMentionReference(versionID, ids[0]), Limits: topicgraph.TimelineLimits{ForwardDepth: 2, MaxNodes: 3, MaxBytes: 1024, MaxLatency: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	if out.GraphVersion != versionID || len(out.Nodes) != 3 || len(out.Relations) != 2 {
		t.Fatalf("timeline = %+v", out)
	}
	if out.Nodes[0].MentionRef != topicgraph.EncodeMentionReference(versionID, ids[0]) || out.Nodes[2].MentionRef != topicgraph.EncodeMentionReference(versionID, ids[2]) {
		t.Fatalf("timeline ordering = %+v", out.Nodes)
	}
	for _, node := range out.Nodes {
		if len(node.Evidence) == 0 || node.Evidence[0].DocumentRef == "" {
			t.Fatalf("node lacks canonical source range: %+v", node)
		}
	}
}
