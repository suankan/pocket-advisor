package retrieval

import (
	"context"
	"strings"
	"unicode/utf8"
)

const snippetBytes = 240

// buildPackets turns selected matches into the deliverable: each matched
// document with its provenance, its text, and what it is part of.
//
// Fill is breadth-first across packets rather than depth-first per packet.
// Every packet gets its matched text before any packet gets a neighbour, so a
// single long thread cannot consume the allowance and leave other matched
// documents with nothing (§5.3).
func (s *Service) buildPackets(ctx context.Context, picked []scored, subQueries []string) ([]Packet, *budgeter, error) {
	budget := newBudgeter(s.cfg.AnswerContextBytes)
	if len(picked) == 0 {
		return nil, budget, nil
	}

	ids := make([]string, 0, len(picked))
	for _, p := range picked {
		ids = append(ids, p.DocID)
	}
	docs, texts, err := s.loadDocuments(ctx, ids)
	if err != nil {
		return nil, nil, err
	}

	packets := make([]Packet, 0, len(picked))
	for _, p := range picked {
		doc, ok := docs[p.DocID]
		if !ok {
			continue
		}
		legs := "dense"
		switch {
		case p.DenseRank > 0 && p.LexRank > 0:
			legs = "both"
		case p.DenseRank == 0:
			legs = "lexical"
		}
		sq := ""
		if len(subQueries) > 1 && p.SubQuery < len(subQueries) {
			sq = subQueries[p.SubQuery]
		}
		packets = append(packets, Packet{
			Document: doc,
			Match: Match{
				ChunkID: p.ChunkID, StartByte: p.StartByte, EndByte: p.EndByte,
				Score: p.Score, Legs: legs, SubQuery: sq,
				Snippet: snippet(p.Text),
			},
		})
	}

	// Level 1 — matched documents in full, across every packet first.
	for i := range packets {
		if text, ok := budget.take(texts[packets[i].DocID]); ok {
			packets[i].Text = text
		}
	}

	// Levels 2-4 — parents, attachments, then thread chronology. Omitted
	// neighbours keep their identity and citation so a reader can pull them
	// manually; only the text is dropped.
	neighbours, err := s.loadNeighbours(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	if len(neighbours) == 0 {
		return packets, budget, nil
	}

	matched := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		matched[id] = struct{}{}
	}

	byOrder := []Relation{RelationParent, RelationChild, RelationThreadPeer}
	attached := map[string]struct{}{}
	for _, rel := range byOrder {
		for _, n := range neighbours {
			if n.Relation != rel {
				continue
			}
			if _, isMatched := matched[n.DocID]; isMatched {
				continue // already returned as a packet in its own right
			}
			if _, dup := attached[n.DocID]; dup {
				continue
			}
			idx := ownerOf(packets, n)
			if idx < 0 {
				continue
			}
			attached[n.DocID] = struct{}{}
			r := n.Related
			if text, ok := budget.take(n.text); ok {
				r.Text = text
			}
			packets[idx].Related = append(packets[idx].Related, r)
		}
	}
	return packets, budget, nil
}

// ownerOf attaches a neighbour to exactly one packet — the first it relates
// to. A thread's chronology is therefore carried once and referenced, not
// repeated for every packet sharing that thread.
func ownerOf(packets []Packet, n neighbour) int {
	for i, p := range packets {
		switch n.Relation {
		case RelationParent:
			if p.ParentID == n.DocID {
				return i
			}
		case RelationChild:
			if n.ParentID == p.DocID {
				return i
			}
		case RelationThreadPeer:
			if p.ThreadID != "" && p.ThreadID == n.ThreadID {
				return i
			}
		}
	}
	return -1
}

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= snippetBytes {
		return s
	}
	end := snippetBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	cut := s[:end]
	if i := strings.LastIndex(cut, " "); i > snippetBytes/2 {
		cut = cut[:i]
	}
	return cut + "…"
}
