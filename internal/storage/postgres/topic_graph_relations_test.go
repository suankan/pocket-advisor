package postgres

import (
	"testing"
	"time"
)

func TestTopicRelationChronologyUsesSentAtThenDocID(t *testing.T) {
	at := time.Date(2026, 1, 7, 8, 0, 0, 0, time.UTC)
	later := at.Add(time.Minute)
	if !chronologicallyBefore(topicRelationMention{docID: "b", sentAt: &at}, topicRelationMention{docID: "a", sentAt: &later}) {
		t.Fatal("earlier sent_at was not ordered before later sent_at")
	}
	if !chronologicallyBefore(topicRelationMention{docID: "a", sentAt: &at}, topicRelationMention{docID: "b", sentAt: &at}) {
		t.Fatal("equal sent_at did not use ascending doc_id tie-breaker")
	}
	if chronologicallyBefore(topicRelationMention{docID: "b", sentAt: &at}, topicRelationMention{docID: "a", sentAt: &at}) {
		t.Fatal("descending doc_id tie-breaker was accepted")
	}
	if !chronologicallyBefore(topicRelationMention{docID: "z", sentAt: &at}, topicRelationMention{docID: "a"}) {
		t.Fatal("dated message did not sort before undated message")
	}
	if chronologicallyBefore(topicRelationMention{docID: "a"}, topicRelationMention{docID: "z", sentAt: &at}) {
		t.Fatal("undated message sorted before dated message")
	}
}

func TestTopicComponentsOnlyContainSupportedEdgeEndpoints(t *testing.T) {
	components := newTopicComponents()
	components.union("mention-a", "mention-b")
	components.union("mention-b", "mention-c")
	got := components.groups()
	if len(got) != 1 || len(got[0]) != 3 || got[0][0] != "mention-a" || got[0][2] != "mention-c" {
		t.Fatalf("groups = %#v, want one sorted supported component", got)
	}
	if len(newTopicComponents().groups()) != 0 {
		t.Fatal("unsupported or disconnected mentions must not create episodes")
	}
}
