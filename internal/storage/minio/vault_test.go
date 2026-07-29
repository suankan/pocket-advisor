package minio

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A control character in a header value is rejected by net/http outright, so
// the alias separator has to be something a filename can never smuggle one
// into. Joining on \x1f meant an object with two aliases could never have its
// metadata written again — every later upload run failed on it permanently.
func TestAliasEncodingSurvivesHTTPHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	aliases := []string{
		"Re_ Про твою поездку - John Doe.eml",
		"plain.pdf",
		"tab\tand\x1fcontrol.txt",
		`quotes "and" backslash\.eml`,
	}

	encoded := encodeAliases(aliases)

	req, err := http.NewRequest("PUT", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Amz-Meta-Alias-Filenames", encoded)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("encoded alias list is not a legal header value: %v", err)
	}
	resp.Body.Close()
}

func TestAliasEncodingRoundTrips(t *testing.T) {
	aliases := []string{"Про поездку.eml", "second name.pdf", "with\x1fseparator.txt"}

	got := decodeAliases(encodeAliases(aliases))
	if len(got) != len(aliases) {
		t.Fatalf("round trip produced %d aliases, want %d: %#v", len(got), len(aliases), got)
	}
	for i := range aliases {
		if got[i] != aliases[i] {
			t.Errorf("alias %d = %q, want %q", i, got[i], aliases[i])
		}
	}
}

// Objects written before the JSON change carry \x1f-joined values. Re-reading
// them must not lose names, or the first upload run after the fix would drop
// provenance that is not recoverable from anywhere else.
func TestAliasDecodingReadsLegacySeparator(t *testing.T) {
	got := decodeAliases("first.eml\x1fsecond.eml")
	if len(got) != 2 || got[0] != "first.eml" || got[1] != "second.eml" {
		t.Fatalf("legacy alias list decoded to %#v", got)
	}
	if got := decodeAliases(""); got != nil {
		t.Errorf("empty alias list decoded to %#v, want nil", got)
	}
}

// minio-go RFC 2047-encodes non-ASCII metadata on write and does not decode it
// on read. Without decoding here, a Cyrillic filename never compares equal to
// the name it was stored under, so the uploader treats every non-ASCII document
// as renamed and records a duplicate alias on every single run.
func TestProvenanceDecodesRFC2047Metadata(t *testing.T) {
	const want = "Re_ Про встречу в пятницу - John Doe (john@example.com) - 2026-01-19 1817.eml"
	stored := "=?UTF-8?b?UmVfINCf0YDQviDQstGB0YLRgNC10YfRgyDQsiDQv9GP0YLQvdC40YbRgyAt?= " +
		"=?UTF-8?b?IEpvaG4gRG9lIChqb2huQGV4YW1wbGUuY29tKSAtIDIwMjYtMDEtMTkgMTgx?= " +
		"=?UTF-8?b?Ny5lbWw=?="

	p := provenanceFrom(map[string]string{"X-Amz-Meta-Source-Filename": stored})
	if p.SourceFilename != want {
		t.Errorf("SourceFilename = %q,\n                want %q", p.SourceFilename, want)
	}
}

func TestProvenanceLeavesPlainValuesAlone(t *testing.T) {
	p := provenanceFrom(map[string]string{
		"X-Amz-Meta-Source-Filename": "statement.pdf",
		"X-Amz-Meta-Collection-Id":   "test-correspondence",
	})
	if p.SourceFilename != "statement.pdf" {
		t.Errorf("SourceFilename = %q", p.SourceFilename)
	}
	if p.CollectionID != "test-correspondence" {
		t.Errorf("CollectionID = %q", p.CollectionID)
	}
}
