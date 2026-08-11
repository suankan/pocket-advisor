package postgres

import (
	"reflect"
	"testing"
)

// Component maintenance is the part of email persistence that has to be right
// regardless of the order messages arrive in, so it is exercised as pure
// decisions over an in-memory graph rather than through a database.

const (
	compA = "11111111-1111-5111-8111-111111111111"
	compB = "22222222-2222-5222-8222-222222222222"
	compC = "33333333-3333-5333-8333-333333333333"
	seedX = "99999999-9999-5999-8999-999999999999"

	docOne = "aaaaaaaa-0000-5000-8000-000000000001"
	docTwo = "aaaaaaaa-0000-5000-8000-000000000002"
)

func TestPlanComponentSeedsANewConversation(t *testing.T) {
	plan := planComponent(docOne, "a@x", []string{"a@x"}, map[string]identifierNode{}, seedX)

	if plan.ComponentID != seedX {
		t.Errorf("component = %q, want the seed %q", plan.ComponentID, seedX)
	}
	if len(plan.MergeFrom) != 0 {
		t.Errorf("merged %v with nothing present", plan.MergeFrom)
	}
	want := []identifierNode{{MessageID: "a@x", DocID: docOne, ComponentID: seedX}}
	if !reflect.DeepEqual(plan.Insert, want) {
		t.Errorf("insert = %+v, want %+v", plan.Insert, want)
	}
	if plan.AdoptOwn || plan.DuplicateOf != "" {
		t.Errorf("unexpected adoption/duplicate: %+v", plan)
	}
}

// A reply naming an ancestor nobody has ingested must still form one
// conversation, with the ancestor present as a placeholder and no document
// invented for it.
func TestPlanComponentPlaceholdersAbsentAncestors(t *testing.T) {
	plan := planComponent(docOne, "b@x", []string{"b@x", "a@x"}, map[string]identifierNode{}, seedX)

	want := []identifierNode{
		{MessageID: "a@x", DocID: "", ComponentID: seedX},
		{MessageID: "b@x", DocID: docOne, ComponentID: seedX},
	}
	if !reflect.DeepEqual(plan.Insert, want) {
		t.Fatalf("insert = %+v, want %+v", plan.Insert, want)
	}
}

// The ancestor arriving second must land in the component its reply already
// created, and take over the placeholder standing in for it.
func TestPlanComponentAdoptsItsOwnPlaceholder(t *testing.T) {
	existing := map[string]identifierNode{
		"a@x": {MessageID: "a@x", ComponentID: compA},
		"b@x": {MessageID: "b@x", DocID: docTwo, ComponentID: compA},
	}
	plan := planComponent(docOne, "a@x", []string{"a@x"}, existing, seedX)

	if plan.ComponentID != compA {
		t.Errorf("component = %q, want the existing %q", plan.ComponentID, compA)
	}
	if !plan.AdoptOwn {
		t.Error("placeholder for the message's own id was not adopted")
	}
	if len(plan.Insert) != 0 {
		t.Errorf("inserted %+v for identifiers already present", plan.Insert)
	}
	if plan.DuplicateOf != "" {
		t.Errorf("placeholder reported as a duplicate of %q", plan.DuplicateOf)
	}
}

// Two conversations that turn out to be one: the survivor is the smallest
// component id, whatever order the merge is discovered in.
func TestPlanComponentMergesOntoTheSmallestComponent(t *testing.T) {
	existing := map[string]identifierNode{
		"c@x": {MessageID: "c@x", DocID: docTwo, ComponentID: compC},
		"b@x": {MessageID: "b@x", ComponentID: compB},
		"a@x": {MessageID: "a@x", DocID: docOne, ComponentID: compA},
	}
	plan := planComponent(docOne, "d@x", []string{"d@x", "c@x", "b@x", "a@x"}, existing, seedX)

	if plan.ComponentID != compA {
		t.Errorf("survivor = %q, want the smallest %q", plan.ComponentID, compA)
	}
	if want := []string{compB, compC}; !reflect.DeepEqual(plan.MergeFrom, want) {
		t.Errorf("merge from %v, want %v", plan.MergeFrom, want)
	}
	want := []identifierNode{{MessageID: "d@x", DocID: docOne, ComponentID: compA}}
	if !reflect.DeepEqual(plan.Insert, want) {
		t.Errorf("insert = %+v, want %+v", plan.Insert, want)
	}
}

// The seed is never preferred to a component that already exists: introducing
// a fresh id would rename a conversation on every ingest.
func TestPlanComponentPrefersAnExistingComponentToItsSeed(t *testing.T) {
	existing := map[string]identifierNode{
		"z@x": {MessageID: "z@x", DocID: docTwo, ComponentID: "ffffffff-ffff-5fff-8fff-ffffffffffff"},
	}
	plan := planComponent(docOne, "a@x", []string{"a@x", "z@x"}, existing, seedX)

	if plan.ComponentID != "ffffffff-ffff-5fff-8fff-ffffffffffff" {
		t.Errorf("component = %q, want the existing one even though the seed sorts lower",
			plan.ComponentID)
	}
}

// Merging is over the set of components, not the number of identifiers naming
// them, so a message referencing five ids in one conversation merges nothing.
func TestPlanComponentDeduplicatesComponents(t *testing.T) {
	existing := map[string]identifierNode{
		"a@x": {MessageID: "a@x", DocID: docOne, ComponentID: compA},
		"b@x": {MessageID: "b@x", DocID: docTwo, ComponentID: compA},
		"c@x": {MessageID: "c@x", ComponentID: compA},
	}
	plan := planComponent(docOne, "", []string{"a@x", "b@x", "c@x"}, existing, seedX)

	if plan.ComponentID != compA {
		t.Errorf("component = %q, want %q", plan.ComponentID, compA)
	}
	if len(plan.MergeFrom) != 0 {
		t.Errorf("merge from %v, want nothing: one component was named three times", plan.MergeFrom)
	}
}

// A second document claiming an identifier the first already owns is reported,
// not honoured: the node keeps its first writer.
func TestPlanComponentKeepsTheFirstOwnerOfADuplicateIdentifier(t *testing.T) {
	existing := map[string]identifierNode{
		"a@x": {MessageID: "a@x", DocID: docOne, ComponentID: compA},
	}
	plan := planComponent(docTwo, "a@x", []string{"a@x"}, existing, seedX)

	if plan.DuplicateOf != docOne {
		t.Errorf("duplicate of %q, want %q", plan.DuplicateOf, docOne)
	}
	if plan.AdoptOwn {
		t.Error("an owned identifier was retargeted at the second document")
	}
	if plan.ComponentID != compA {
		t.Errorf("component = %q, want %q: a duplicate still joins the conversation",
			plan.ComponentID, compA)
	}
}

// Re-ingesting the same message must decide nothing at all: same component, no
// inserts, no adoption, no duplicate.
func TestPlanComponentIsAnoOpOnReIngestion(t *testing.T) {
	existing := map[string]identifierNode{
		"a@x": {MessageID: "a@x", DocID: docOne, ComponentID: compA},
		"r@x": {MessageID: "r@x", ComponentID: compA},
	}
	plan := planComponent(docOne, "a@x", []string{"a@x", "r@x"}, existing, seedX)

	if plan.ComponentID != compA || len(plan.MergeFrom) != 0 || len(plan.Insert) != 0 {
		t.Errorf("re-ingestion changed the graph: %+v", plan)
	}
	if plan.AdoptOwn || plan.DuplicateOf != "" {
		t.Errorf("re-ingestion reported a change: %+v", plan)
	}
}

// A message listing its own identifier among its references contributes one
// node, which is why the union cannot loop back on itself.
func TestPlanComponentToleratesASelfReference(t *testing.T) {
	plan := planComponent(docOne, "a@x", []string{"a@x"}, map[string]identifierNode{}, seedX)
	if len(plan.Insert) != 1 {
		t.Errorf("insert = %+v, want exactly one node", plan.Insert)
	}
}

// The seed identifier is the smallest, so two messages of one conversation
// derive the same component whichever of them is ingested first.
func TestSmallestIdentifierIsOrderIndependent(t *testing.T) {
	forward := smallestIdentifier([]string{"b@x", "a@x", "c@x"})
	reverse := smallestIdentifier([]string{"c@x", "b@x", "a@x"})
	if forward != "a@x" || reverse != "a@x" {
		t.Errorf("seed identifiers disagree: %q and %q", forward, reverse)
	}
	if smallestIdentifier(nil) != "" {
		t.Error("an empty identifier set must not produce a seed")
	}
}
