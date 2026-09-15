package eval

import (
	"math"
	"sort"
	"strings"
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
// Text is the evidence the run actually admitted for this packet, including
// the related-document text delivered alongside it, and is empty exactly when
// the shared budget dropped it. Carrying it is what lets a fact assertion see
// the difference between "ranked this document" and "returned its text".
type packetRef struct {
	DocID string
	Score float64
	Text  string
}

// collapseSpace normalises whitespace runs to single spaces.
//
// The same rule that defines shared passage identity, deliberately: extracted
// PDF text is full of layout spacing that carries no meaning, and a fact
// assertion that broke whenever a column shifted would be measuring the
// layout renderer rather than retrieval.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

func factMatches(haystack string, fact ExpectedFact) bool {
	span := collapseSpace(fact.Span)
	if span == "" {
		return false
	}
	if fact.CaseSensitive {
		return strings.Contains(haystack, span)
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(span))
}

// FactRecall is the fraction of asserted facts present in the admitted
// evidence. Unlike document recall it is unaffected by which document
// supplied the text, because a caller composing an answer does not care.
func FactRecall(packets []packetRef, facts []ExpectedFact) float64 {
	if len(facts) == 0 {
		return 1.0
	}
	texts := make([]string, 0, len(packets))
	for _, p := range packets {
		if p.Text != "" {
			texts = append(texts, collapseSpace(p.Text))
		}
	}
	found := 0
	for _, fact := range facts {
		for _, t := range texts {
			if factMatches(t, fact) {
				found++
				break
			}
		}
	}
	return float64(found) / float64(len(facts))
}

// FirstFactRank returns the 1-based position of the first packet whose
// admitted text grounds any asserted fact, or 0 when none does.
//
// The same observation ReciprocalRankFirstFact carries, kept as a raw
// position because the two are read for different purposes: a reciprocal
// averages sensibly across a suite, while a position is what tells a person
// looking at one case how far down the answer actually was. Both are
// reported; neither gates anything.
func FirstFactRank(packets []packetRef, facts []ExpectedFact) int {
	if len(facts) == 0 {
		return 0
	}
	for i, p := range packets {
		if p.Text == "" {
			continue
		}
		text := collapseSpace(p.Text)
		for _, fact := range facts {
			if factMatches(text, fact) {
				return i + 1
			}
		}
	}
	return 0
}

// FirstExpectedDocumentRank returns the 1-based position of the first
// expected document, or 0 when none was returned.
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

// ReciprocalRankFirstFact returns 1/(rank of the first packet whose admitted
// text grounds any asserted fact), or 0 when none does. It is the identity-free
// counterpart of ReciprocalRankFirstExpectedDocument: same question, asked of
// the evidence rather than of the row it came from.
func ReciprocalRankFirstFact(packets []packetRef, facts []ExpectedFact) float64 {
	if len(facts) == 0 {
		return 1.0
	}
	for i, p := range packets {
		if p.Text == "" {
			continue
		}
		text := collapseSpace(p.Text)
		for _, fact := range facts {
			if factMatches(text, fact) {
				return 1.0 / float64(i+1)
			}
		}
	}
	return 0.0
}
