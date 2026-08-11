package eval

import (
	"math"
	"sort"
	"strings"
)

// SourceRecallAtK computes the fraction of expected sources found among the
// top-k selected packets. A packet matches when its doc_id or source hash
// appears in the expected set. A source counts once no matter how many of
// its chunks appear in the top-k — this is source recall, not packet
// recall, so a document split across several selected packets must not
// inflate the result past 1.0.
func SourceRecallAtK(packets []packetRef, expected []ExpectedSource, k int) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	if k <= 0 {
		k = len(packets)
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		expectedSet[e.FixtureID] = struct{}{}
	}
	found := make(map[string]struct{}, len(expected))
	for i, p := range packets {
		if i >= k {
			break
		}
		if _, ok := expectedSet[p.FixtureID]; ok {
			found[p.FixtureID] = struct{}{}
		}
	}
	return float64(len(found)) / float64(len(expected))
}

// ReciprocalRankFirst returns 1/(rank of first acceptable source), or 0 if
// none found. Rank is 1-based.
func ReciprocalRankFirst(packets []packetRef, expected []ExpectedSource) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		expectedSet[e.FixtureID] = struct{}{}
	}
	for i, p := range packets {
		if _, ok := expectedSet[p.FixtureID]; ok {
			return 1.0 / float64(i+1)
		}
	}
	return 0.0
}

// NDCG computes normalized discounted cumulative gain using relevance grades
// from the case. When no relevance grades are present, binary relevance is
// assumed (1 for expected sources, 0 otherwise). Each expected source earns
// its gain at most once, on its best-ranked (first) occurrence — a document
// split across several selected packets must not earn repeat credit, or the
// achieved DCG could exceed the ideal DCG it is normalized against.
func NDCG(packets []packetRef, expected []ExpectedSource, grades map[string]int) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		expectedSet[e.FixtureID] = struct{}{}
	}

	// Build relevance vector for returned packets.
	relevances := make([]float64, len(packets))
	credited := make(map[string]struct{}, len(expected))
	for i, p := range packets {
		if _, ok := expectedSet[p.FixtureID]; !ok {
			continue
		}
		if _, dup := credited[p.FixtureID]; dup {
			continue
		}
		credited[p.FixtureID] = struct{}{}
		if grades != nil {
			if g, ok := grades[p.FixtureID]; ok {
				relevances[i] = float64(g)
				continue
			}
		}
		relevances[i] = 1.0
	}

	// Ideal ranking: sort expected by grade descending.
	ideal := make([]float64, len(expected))
	for i, e := range expected {
		if grades != nil {
			if g, ok := grades[e.FixtureID]; ok {
				ideal[i] = float64(g)
				continue
			}
		}
		ideal[i] = 1.0
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

// TopicGroupCoverage computes the fraction of topic groups that have at least
// one matching packet.
func TopicGroupCoverage(packets []packetRef, groups []TopicGroup) float64 {
	if len(groups) == 0 {
		return 1.0
	}
	hit := 0
	for _, g := range groups {
		groupSet := make(map[string]struct{}, len(g.FixtureIDs))
		for _, fid := range g.FixtureIDs {
			groupSet[fid] = struct{}{}
		}
		for _, p := range packets {
			if _, ok := groupSet[p.FixtureID]; ok {
				hit++
				break
			}
		}
	}
	return float64(hit) / float64(len(groups))
}

// ForbiddenHits counts packets whose fixture ID appears in the forbidden set.
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
		if _, ok := forbiddenSet[p.FixtureID]; ok {
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
// the retrieval package's Packet type.
type packetRef struct {
	DocID     string
	FixtureID string // stable synthetic ID, resolved from source hash or metadata
	Score     float64
}

// ResolveFixtureID maps a retrieval packet's source hash or doc_id to a
// synthetic fixture ID using a lookup table built at evaluation setup.
func ResolveFixtureID(docID string, lookup map[string]string) string {
	if fid, ok := lookup[docID]; ok {
		return fid
	}
	return docID
}

// FixtureLookup builds a doc_id -> fixture_id mapping from case expectations
// and the workspace's document metadata.
type FixtureLookup struct {
	byDocID map[string]string
	byHash  map[string]string
}

// NewFixtureLookup creates a lookup from explicit mappings.
func NewFixtureLookup(byDocID, byHash map[string]string) *FixtureLookup {
	return &FixtureLookup{
		byDocID: byDocID,
		byHash:  byHash,
	}
}

// Resolve finds the fixture ID for a document, trying doc_id first then hash.
func (fl *FixtureLookup) Resolve(docID, hash string) string {
	if fl == nil {
		return docID
	}
	if fid, ok := fl.byDocID[docID]; ok {
		return fid
	}
	if fid, ok := fl.byHash[hash]; ok {
		return fid
	}
	return docID
}

// StringJoin is a helper for joining strings in test output.
func StringJoin(ss []string) string {
	return strings.Join(ss, ", ")
}
