package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/suankan/pocket-advisor/internal/domain"
)

// Non-UTF-8 text is refused before the driver sees it.
//
// PostgreSQL rejects such a value itself, but only as SQLSTATE 22021 naming a
// byte offset. Nothing upstream could classify that: the runtime retried it
// three times and dead-lettered it as UNCLASSIFIED, and on the admission path
// the error escaped the scan transaction and ended a 61k-file walk partway
// through. A typed refusal is what lets each caller decide, once.
//
// The repository carries no pool here deliberately — reaching the query would
// dereference nil, so the test proves the check runs before any database work.
func TestSaveTextRefusesInvalidUTF8BeforeTheDatabase(t *testing.T) {
	r := &DocumentRepo{}
	// The byte sequence that ended a real run: ISO-8859-1 "ñ × " in a file the
	// sniffer routed as text.
	bad := string([]byte{0xf1, 0x20, 0xd7, 0x20})

	err := r.SaveText(context.Background(), "doc-1", bad, "text")
	if !errors.Is(err, ErrInvalidText) {
		t.Fatalf("SaveText error = %v, want ErrInvalidText", err)
	}
	if !strings.Contains(err.Error(), "doc-1") {
		t.Errorf("error %q does not name the document it refused", err)
	}

	err = r.SaveEmailText(context.Background(), "doc-2", bad, domain.EmailHeaders{})
	if !errors.Is(err, ErrInvalidText) {
		t.Fatalf("SaveEmailText error = %v, want ErrInvalidText", err)
	}
}

// Valid text must reach the database, or the guard above would be a silent
// content filter rather than an encoding check. Reaching the query with no
// pool panics, which is exactly the signal this test wants.
func TestSaveTextAcceptsValidUTF8(t *testing.T) {
	r := &DocumentRepo{}
	defer func() {
		if recover() == nil {
			t.Fatal("valid UTF-8 was refused before the database instead of being written")
		}
	}()
	_ = r.SaveText(context.Background(), "doc-3", "ñ × still perfectly valid UTF-8", "text")
}
