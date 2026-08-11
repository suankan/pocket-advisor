package mailbox

import (
	"sort"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// Reply-edge derivation.
//
// Edges are derived when a conversation is read, not stored when a message is
// written. A reply edge is a claim about two documents, and the second one
// routinely arrives first: an edge written at ingestion time would have to be
// revisited every time an ancestor, a duplicate, or a missing parent turned up
// later. Deriving over the conversation's own messages makes the answer a pure
// function of what the workspace currently holds, which is also what makes it
// testable without a database.
//
// Scope is the conversation, and that is not a shortcut. A conversation is a
// connected component of the workspace's identifier graph (ingestion-design.md
// §2.5), so every document that could own an identifier this message names is
// already in the component by construction — including a duplicate claiming an
// identifier a sibling already owns.

// Relationship is one message's derived link to its parent.
type Relationship struct {
	Method      string `json:"method"`
	ParentDocID string `json:"parent_doc_id,omitempty"`
	ParentRef   string `json:"parent_ref,omitempty"`
}

// replyEdges derives a relationship for every message in one conversation.
//
// The rules, in order:
//
//  1. In-Reply-To naming exactly one identifier that exactly one document in
//     the workspace owns is an exact parent.
//  2. Otherwise the nearest resolvable References ancestor stands in, labelled
//     as recovery. Nearest means last in header order: References is an
//     ancestor path from the root outwards, so its final entry is the closest
//     relative that can be recovered when the parent itself is absent.
//  3. An identifier that more than one document claims, and an In-Reply-To
//     header that names more than one identifier, are ambiguous. They produce
//     a warning and never an edge — choosing between duplicates would attach
//     the reply to whichever copy happened to be ingested first, which is not
//     a fact about the mail.
//  4. An identifier nobody owns is a missing ancestor. It is counted and
//     tolerated: a conversation that reaches this workspace mid-thread is the
//     normal case, not an error.
//
// It returns the relationship per document, warnings in message order, and the
// number of distinct identifiers the conversation refers to that no document
// here owns.
func replyEdges(msgs []Message) (map[string]Relationship, []Warning, int) {
	owners := identifierOwners(msgs)
	warn := newWarnings()
	edges := make(map[string]Relationship, len(msgs))

	for _, m := range msgs {
		edges[m.DocID] = deriveEdge(m, owners, warn)
	}
	return edges, warn.all(), missingAncestors(msgs, owners)
}

// identifierOwners maps every identifier to the documents claiming it. More
// than one owner is the duplicate-Message-ID case, which is why this is a slice
// and not a single document.
func identifierOwners(msgs []Message) map[string][]string {
	owners := map[string][]string{}
	for _, m := range msgs {
		if m.MessageID == "" {
			continue
		}
		owners[m.MessageID] = append(owners[m.MessageID], m.DocID)
	}
	for id := range owners {
		sort.Strings(owners[id])
	}
	return owners
}

func deriveEdge(m Message, owners map[string][]string, warn *warnings) Relationship {
	parents := distinctIdentifiers(m, domain.EmailReferenceInReplyTo)
	ancestors := distinctIdentifiers(m, domain.EmailReferenceReferences)
	if len(parents) == 0 && len(ancestors) == 0 {
		return Relationship{Method: RelationshipRoot}
	}

	if len(parents) > 1 {
		// A conforming message names one parent. More than one means the
		// header is damaged, and no reading of it identifies a parent. Do not
		// bypass it with References: the request is for a reply edge, and an
		// ambiguous direct-parent claim cannot support one.
		warn.add(WarnAmbiguousParent, m.DocID)
		return Relationship{Method: RelationshipUnresolved}
	}
	if len(parents) == 1 {
		if parent, ok := resolve(parents[0], m.DocID, owners); ok {
			return relationshipTo(RelationshipInReplyTo, parent)
		}
		if ambiguous(parents[0], m.DocID, owners) {
			warn.add(WarnDuplicateIdentifier, m.DocID)
			return Relationship{Method: RelationshipUnresolved}
		}
	}

	// Recovery walks from the nearest ancestor outwards. A missing entry is
	// tolerated and does not make a farther, real ancestor untrue; an ambiguous
	// entry is different — it cannot safely identify any document, so this
	// message gets no edge rather than one selected around the defect.
	for i := len(ancestors) - 1; i >= 0; i-- {
		id := ancestors[i]
		if len(parents) == 1 && id == parents[0] {
			continue // the missing direct parent was already tried above
		}
		if ambiguous(id, m.DocID, owners) {
			warn.add(WarnDuplicateIdentifier, m.DocID)
			return Relationship{Method: RelationshipUnresolved}
		}
		if parent, ok := resolve(id, m.DocID, owners); ok {
			return relationshipTo(RelationshipReferencesRecovery, parent)
		}
	}
	return Relationship{Method: RelationshipUnresolved}
}

// resolve returns the single document owning id, excluding the message itself:
// a message that lists its own identifier among its references is not its own
// parent.
func resolve(id, self string, owners map[string][]string) (string, bool) {
	docs := othersOwning(id, self, owners)
	if len(docs) != 1 {
		return "", false
	}
	return docs[0], true
}

func ambiguous(id, self string, owners map[string][]string) bool {
	return len(othersOwning(id, self, owners)) > 1
}

func othersOwning(id, self string, owners map[string][]string) []string {
	var docs []string
	for _, doc := range owners[id] {
		if doc != self {
			docs = append(docs, doc)
		}
	}
	return docs
}

func relationshipTo(method, parent string) Relationship {
	return Relationship{
		Method:      method,
		ParentDocID: parent,
		ParentRef:   encodeRef(refMessage, parent),
	}
}

// distinctIdentifiers returns one header's identifiers in header order, without
// repeats. Ordinal is authoritative: References is an ordered path and the
// store may return its rows in any order.
func distinctIdentifiers(m Message, kind domain.EmailReferenceKind) []string {
	refs := make([]domain.EmailReference, 0, len(m.References))
	for _, r := range m.References {
		if r.Kind == kind && r.MessageID != "" {
			refs = append(refs, r)
		}
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].Ordinal < refs[j].Ordinal })

	seen := make(map[string]struct{}, len(refs))
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		if _, dup := seen[r.MessageID]; dup {
			continue
		}
		seen[r.MessageID] = struct{}{}
		ids = append(ids, r.MessageID)
	}
	return ids
}

// missingAncestors counts the distinct identifiers this conversation refers to
// that no document in it owns.
func missingAncestors(msgs []Message, owners map[string][]string) int {
	missing := map[string]struct{}{}
	for _, m := range msgs {
		for _, r := range m.References {
			if r.MessageID == "" {
				continue
			}
			if _, owned := owners[r.MessageID]; owned {
				continue
			}
			missing[r.MessageID] = struct{}{}
		}
	}
	return len(missing)
}
