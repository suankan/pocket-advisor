package mailbox

import (
	"testing"

	"github.com/suankan/pocket-advisor/internal/domain"
)

func withRefs(m Message, refs ...domain.EmailReference) Message {
	m.References = refs
	return m
}

func ref(kind domain.EmailReferenceKind, ordinal int, id string) domain.EmailReference {
	return domain.EmailReference{Kind: kind, Ordinal: ordinal, MessageID: id}
}

func TestReplyEdgesPreferAnExactInReplyToParent(t *testing.T) {
	parent := synthetic(1, 1, "ada@example.test", at(1, 9))
	parent.MessageID = "parent@mail.example.test"
	reply := synthetic(2, 1, "owner@example.test", at(2, 9))
	reply.MessageID = "reply@mail.example.test"
	reply = withRefs(reply,
		ref(domain.EmailReferenceInReplyTo, 0, parent.MessageID),
		ref(domain.EmailReferenceReferences, 0, "missing@mail.example.test"),
	)

	edges, warnings, missing := replyEdges([]Message{parent, reply})
	got := edges[reply.DocID]
	if got.Method != RelationshipInReplyTo || got.ParentDocID != parent.DocID {
		t.Errorf("edge = %#v, want exact parent %s", got, parent.DocID)
	}
	if len(warnings) != 0 || missing != 1 {
		t.Errorf("warnings = %#v, missing = %d", warnings, missing)
	}
	if edges[parent.DocID].Method != RelationshipRoot {
		t.Errorf("root edge = %#v", edges[parent.DocID])
	}
}

func TestReplyEdgesRecoverTheNearestResolvableReference(t *testing.T) {
	root := synthetic(1, 1, "ada@example.test", at(1, 9))
	root.MessageID = "root@mail.example.test"
	ancestor := synthetic(2, 1, "bob@example.test", at(2, 9))
	ancestor.MessageID = "ancestor@mail.example.test"
	reply := synthetic(3, 1, "owner@example.test", at(3, 9))
	reply.MessageID = "reply@mail.example.test"
	reply = withRefs(reply,
		ref(domain.EmailReferenceInReplyTo, 0, "absent-parent@mail.example.test"),
		ref(domain.EmailReferenceReferences, 0, root.MessageID),
		ref(domain.EmailReferenceReferences, 1, ancestor.MessageID),
	)

	edges, warnings, missing := replyEdges([]Message{root, ancestor, reply})
	got := edges[reply.DocID]
	if got.Method != RelationshipReferencesRecovery || got.ParentDocID != ancestor.DocID {
		t.Errorf("edge = %#v, want recovery to nearest ancestor %s", got, ancestor.DocID)
	}
	if len(warnings) != 0 || missing != 1 {
		t.Errorf("warnings = %#v, missing = %d", warnings, missing)
	}
}

func TestReplyEdgesRejectAmbiguousAndDuplicateIdentifiers(t *testing.T) {
	first := synthetic(1, 1, "ada@example.test", at(1, 9))
	first.MessageID = "duplicate@mail.example.test"
	second := synthetic(2, 1, "bob@example.test", at(2, 9))
	second.MessageID = "duplicate@mail.example.test"
	reply := synthetic(3, 1, "owner@example.test", at(3, 9))
	reply = withRefs(reply,
		ref(domain.EmailReferenceInReplyTo, 0, first.MessageID),
		ref(domain.EmailReferenceReferences, 0, first.MessageID),
	)

	edges, warnings, missing := replyEdges([]Message{first, second, reply})
	if got := edges[reply.DocID]; got.Method != RelationshipUnresolved || got.ParentDocID != "" {
		t.Errorf("edge = %#v, want no edge", got)
	}
	if missing != 0 {
		t.Errorf("missing = %d, want 0", missing)
	}
	if len(warnings) != 1 || warnings[0] != (Warning{Code: WarnDuplicateIdentifier, DocID: reply.DocID}) {
		t.Errorf("warnings = %#v", warnings)
	}
}

func TestReplyEdgesRejectMultipleInReplyToIdentifiers(t *testing.T) {
	first := synthetic(1, 1, "ada@example.test", at(1, 9))
	first.MessageID = "one@mail.example.test"
	second := synthetic(2, 1, "bob@example.test", at(2, 9))
	second.MessageID = "two@mail.example.test"
	reply := synthetic(3, 1, "owner@example.test", at(3, 9))
	reply = withRefs(reply,
		ref(domain.EmailReferenceInReplyTo, 0, first.MessageID),
		ref(domain.EmailReferenceInReplyTo, 1, second.MessageID),
	)

	edges, warnings, _ := replyEdges([]Message{first, second, reply})
	if got := edges[reply.DocID]; got.Method != RelationshipUnresolved {
		t.Errorf("edge = %#v, want unresolved", got)
	}
	if len(warnings) != 1 || warnings[0].Code != WarnAmbiguousParent {
		t.Errorf("warnings = %#v", warnings)
	}
}

func TestReplyEdgesTolerateMissingAncestors(t *testing.T) {
	reply := synthetic(1, 1, "owner@example.test", at(2, 9))
	reply = withRefs(reply,
		ref(domain.EmailReferenceInReplyTo, 0, "missing-parent@mail.example.test"),
		ref(domain.EmailReferenceReferences, 0, "missing-root@mail.example.test"),
	)
	edges, warnings, missing := replyEdges([]Message{reply})
	if got := edges[reply.DocID]; got.Method != RelationshipUnresolved {
		t.Errorf("edge = %#v", got)
	}
	if len(warnings) != 0 || missing != 2 {
		t.Errorf("warnings = %#v, missing = %d", warnings, missing)
	}
}

func TestReplyEdgesDoNotMakeSelfReferencesParents(t *testing.T) {
	m := synthetic(1, 1, "ada@example.test", at(1, 9))
	m.MessageID = "self@mail.example.test"
	m = withRefs(m, ref(domain.EmailReferenceInReplyTo, 0, m.MessageID))
	edges, _, _ := replyEdges([]Message{m})
	if got := edges[m.DocID]; got.Method != RelationshipUnresolved || got.ParentDocID != "" {
		t.Errorf("self reference = %#v", got)
	}
}
