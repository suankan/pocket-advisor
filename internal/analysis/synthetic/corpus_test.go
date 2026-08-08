package synthetic

import (
	"strings"
	"testing"
)

func TestBuildCorpus(t *testing.T) {
	corpus := BuildCorpus()
	if len(corpus) == 0 {
		t.Fatal("corpus is empty")
	}

	// Check that all threads have IDs and messages.
	for _, thread := range corpus {
		if thread.ID == "" {
			t.Error("thread has empty ID")
		}
		if len(thread.Messages) == 0 {
			t.Errorf("thread %s has no messages", thread.ID)
		}
		for _, msg := range thread.Messages {
			if msg.ID == "" {
				t.Error("message has empty ID")
			}
			if msg.From.Address == "" {
				t.Error("message has empty from address")
			}
			if msg.Body == "" {
				t.Error("message has empty body")
			}
		}
	}
}

func TestBuildCorpusFixtureIDs(t *testing.T) {
	corpus := BuildCorpus()
	ids := make(map[string]bool)
	for _, thread := range corpus {
		for _, msg := range thread.Messages {
			fid := FixtureID(msg)
			if fid == "" {
				t.Error("empty fixture ID")
			}
			if strings.HasPrefix(fid, "synth-") == false {
				t.Errorf("fixture ID %q does not start with synth-", fid)
			}
			if ids[fid] {
				t.Errorf("duplicate fixture ID %q", fid)
			}
			ids[fid] = true
		}
	}
}

func TestBuildOutboxFixtures(t *testing.T) {
	fixtures := BuildOutboxFixtures()
	if len(fixtures) == 0 {
		t.Fatal("outbox fixtures are empty")
	}

	expected := map[string]string{
		"thread-outbox-action":   "likely_action_required",
		"thread-outbox-noaction": "likely_no_action_required",
		"thread-outbox-uncertain": "uncertain",
	}

	for _, f := range fixtures {
		if f.ConversationID == "" {
			t.Error("fixture has empty conversation ID")
		}
		want, ok := expected[f.ConversationID]
		if !ok {
			t.Errorf("unexpected fixture: %s", f.ConversationID)
			continue
		}
		if f.ExpectedClass != want {
			t.Errorf("fixture %s: expected class %s, got %s", f.ConversationID, want, f.ExpectedClass)
		}
		if len(f.Messages) == 0 {
			t.Errorf("fixture %s has no messages", f.ConversationID)
		}
	}
}

func TestMultiPageParticipantRequest(t *testing.T) {
	req := MultiPageParticipantRequest()
	if req == nil {
		t.Fatal("nil request")
	}
	q, ok := req["question"].(string)
	if !ok || q == "" {
		t.Error("missing or empty question")
	}
	parts, ok := req["participants"].([]string)
	if !ok || len(parts) != 3 {
		t.Error("expected 3 participants")
	}
}

func TestBudgetThread(t *testing.T) {
	thread := buildBudgetThread()
	if thread.ID != "thread-budget-review" {
		t.Errorf("unexpected thread ID: %s", thread.ID)
	}
	if len(thread.Messages) != 4 {
		t.Errorf("expected 4 messages, got %d", len(thread.Messages))
	}

	// Verify thread IDs are consistent.
	for _, msg := range thread.Messages {
		if msg.ThreadID != thread.ID {
			t.Errorf("message %s has thread ID %s, want %s", msg.ID, msg.ThreadID, thread.ID)
		}
	}
}

func TestContradictionThread(t *testing.T) {
	thread := buildContradictionThread()
	if len(thread.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(thread.Messages))
	}

	// Both messages should be from Alice.
	for _, msg := range thread.Messages {
		if msg.From.Address != "alice@example.test" {
			t.Errorf("expected message from alice@example.test, got %s", msg.From.Address)
		}
	}

	// Messages should contain contradictory statements.
	if strings.Contains(thread.Messages[0].Body, "three days") == false {
		t.Error("first message should mention three days")
	}
	if strings.Contains(thread.Messages[1].Body, "two days") == false {
		t.Error("second message should mention two days")
	}
}

func TestLongThread(t *testing.T) {
	thread := buildLongThread()
	if len(thread.Messages) != 25 {
		t.Errorf("expected 25 messages, got %d", len(thread.Messages))
	}

	// Messages should alternate between Alice and Bob.
	for i, msg := range thread.Messages {
		if i%2 == 0 && msg.From.Address != "alice@example.test" {
			t.Errorf("message %d: expected alice, got %s", i, msg.From.Address)
		}
		if i%2 == 1 && msg.From.Address != "bob@example.test" {
			t.Errorf("message %d: expected bob, got %s", i, msg.From.Address)
		}
	}
}

func TestParticipantAddresses(t *testing.T) {
	if Alice.Address != "alice@example.test" {
		t.Errorf("unexpected Alice address: %s", Alice.Address)
	}
	if Bob.Address != "bob@example.test" {
		t.Errorf("unexpected Bob address: %s", Bob.Address)
	}
	if Carol.Address != "carol@example.test" {
		t.Errorf("unexpected Carol address: %s", Carol.Address)
	}
	if Dave.Address != "dave@example.test" {
		t.Errorf("unexpected Dave address: %s", Dave.Address)
	}
}

func TestSparseParticipantThread(t *testing.T) {
	thread := buildSparseParticipantThread()
	if len(thread.Messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(thread.Messages))
	}
	msg := thread.Messages[0]
	if msg.HasQuestion {
		t.Error("expected no question in sparse thread")
	}
}
