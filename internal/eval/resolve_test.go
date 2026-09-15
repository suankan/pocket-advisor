package eval

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubResolver struct {
	known map[string]string
	asked []string
	err   error
}

func (s *stubResolver) ResolveContentHashes(_ context.Context, hashes []string) (map[string]string, error) {
	s.asked = append(s.asked, hashes...)
	if s.err != nil {
		return nil, s.err
	}
	out := map[string]string{}
	for _, h := range hashes {
		if id, ok := s.known[h]; ok {
			out[h] = id
		}
	}
	return out, nil
}

func expectedCase(id string, hashes ...string) Case {
	c := Case{ID: id, Question: "q"}
	for _, h := range hashes {
		c.ExpectedDocuments = append(c.ExpectedDocuments, ExpectedDocument{Match: DocumentMatch{RawSHA256: h}, Grade: 3})
	}
	return c
}

func TestResolveCasesMapsContentHashesToDocumentIDs(t *testing.T) {
	r := &stubResolver{known: map[string]string{hashA: "doc-a", hashB: "doc-b"}}
	cases := []Case{expectedCase("c1", hashA), expectedCase("c2", hashA, hashB)}

	resolved, err := ResolveCases(context.Background(), r, cases)
	if err != nil {
		t.Fatalf("ResolveCases() error = %v", err)
	}
	if len(resolved) != 2 {
		t.Fatalf("resolved %d cases, want 2", len(resolved))
	}
	if got := resolved[0].Expected; len(got) != 1 || got[0].DocID != "doc-a" || got[0].Grade != 3 {
		t.Errorf("case 1 expected = %+v, want doc-a at grade 3", got)
	}
	if got := resolved[1].Expected; len(got) != 2 || got[0].DocID != "doc-a" || got[1].DocID != "doc-b" {
		t.Errorf("case 2 expected = %+v, want doc-a and doc-b in case order", got)
	}
	// One batched round trip, deduplicated: hashA is named by both cases.
	if len(r.asked) != 2 {
		t.Errorf("resolver asked for %v, want each hash once", r.asked)
	}
}

// The failure this whole scheme exists to make visible: a case naming a
// document the workspace does not hold is a broken case, and must not be
// reported as a retrieval result.
func TestResolveCasesRejectsUnresolvedDocuments(t *testing.T) {
	r := &stubResolver{known: map[string]string{hashA: "doc-a"}}
	cases := []Case{expectedCase("c1", hashA), expectedCase("c2", hashB)}

	_, err := ResolveCases(context.Background(), r, cases)
	if err == nil {
		t.Fatal("ResolveCases() error = nil, want an unresolved-document error")
	}
	var unresolved *UnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("error = %T, want *UnresolvedError so a caller can tell setup from quality", err)
	}
	if unresolved.Missing != 1 || len(unresolved.Cases) != 1 || unresolved.Cases[0] != "c2" {
		t.Errorf("unresolved = %+v, want one missing document in c2", unresolved)
	}
	// A content hash identifies private material, so it must not appear in an
	// error a developer might paste somewhere.
	if strings.Contains(err.Error(), hashB) {
		t.Error("error message leaks a content hash")
	}
}

func TestResolveCasesPropagatesResolverFailure(t *testing.T) {
	r := &stubResolver{err: errors.New("connection refused")}
	if _, err := ResolveCases(context.Background(), r, []Case{expectedCase("c1", hashA)}); err == nil {
		t.Fatal("ResolveCases() error = nil, want the resolver failure")
	}
}
