// Package synthetic provides a privacy-safe synthetic corpus for analysis
// evaluation. It contains deterministic test fixtures that exercise:
//   - multi-party topic requests with chronology
//   - outstanding-item review with action/no-action/uncertain classifications
//   - conflicting statements across participants
//   - coverage warnings and budget behavior
//   - collision-free evidence references
//   - continuation across multiple pages
//
// No private questions, identities, evidence, analysis output, or workspace
// details enter committed fixtures.
package synthetic

import (
	"fmt"
	"time"
)

// Participant is a synthetic email participant.
type Participant struct {
	Name    string
	Address string
}

// Synthetic participants used across all fixtures.
var (
	Alice = Participant{Name: "Alice Chen", Address: "alice@example.test"}
	Bob   = Participant{Name: "Bob Wang", Address: "bob@example.test"}
	Carol = Participant{Name: "Carol Li", Address: "carol@example.test"}
	Dave  = Participant{Name: "Dave Kim", Address: "dave@example.test"}
)

// Message is a synthetic email message.
type Message struct {
	ID        string
	From      Participant
	To        Participant
	Subject   string
	Date      time.Time
	ThreadID  string
	Body      string
	HasQuestion bool
}

// Thread is a synthetic email thread.
type Thread struct {
	ID       string
	Subject  string
	Messages []Message
}

// FixtureID returns a stable fixture identifier for synthetic evaluation.
func FixtureID(msg Message) string {
	return fmt.Sprintf("synth-%s", msg.ID)
}

// BuildCorpus creates the complete synthetic corpus for evaluation.
func BuildCorpus() []Thread {
	return []Thread{
		buildBudgetThread(),
		buildProjectTimelineThread(),
		buildOutboxActionRequired(),
		buildOutboxNoAction(),
		buildOutboxUncertain(),
		buildContradictionThread(),
		buildSparseParticipantThread(),
		buildLongThread(),
	}
}

// buildBudgetThread creates a multi-party budget discussion thread.
func buildBudgetThread() Thread {
	return Thread{
		ID:      "thread-budget-review",
		Subject: "Q2 Budget Review",
		Messages: []Message{
			{
				ID: "msg-budget-001", From: Alice, To: Bob,
				Subject: "Q2 Budget Review", Date: time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
				ThreadID: "thread-budget-review",
				Body:     "Hi Bob, I've prepared the Q2 budget review. The total allocation is $500K with $120K for engineering and $80K for marketing.",
				HasQuestion: false,
			},
			{
				ID: "msg-budget-002", From: Bob, To: Alice,
				Subject: "Re: Q2 Budget Review", Date: time.Date(2025, 3, 2, 14, 30, 0, 0, time.UTC),
				ThreadID: "thread-budget-review",
				Body:     "Thanks Alice. I think the engineering allocation should be higher given our hiring plans. Can we increase it to $150K?",
				HasQuestion: true,
			},
			{
				ID: "msg-budget-003", From: Carol, To: Alice,
				Subject: "Re: Q2 Budget Review", Date: time.Date(2025, 3, 3, 10, 0, 0, 0, time.UTC),
				ThreadID: "thread-budget-review",
				Body:     "I agree with Bob. Marketing can work with $60K if engineering gets more. The priority should be technical capacity.",
				HasQuestion: false,
			},
			{
				ID: "msg-budget-004", From: Alice, To: Bob,
				Subject: "Re: Q2 Budget Review", Date: time.Date(2025, 3, 5, 11, 0, 0, 0, time.UTC),
				ThreadID: "thread-budget-review",
				Body:     "After reviewing, I've approved $145K for engineering and $75K for marketing. Total remains $500K with remaining $280K for operations.",
				HasQuestion: false,
			},
		},
	}
}

// buildProjectTimelineThread creates a thread with timeline changes.
func buildProjectTimelineThread() Thread {
	return Thread{
		ID:      "thread-project-timeline",
		Subject: "Project Alpha Timeline",
		Messages: []Message{
			{
				ID: "msg-timeline-001", From: Dave, To: Alice,
				Subject: "Project Alpha Timeline", Date: time.Date(2025, 2, 15, 9, 0, 0, 0, time.UTC),
				ThreadID: "thread-project-timeline",
				Body:     "Project Alpha is on track for a June delivery. All milestones are being met.",
				HasQuestion: false,
			},
			{
				ID: "msg-timeline-002", From: Alice, To: Dave,
				Subject: "Re: Project Alpha Timeline", Date: time.Date(2025, 3, 10, 16, 0, 0, 0, time.UTC),
				ThreadID: "thread-project-timeline",
				Body:     "We've encountered some delays. The new estimated delivery is August. The testing phase needs two additional weeks.",
				HasQuestion: false,
			},
			{
				ID: "msg-timeline-003", From: Dave, To: Alice,
				Subject: "Re: Project Alpha Timeline", Date: time.Date(2025, 4, 1, 10, 0, 0, 0, time.UTC),
				ThreadID: "thread-project-timeline",
				Body:     "Understood. I've updated the stakeholder communication. Do we need to adjust the resource allocation as well?",
				HasQuestion: true,
			},
		},
	}
}

// buildOutboxActionRequired creates a thread requiring action.
func buildOutboxActionRequired() Thread {
	return Thread{
		ID:      "thread-outbox-action",
		Subject: "Contract Review Needed",
		Messages: []Message{
			{
				ID: "msg-outbox-action-001", From: Bob, To: Alice,
				Subject: "Contract Review Needed", Date: time.Date(2025, 4, 10, 9, 0, 0, 0, time.UTC),
				ThreadID: "thread-outbox-action",
				Body:     "Hi Alice, the vendor contract expires next week. Can you review the renewal terms and provide your recommendation by Friday?",
				HasQuestion: true,
			},
			{
				ID: "msg-outbox-action-002", From: Alice, To: Bob,
				Subject: "Re: Contract Review Needed", Date: time.Date(2025, 4, 11, 14, 0, 0, 0, time.UTC),
				ThreadID: "thread-outbox-action",
				Body:     "I'll take a look at it. Do you have the updated terms from the vendor?",
				HasQuestion: true,
			},
		},
	}
}

// buildOutboxNoAction creates a thread that needs no action.
func buildOutboxNoAction() Thread {
	return Thread{
		ID:      "thread-outbox-noaction",
		Subject: "Monthly Report Update",
		Messages: []Message{
			{
				ID: "msg-outbox-noaction-001", From: Carol, To: Alice,
				Subject: "Monthly Report Update", Date: time.Date(2025, 4, 8, 10, 0, 0, 0, time.UTC),
				ThreadID: "thread-outbox-noaction",
				Body:     "Hi Alice, just wanted to let you know that the monthly report has been uploaded to the shared drive. No action needed from your side.",
				HasQuestion: false,
			},
		},
	}
}

// buildOutboxUncertain creates a thread with ambiguous intent.
func buildOutboxUncertain() Thread {
	return Thread{
		ID:      "thread-outbox-uncertain",
		Subject: "Team Building Ideas",
		Messages: []Message{
			{
				ID: "msg-outbox-uncertain-001", From: Dave, To: Alice,
				Subject: "Team Building Ideas", Date: time.Date(2025, 4, 12, 15, 0, 0, 0, time.UTC),
				ThreadID: "thread-outbox-uncertain",
				Body:     "Hey Alice, I was thinking about organizing a team lunch next week. What do you think? Maybe Thursday?",
				HasQuestion: true,
			},
		},
	}
}

// buildContradictionThread creates a thread with contradictory statements.
func buildContradictionThread() Thread {
	return Thread{
		ID:      "thread-contradiction",
		Subject: "Policy Interpretation",
		Messages: []Message{
			{
				ID: "msg-contradiction-001", From: Alice, To: Bob,
				Subject: "Policy Interpretation", Date: time.Date(2025, 3, 20, 9, 0, 0, 0, time.UTC),
				ThreadID: "thread-contradiction",
				Body:     "Our remote work policy allows three days per week in the office as the standard arrangement.",
				HasQuestion: false,
			},
			{
				ID: "msg-contradiction-002", From: Alice, To: Bob,
				Subject: "Re: Policy Interpretation", Date: time.Date(2025, 4, 15, 11, 0, 0, 0, time.UTC),
				ThreadID: "thread-contradiction",
				Body:     "After reconsidering, I believe the remote work policy should be interpreted as two days per week maximum to ensure collaboration.",
				HasQuestion: false,
			},
		},
	}
}

// buildSparseParticipantThread creates a thread with minimal participation.
func buildSparseParticipantThread() Thread {
	return Thread{
		ID:      "thread-sparse",
		Subject: "Quick Update",
		Messages: []Message{
			{
				ID: "msg-sparse-001", From: Bob, To: Alice,
				Subject: "Quick Update", Date: time.Date(2025, 4, 5, 9, 0, 0, 0, time.UTC),
				ThreadID: "thread-sparse",
				Body:     "Just a quick note: the server migration is complete. Everything is running smoothly.",
				HasQuestion: false,
			},
		},
	}
}

// buildLongThread creates a long thread for chronological sampling tests.
func buildLongThread() Thread {
	msgs := make([]Message, 0, 25)
	for i := 0; i < 25; i++ {
		date := time.Date(2025, 1, 1+i, 9, 0, 0, 0, time.UTC)
		from := Alice
		to := Bob
		if i%2 == 1 {
			from = Bob
			to = Alice
		}
		msgs = append(msgs, Message{
			ID:        fmt.Sprintf("msg-long-%03d", i),
			From:      from,
			To:        to,
			Subject:   "Long Discussion",
			Date:      date,
			ThreadID:  "thread-long",
			Body:      fmt.Sprintf("Message %d in the long discussion thread about project coordination and resource sharing.", i),
			HasQuestion: i%5 == 0,
		})
	}
	return Thread{
		ID:       "thread-long",
		Subject:  "Long Discussion",
		Messages: msgs,
	}
}

// ConversationFixture represents a synthetic awaiting-reply candidate.
type ConversationFixture struct {
	ConversationID   string
	Subject          string
	ExpectedClass    string
	EvidenceRefs     []string
	Reasoning        string
	Messages         []Message
}

// BuildOutboxFixtures returns synthetic outbox candidates for evaluation.
func BuildOutboxFixtures() []ConversationFixture {
	return []ConversationFixture{
		{
			ConversationID: "thread-outbox-action",
			Subject:        "Contract Review Needed",
			ExpectedClass:  "likely_action_required",
			EvidenceRefs:   []string{"msg-outbox-action-001", "msg-outbox-action-002"},
			Reasoning:      "Contains a direct question requiring response about contract renewal",
			Messages:       buildOutboxActionRequired().Messages,
		},
		{
			ConversationID: "thread-outbox-noaction",
			Subject:        "Monthly Report Update",
			ExpectedClass:  "likely_no_action_required",
			EvidenceRefs:   []string{"msg-outbox-noaction-001"},
			Reasoning:      "Informational update with explicit no-action-needed statement",
			Messages:       buildOutboxNoAction().Messages,
		},
		{
			ConversationID: "thread-outbox-uncertain",
			Subject:        "Team Building Ideas",
			ExpectedClass:  "uncertain",
			EvidenceRefs:   []string{"msg-outbox-uncertain-001"},
			Reasoning:      "Casual suggestion with unclear decision requirement",
			Messages:       buildOutboxUncertain().Messages,
		},
	}
}

// MultiPageParticipantRequest returns a topic request that will produce
// multiple research passes and exceed one result page.
func MultiPageParticipantRequest() map[string]any {
	return map[string]any{
		"question":       "What are the current positions on the budget allocation?",
		"participants":   []string{"alice@example.test", "bob@example.test", "carol@example.test"},
		"evidence_budget": 250000,
	}
}
