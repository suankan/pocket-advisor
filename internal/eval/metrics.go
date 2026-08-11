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
func DocumentRecallAtK(packets []packetRef, expected []ExpectedDocument, k int) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	if k <= 0 {
		k = len(packets)
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		expectedSet[e.DocumentID] = struct{}{}
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
func ReciprocalRankFirstExpectedDocument(packets []packetRef, expected []ExpectedDocument) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		expectedSet[e.DocumentID] = struct{}{}
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
func NDCG(packets []packetRef, expected []ExpectedDocument) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	expectedGrades := make(map[string]int, len(expected))
	for _, document := range expected {
		expectedGrades[document.DocumentID] = document.Grade
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

// ForbiddenHits counts packets whose document ID appears in the forbidden set.
func ForbiddenHits(packets []packetRef, forbidden []string) int {
	if len(forbidden) == 0 {
		return 0
	}
	forbiddenSet := make(map[string]struct{}, len(forbidden))
	for _, f := range forbidden {
		forbiddenSet[f] = struct{}{}
	}
	n := 0
	for _, p := range packets {
		if _, ok := forbiddenSet[p.DocID]; ok {
			n++
		}
	}
	return n
}

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
type packetRef struct {
	DocID string
	Score float64
}
