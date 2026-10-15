package pdf

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/suankan/pocket-advisor/internal/limits"
)

// A file that will never open must say so distinctly from an engine fault, or
// every retry spends a worker on bytes that cannot change their answer. The
// encrypted fixture is four-orientations.pdf under a 256-bit user password.
func TestOpenNamesEncryptedAndDamagedFiles(t *testing.T) {
	encrypted, err := os.ReadFile("testdata/encrypted.pdf")
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(1, limits.NewCPU(2))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"encrypted", encrypted, ErrEncrypted},
		{"damaged", []byte("%PDF-1.7\nthis is not a cross-reference table\n%%EOF\n"), ErrDamaged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := e.Open(context.Background(), tc.data)
			if err == nil {
				doc.Close()
				t.Fatal("opened")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
