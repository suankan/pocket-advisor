package worker

import (
	"testing"

	"github.com/suankan/pocket-advisor/internal/domain"
	"github.com/suankan/pocket-advisor/internal/engine/email"
)

// Members are admitted in content-hash order whatever their order in the
// message, so every handler takes new-content stubs in one global order.
func TestAdmissionOrderSortsMembersByContentHash(t *testing.T) {
	a := []byte("synthetic member A")
	b := []byte("synthetic member B")
	c := []byte("synthetic member C")
	children := []email.Child{
		{Selector: "mime:1", Data: c},
		{Selector: "mime:2", Data: nil},
		{Selector: "mime:3", Data: a},
		{Selector: "mime:4", Data: b},
		{Selector: "mime:5", Data: a},
	}
	got := admissionOrder(children)
	if len(got) != 4 {
		t.Fatalf("admitted %d members, want 4 with the empty one skipped", len(got))
	}
	for i, m := range got {
		if m.sum != domain.SHA256Hex(m.child.Data) {
			t.Errorf("member %s carries the wrong hash", m.child.Selector)
		}
		if i > 0 && got[i-1].sum > m.sum {
			t.Errorf("member %s admitted before a smaller hash", m.child.Selector)
		}
	}
	// Identical members keep their order in the message.
	var dup []string
	for _, m := range got {
		if m.sum == domain.SHA256Hex(a) {
			dup = append(dup, m.child.Selector)
		}
	}
	if len(dup) != 2 || dup[0] != "mime:3" || dup[1] != "mime:5" {
		t.Errorf("identical members admitted as %v, want [mime:3 mime:5]", dup)
	}
}
