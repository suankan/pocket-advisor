package eval

import (
	"math"
	"sort"
)

// DocumentRecallAtK computes the fraction of expected documents found among
// the top-k selected packets. A document counts once no matter how many of
// its chunks appear in the top-k — this is document recall, not packet
// recall, so a document split across several selected packets must not
// inflate the result past 1.0.
func DocumentRecallAtK(packets []packetRef, expected []ResolvedDocument, k int) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	if k <= 0 {
		k = len(packets)
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		expectedSet[e.DocID] = struct{}{}
	}
	found := make(map[string]struct{}, len(expected))
	for i, p := range packets {
		if i >= k {
			break
		}
		if _, ok := expectedSet[p.DocID]; ok {
			found[p.DocID] = struct{}{}
		}
	}
	return float64(len(found)) / float64(len(expected))
}

// ReciprocalRankFirstExpectedDocument returns 1/(rank of the first expected
// document), or 0 if none is found. Rank is 1-based.
func ReciprocalRankFirstExpectedDocument(packets []packetRef, expected []ResolvedDocument) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		expectedSet[e.DocID] = struct{}{}
	}
	for i, p := range packets {
		if _, ok := expectedSet[p.DocID]; ok {
			return 1.0 / float64(i+1)
		}
	}
	return 0.0
}

// NDCG computes normalized discounted cumulative gain using the relevance
// grades on expected documents. Each expected document earns its gain at most
// once, on its best-ranked (first) occurrence — a document split across
// several selected packets must not earn repeat credit, or the achieved DCG
// could exceed the ideal DCG it is normalized against.
func NDCG(packets []packetRef, expected []ResolvedDocument) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	expectedGrades := make(map[string]int, len(expected))
	for _, document := range expected {
		expectedGrades[document.DocID] = document.Grade
	}

	// Build relevance vector for returned packets.
	relevances := make([]float64, len(packets))
	credited := make(map[string]struct{}, len(expected))
	for i, p := range packets {
		grade, ok := expectedGrades[p.DocID]
		if !ok {
			continue
		}
		if _, dup := credited[p.DocID]; dup {
			continue
		}
		credited[p.DocID] = struct{}{}
		relevances[i] = float64(grade)
	}

	// Ideal ranking sorts expected documents by grade descending.
	ideal := make([]float64, len(expected))
	for i, document := range expected {
		ideal[i] = float64(document.Grade)
	}
	sort.Sort(sort.Reverse(float64Slice(ideal)))

	dcgVal := dcg(relevances)
	idcg := dcg(ideal)
	if idcg == 0 {
		return 0.0
	}
	return dcgVal / idcg
}

func dcg(rels []float64) float64 {
	var sum float64
	for i, r := range rels {
		if r > 0 {
			sum += (math.Pow(2, r) - 1) / math.Log2(float64(i+2))
		}
	}
	return sum
}

type float64Slice []float64

func (s float64Slice) Len() int           { return len(s) }
func (s float64Slice) Less(i, j int) bool { return s[i] < s[j] }
func (s float64Slice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// UniqueDocIDs extracts distinct doc_ids from packet references.
func UniqueDocIDs(packets []packetRef) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, p := range packets {
		if _, dup := seen[p.DocID]; dup {
			continue
		}
		seen[p.DocID] = struct{}{}
		out = append(out, p.DocID)
	}
	return out
}

// DistributionFrom computes a Distribution from a slice of measurements.
func DistributionFrom(vals []float64) Distribution {
	if len(vals) == 0 {
		return Distribution{}
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)

	var sum float64
	for _, v := range sorted {
		sum += v
	}
	n := len(sorted)

	pctile := func(p float64) float64 {
		idx := int(math.Ceil(p*float64(n))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= n {
			idx = n - 1
		}
		return sorted[idx]
	}

	return Distribution{
		Min:  sorted[0],
		Max:  sorted[n-1],
		Mean: sum / float64(n),
		P50:  pctile(0.50),
		P95:  pctile(0.95),
		P99:  pctile(0.99),
		N:    n,
	}
}

// packetRef is a lightweight reference for metric calculation, decoupled from
// the retrieval package's Packet type. DocID is the Postgres document UUID
// carried directly by the retrieval packet.
//
// Text is this document's own admitted text and nothing else. It is empty
// exactly when the shared budget dropped it, which is what lets delivery see
// the difference between "ranked this document" and "returned its text".
//
// Related carries the neighbours delivered in the same response as separate
// identities. They are evidence the caller genuinely received, but they are
// not this document, and folding their text into Text would let a covering
// email prove that the statement attached to it arrived when the budget had
// refused it.
type packetRef struct {
	DocID   string
	Score   float64
	Text    string
	Related []relatedRef
}

// relatedRef is one neighbour delivered alongside a packet, kept under its own
// identity so supplementary delivery is measured rather than assumed.
type relatedRef struct {
	DocID string
	Text  string
}

// deliveredPrimary is the set of documents that returned their own text.
func deliveredPrimary(packets []packetRef, k int) map[string]struct{} {
	if k <= 0 {
		k = len(packets)
	}
	out := make(map[string]struct{}, len(packets))
	for i, p := range packets {
		if i >= k {
			break
		}
		if p.Text != "" {
			out[p.DocID] = struct{}{}
		}
	}
	return out
}

// deliveredRelated is the set of documents that returned their own text as a
// neighbour of some packet. A document that is also a primary packet is
// excluded, so the two sets never double-count one identity.
func deliveredRelated(packets []packetRef, k int) map[string]struct{} {
	if k <= 0 {
		k = len(packets)
	}
	primary := deliveredPrimary(packets, k)
	out := make(map[string]struct{})
	for i, p := range packets {
		if i >= k {
			break
		}
		for _, r := range p.Related {
			if r.Text == "" {
				continue
			}
			if _, isPrimary := primary[r.DocID]; isPrimary {
				continue
			}
			out[r.DocID] = struct{}{}
		}
	}
	return out
}

func expectedSetOf(expected []ResolvedDocument) map[string]struct{} {
	want := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		want[e.DocID] = struct{}{}
	}
	return want
}

func overlapFraction(have, want map[string]struct{}) float64 {
	if len(want) == 0 {
		return 1.0
	}
	found := 0
	for id := range want {
		if _, ok := have[id]; ok {
			found++
		}
	}
	return float64(found) / float64(len(want))
}

// DeliveredRecallAtK is document recall counting only expected documents that
// returned their own text.
//
// The budget admits a document's text whole or not at all, so a delivered
// document carries everything it says and an undelivered one carries nothing.
// That makes delivery, not the presence of any particular wording, the honest
// test of whether a caller could work from the result: the API's job is to
// return the documents holding the answer, and a title with an empty body does
// not do that.
//
// A neighbour's text never counts here. Those documents reach the caller and
// are measured by SupplementaryDeliveredRecallAtK, but crediting them to the
// document they were attached to would report an expected document as
// delivered on the strength of a covering email the budget happened to fit.
func DeliveredRecallAtK(packets []packetRef, expected []ResolvedDocument, k int) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	return overlapFraction(deliveredPrimary(packets, k), expectedSetOf(expected))
}

// SupplementaryDeliveredRecallAtK is the fraction of expected documents whose
// text arrived as a neighbour of some packet rather than as a packet of its
// own. It is reported beside primary delivery rather than added to it: the two
// answer different questions, and a change that moves evidence from one to the
// other is a change worth seeing.
func SupplementaryDeliveredRecallAtK(packets []packetRef, expected []ResolvedDocument, k int) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	return overlapFraction(deliveredRelated(packets, k), expectedSetOf(expected))
}

// UnionDeliveredRecallAtK is the fraction of expected documents whose text
// reached the caller by either route. Each identity counts once however many
// packets carried it.
func UnionDeliveredRecallAtK(packets []packetRef, expected []ResolvedDocument, k int) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	union := deliveredPrimary(packets, k)
	for id := range deliveredRelated(packets, k) {
		union[id] = struct{}{}
	}
	return overlapFraction(union, expectedSetOf(expected))
}

// ReciprocalRankFirstDelivered is the reciprocal rank of the first expected
// document returned with its text.
func ReciprocalRankFirstDelivered(packets []packetRef, expected []ResolvedDocument) float64 {
	if rank := FirstDeliveredRank(packets, expected); rank > 0 {
		return 1.0 / float64(rank)
	}
	if len(expected) == 0 {
		return 1.0
	}
	return 0.0
}

// FirstDeliveredRank is the 1-based position of the first expected document
// returned with its text, or 0 when none was.
func FirstDeliveredRank(packets []packetRef, expected []ResolvedDocument) int {
	if len(expected) == 0 {
		return 0
	}
	want := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		want[e.DocID] = struct{}{}
	}
	for i, p := range packets {
		if p.Text == "" {
			continue
		}
		if _, ok := want[p.DocID]; ok {
			return i + 1
		}
	}
	return 0
}

// FirstExpectedDocumentRank is the 1-based position of the first expected
// document, delivered or not, or 0 when none was returned.
func FirstExpectedDocumentRank(packets []packetRef, expected []ResolvedDocument) int {
	if len(expected) == 0 {
		return 0
	}
	want := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		want[e.DocID] = struct{}{}
	}
	for i, p := range packets {
		if _, ok := want[p.DocID]; ok {
			return i + 1
		}
	}
	return 0
}
