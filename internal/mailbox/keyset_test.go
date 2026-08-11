package mailbox

import (
	"testing"
	"time"
)

func TestBrowseOrdersAreExactReverses(t *testing.T) {
	msgs := []Message{
		synthetic(1, 1, "ada@example.test", at(2, 9)),
		synthetic(2, 1, "ada@example.test", at(2, 9)),
		synthetic(3, 1, "ada@example.test", at(1, 9)),
		synthetic(4, 1, "ada@example.test", time.Time{}),
		synthetic(5, 1, "ada@example.test", time.Time{}),
	}
	newest := append([]Message(nil), msgs...)
	oldest := append([]Message(nil), msgs...)
	sortByOrder(newest, OrderNewestFirst)
	sortByOrder(oldest, OrderOldestFirst)

	for i := range newest {
		if newest[i].DocID != oldest[len(oldest)-1-i].DocID {
			t.Fatalf("newest[%d] = %s, oldest reverse = %s", i, newest[i].DocID, oldest[len(oldest)-1-i].DocID)
		}
	}
}

func TestAfterKeyPartitionsBothOrders(t *testing.T) {
	msgs := []Message{
		synthetic(1, 1, "ada@example.test", at(3, 9)),
		synthetic(2, 1, "ada@example.test", at(3, 9)),
		synthetic(3, 1, "ada@example.test", at(2, 9)),
		synthetic(4, 1, "ada@example.test", time.Time{}),
		synthetic(5, 1, "ada@example.test", time.Time{}),
	}
	for _, order := range []Order{OrderNewestFirst, OrderOldestFirst} {
		t.Run(string(order), func(t *testing.T) {
			ordered := append([]Message(nil), msgs...)
			sortByOrder(ordered, order)
			for i, boundary := range ordered {
				for j, candidate := range ordered {
					got := order.afterKey(boundary.key(), candidate.key())
					want := j > i
					if got != want {
						t.Errorf("after index %d candidate %d = %t, want %t", i, j, got, want)
					}
				}
			}
		})
	}
}

func TestChronologicalPutsUndatedMessagesLast(t *testing.T) {
	msgs := []Message{
		synthetic(2, 1, "ada@example.test", time.Time{}),
		synthetic(3, 1, "ada@example.test", at(3, 9)),
		synthetic(1, 1, "ada@example.test", at(3, 9)),
		synthetic(4, 1, "ada@example.test", at(1, 9)),
	}
	chronological(msgs)
	want := []string{docID(4), docID(1), docID(3), docID(2)}
	for i, id := range want {
		if msgs[i].DocID != id {
			t.Errorf("message %d = %s, want %s", i, msgs[i].DocID, id)
		}
	}
}

func TestKeysetPredicateCoversNullBoundaries(t *testing.T) {
	newestDated := keysetPredicate(OrderNewestFirst, key{SentAt: at(2, 1), DocID: docID(1)}, "$2", "$1")
	if newestDated != "sent_at IS NULL OR sent_at < $2 OR (sent_at = $2 AND doc_id < $1)" {
		t.Errorf("newest dated = %s", newestDated)
	}
	newestUndated := keysetPredicate(OrderNewestFirst, key{DocID: docID(1)}, "", "$1")
	if newestUndated != "sent_at IS NULL AND doc_id < $1" {
		t.Errorf("newest undated = %s", newestUndated)
	}
	oldestDated := keysetPredicate(OrderOldestFirst, key{SentAt: at(2, 1), DocID: docID(1)}, "$2", "$1")
	if oldestDated != "sent_at IS NOT NULL AND (sent_at > $2 OR (sent_at = $2 AND doc_id > $1))" {
		t.Errorf("oldest dated = %s", oldestDated)
	}
	oldestUndated := keysetPredicate(OrderOldestFirst, key{DocID: docID(1)}, "", "$1")
	if oldestUndated != "(sent_at IS NULL AND doc_id > $1) OR sent_at IS NOT NULL" {
		t.Errorf("oldest undated = %s", oldestUndated)
	}
}
