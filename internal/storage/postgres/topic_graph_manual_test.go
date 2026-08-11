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
			for _, table := range []string{"topic_graph_versions", "topic_mentions", "topic_mention_spans"} {
				if !relationExists(t, db, schema, table) {
					t.Errorf("table %s missing", table)
				}
			}
			for _, index := range []string{"topic_graph_versions_one_active_idx", "topic_mentions_source_idx", "topic_mention_spans_source_idx"} {
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
	childID := domain.NewDocID(stageBWorkspace, "stage-b", strings.Repeat("f", 64))
	if _, err := db.Pool.Exec(ctx, `
        INSERT INTO documents (doc_id, parent_doc_id, workspace_id, collection_id,
                               processing_status, doc_type, normalized_text)
        VALUES ($1, $2, $3, 'stage-b', 'COMPLETED', 'email', $4)`,
		childID, docID, stageBWorkspace, text); err != nil {
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
