package postgres

import "sort"

// Deterministic maintenance of the email identifier graph (ingestion-design.md
// §2.5). The decision is kept here, separate from the SQL that carries it out,
// because it is the part that has to be right under out-of-order arrival,
// missing ancestors and duplicate identifiers — conditions that are awkward to
// stage in a database and trivial to enumerate in a table test.

// identifierNode is one email_identifier_nodes row.
//
// DocID is empty for a placeholder: an identifier named by a reply whose own
// message was never ingested, or has not been ingested yet. A placeholder is a
// node in the graph and nothing more — no document row is created to stand
// behind it.
type identifierNode struct {
	MessageID   string
	DocID       string
	ComponentID string
}

// componentPlan is the deterministic outcome of folding one message's
// identifier set into the graph: the component it lands in, the components that
// have to be rewritten onto it, and the node writes that follow.
type componentPlan struct {
	// ComponentID is the surviving component, which is the conversation.
	ComponentID string
	// MergeFrom are the components being folded into ComponentID, ascending.
	MergeFrom []string
	// Insert are nodes that do not exist yet, ordered by identifier.
	Insert []identifierNode
	// AdoptOwn reports that this document now owns a placeholder that was
	// standing in for its own Message-ID.
	AdoptOwn bool
	// DuplicateOf names the document that already owns this Message-ID, empty
	// when there is none.
	DuplicateOf string
}

// planComponent decides where a message belongs in the identifier graph.
//
// ids is the message's identifier set — its own Message-ID when present, then
// every In-Reply-To and References entry — and existing holds whichever of
// those identifiers the workspace has already seen. seed is the component to
// create when none of them have.
//
// The rule is a union: every identifier a message mentions is, by RFC 5322,
// part of the same conversation, so their components become one. The survivor
// is the lexicographically smallest component id, which is what makes the
// result independent of arrival order — the same set of messages produces the
// same conversation whether the reply or the original landed first. Nothing
// here can loop: the identifier set is deduplicated, so a message that lists
// its own identifier among its references contributes one node, not an edge
// back to itself, and merging is over a finite set of components already
// present in the graph.
//
// A Message-ID another document already owns is never taken from it. The first
// writer keeps the identifier and the second is reported as a duplicate: the
// alternative is moving an existing conversation onto whichever copy happened
// to be ingested last.
func planComponent(docID, messageID string, ids []string, existing map[string]identifierNode, seed string) componentPlan {
	plan := componentPlan{ComponentID: seed}

	seen := make(map[string]struct{}, len(ids))
	var components []string
	for _, id := range ids {
		n, ok := existing[id]
		if !ok || n.ComponentID == "" {
			continue
		}
		if _, dup := seen[n.ComponentID]; dup {
			continue
		}
		seen[n.ComponentID] = struct{}{}
		components = append(components, n.ComponentID)
	}
	if len(components) > 0 {
		sort.Strings(components)
		plan.ComponentID = components[0]
		plan.MergeFrom = components[1:]
	}

	for _, id := range ids {
		n, ok := existing[id]
		if !ok {
			node := identifierNode{MessageID: id, ComponentID: plan.ComponentID}
			if id == messageID {
				node.DocID = docID
			}
			plan.Insert = append(plan.Insert, node)
			continue
		}
		if id != messageID {
			continue
		}
		switch {
		case n.DocID == "":
			plan.AdoptOwn = true
		case n.DocID != docID:
			plan.DuplicateOf = n.DocID
		}
	}
	sort.Slice(plan.Insert, func(i, j int) bool {
		return plan.Insert[i].MessageID < plan.Insert[j].MessageID
	})
	return plan
}

// smallestIdentifier is the seed identifier a new component is derived from.
// Taking the smallest rather than the message's own id means two messages of
// one conversation seed the same component whichever arrives first, so an
// out-of-order ingest converges without needing a merge at all.
func smallestIdentifier(ids []string) string {
	var min string
	for _, id := range ids {
		if min == "" || id < min {
			min = id
		}
	}
	return min
}
