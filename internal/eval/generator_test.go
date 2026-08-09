package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixtureIDFromFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"report.pdf", "report"},
		{"data.csv", "data"},
		{"archive.tar.gz", "archive.tar"},
		{"noext", "noext"},
		{"", "unnamed"},
		{".hidden", ".hidden"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := fixtureIDFromFilename(tt.input)
			if got != tt.want {
				t.Errorf("fixtureIDFromFilename(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeCategory(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"exact-identifier", CatExactIdentifier},
		{"Exact-Identifier", CatExactIdentifier},
		{"exact_identifier", CatExactIdentifier},
		{"paraphrase", CatParaphrase},
		{"multi-topic", CatMultiTopic},
		{"multi_topic", CatMultiTopic},
		{"thread", CatThread},
		{"bilingual", CatBilingual},
		{"off-domain", CatOffDomain},
		{"off_domain", CatOffDomain},
		{"attachment", CatAttachment},
		{"unknown", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeCategory(tt.input)
			if got != tt.want {
				t.Errorf("normalizeCategory(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseLLMQuestions(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantN   int
		wantErr bool
	}{
		{
			name: "clean JSON",
			raw:  `{"questions": [{"question": "What is X?", "category": "paraphrase", "fixture_ids": ["doc-a"]}]}`,
			wantN: 1,
		},
		{
			name: "JSON with markdown fences",
			raw:  "```json\n{\"questions\": [{\"question\": \"What is X?\", \"category\": \"exact-identifier\", \"fixture_ids\": [\"doc-a\"]}]}\n```",
			wantN: 1,
		},
		{
			name: "JSON array fallback",
			raw:  `[{"question": "What is Y?", "category": "multi-topic", "fixture_ids": ["doc-a", "doc-b"]}]`,
			wantN: 1,
		},
		{
			name:    "invalid JSON",
			raw:     "not json at all",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLLMQuestions(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseLLMQuestions() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && len(got) != tt.wantN {
				t.Errorf("parseLLMQuestions() returned %d questions, want %d", len(got), tt.wantN)
			}
		})
	}
}

func TestCasePath(t *testing.T) {
	got := CasePath("my-workspace")
	want := filepath.Join("workspaces", "evaluation", "my-workspace", "cases.json")
	if got != want {
		t.Errorf("CasePath(%q) = %q, want %q", "my-workspace", got, want)
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input string
		n     int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel…"},
		{"hello", 5, "hello"},
		{"", 5, ""},
		{"日本語テスト", 3, "日本語…"},
		{"日本語テスト", 4, "日本語テ…"},
	}
	for _, tt := range tests {
		got := truncate(tt.input, tt.n)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
		}
	}
}

func TestGeneratorProducesValidCaseSet(t *testing.T) {
	// This tests the output structure without an LLM or DB.
	// We build a CaseSet directly and validate it, confirming the types work.
	cs := CaseSet{
		Version: CaseSchemaVersion,
		SetID:   "workspace-test-ws",
		Cases: []Case{
			{
				ID:       "gen-0001",
				Category: CatExactIdentifier,
				Question: "What is the deployment target?",
				ExpectedSources: []ExpectedSource{
					{FixtureID: "deployment-guide", Grade: 3},
				},
			},
			{
				ID:       "gen-0002",
				Category: CatParaphrase,
				Question: "How does the system handle PDFs?",
				ExpectedSources: []ExpectedSource{
					{FixtureID: "pdf-guide", Grade: 3},
				},
			},
			{
				ID:       "gen-0003",
				Category: CatMultiTopic,
				Question: "What are the security measures and deployment steps?",
				ExpectedSources: []ExpectedSource{
					{FixtureID: "security-audit", Grade: 2},
					{FixtureID: "deployment-guide", Grade: 2},
				},
			},
		},
	}

	if err := ValidateCaseSet(&cs); err != nil {
		t.Fatalf("ValidateCaseSet() error = %v", err)
	}

	// Write and reload.
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(cs); err != nil {
		t.Fatal(err)
	}
	f.Close()

	loaded, err := LoadCaseSet(path)
	if err != nil {
		t.Fatalf("LoadCaseSet() error = %v", err)
	}
	if len(loaded.Cases) != 3 {
		t.Errorf("loaded %d cases, want 3", len(loaded.Cases))
	}
}
