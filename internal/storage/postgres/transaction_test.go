package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsDataExceptionOnlyBlamesTheValues(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&pgconn.PgError{Code: "22021"}, true}, // invalid byte sequence, NUL in text
		{fmt.Errorf("save text: %w", &pgconn.PgError{Code: "22P05"}), true},
		{&pgconn.PgError{Code: "40P01"}, false},
		{&pgconn.PgError{Code: "08006"}, false},
		{context.Canceled, false},
		{errors.New("connection reset"), false},
	} {
		if got := IsDataException(tc.err); got != tc.want {
			t.Errorf("IsDataException(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
