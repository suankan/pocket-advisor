package mailbox

import (
	"context"
	"errors"
	"testing"

	"github.com/suankan/pocket-advisor/internal/domain"
)

func ownerService(t *testing.T, store Store) *Service {
	t.Helper()
	s, err := NewWithOwnerIdentities(store, []string{"Owner <OWNER@EXAMPLE.TEST>"}, Config{DefaultLimit: 5, MaxCandidates: 5, MaxLimit: 5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestListMessagesFiltersDirectionAgainstOwners(t *testing.T) {
	store := newFakeStore()
	inbound := synthetic(1, 1, "sender@example.test", at(1, 9))
	outbound := synthetic(2, 2, "owner@example.test", at(2, 9))
	ownerToOwner := synthetic(3, 3, "owner@example.test", at(3, 9))
	store.ingest(inbound)
	store.ingest(outbound)
	store.ingest(ownerToOwner)
	s := ownerService(t, store)
	in, err := s.ListMessages(context.Background(), ListRequest{Direction: DirectionInbound})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Messages) != 1 || in.Messages[0].DocID != inbound.DocID {
		t.Errorf("inbound = %#v", in.Messages)
	}
	out, err := s.ListMessages(context.Background(), ListRequest{Direction: DirectionOutbound})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 2 {
		t.Errorf("outbound = %#v", out.Messages)
	}
	_, err = testService(t, store).ListMessages(context.Background(), ListRequest{Direction: DirectionInbound})
	if !errors.Is(err, ErrOwnerIdentitiesRequired) {
		t.Errorf("missing owners error = %v", err)
	}
}

func TestAwaitingReplyCandidatesAreExactAndEvidenceBacked(t *testing.T) {
	store := newFakeStore()
	// This candidate has a later automatic event. It remains a candidate, and
	// the event is retained with its stored class rather than called a reply.
	inbound := synthetic(1, 1, "sender@example.test", at(1, 9))
	inbound.MessageID = "inbound@example.test"
	auto := synthetic(2, 1, "mailer@example.test", at(2, 9))
	auto.MessageID = "automatic@example.test"
	auto.AutomatedClass = domain.EmailAutomatedAutoSubmitted
	auto.References = []domain.EmailReference{{Kind: domain.EmailReferenceInReplyTo, MessageID: inbound.MessageID}}
	// Same subject in a subject fallback is deliberately not candidate proof.
	subjectOnly := synthetic(3, 2, "other@example.test", at(4, 9))
	subjectOnly.ConversationMethod = domain.ConversationBySubject
	store.ingest(inbound)
	store.ingest(auto)
	store.ingest(subjectOnly)

	result, err := ownerService(t, store).AwaitingReplyCandidates(context.Background(), AwaitingReplyRequest{Participant: "SENDER@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("candidates = %#v", result.Candidates)
	}
	candidate := result.Candidates[0]
	if candidate.LatestInbound.Ref == "" || candidate.LatestInbound.DocID != inbound.DocID || candidate.LatestInbound.Relationship.Method != RelationshipRoot {
		t.Errorf("inbound evidence = %#v", candidate.LatestInbound)
	}
	if len(candidate.LaterEvents) != 1 || candidate.LaterEvents[0].AutomatedClass != string(domain.EmailAutomatedAutoSubmitted) {
		t.Errorf("later events = %#v", candidate.LaterEvents)
	}
	if candidate.ConversationMethod != string(domain.ConversationByReferences) || len(candidate.Participants) != 3 {
		t.Errorf("candidate = %#v", candidate)
	}
}

func TestAwaitingReplyCandidateDisappearsOnlyForLinkedOwnerReply(t *testing.T) {
	store := newFakeStore()
	inbound := synthetic(1, 1, "sender@example.test", at(1, 9))
	inbound.MessageID = "question@example.test"
	unlinkedOwner := synthetic(2, 1, "owner@example.test", at(2, 9))
	unlinkedOwner.MessageID = "different-branch@example.test"
	// Keep it in the same exact component through a separate root reference;
	// it is not a descendant of the latest inbound message.
	unlinkedOwner.References = []domain.EmailReference{{Kind: domain.EmailReferenceReferences, MessageID: "missing-root@example.test"}}
	// The third message has the direct exact reply link and is what closes it.
	linkedOwner := synthetic(3, 1, "owner@example.test", at(3, 9))
	linkedOwner.References = []domain.EmailReference{{Kind: domain.EmailReferenceInReplyTo, MessageID: inbound.MessageID}}
	store.ingest(inbound)
	store.ingest(unlinkedOwner)
	s := ownerService(t, store)
	before, err := s.AwaitingReplyCandidates(context.Background(), AwaitingReplyRequest{})
	if err != nil || len(before.Candidates) != 1 {
		t.Fatalf("before = %#v, %v", before, err)
	}
	store.ingest(linkedOwner)
	after, err := s.AwaitingReplyCandidates(context.Background(), AwaitingReplyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Candidates) != 0 {
		t.Errorf("linked owner reply did not close candidate: %#v", after.Candidates)
	}
}
