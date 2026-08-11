package eval

import (
	"strings"
	"testing"
)

func TestCaseSetSHA256IdentifiesCaseContent(t *testing.T) {
	base := &CaseSet{
		Version: CaseSchemaVersion,
		SetID:   "workspace-test",
		Cases:   []Case{{ID: "case-1", Question: "first question"}},
	}

	digest := CaseSetSHA256(base)
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		t.Fatalf("digest = %q, want a lowercase SHA-256 hex digest", digest)
	}
	if repeat := CaseSetSHA256(base); repeat != digest {
		t.Errorf("same case set digest = %q, want %q", repeat, digest)
	}

	changed := *base
	changed.Cases = append([]Case(nil), base.Cases...)
	changed.Cases[0].Question = "changed question"
	if got := CaseSetSHA256(&changed); got == digest {
		t.Error("changed case content produced the same digest")
	}
}
