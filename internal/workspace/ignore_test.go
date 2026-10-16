package workspace

import "testing"

// The patterns a workspace configures are the only thing standing between a
// staging directory and everything in it that is not a document. The table is
// the documented vocabulary, case by case.
func TestIgnoreMatchesTheDocumentedVocabulary(t *testing.T) {
	for name, tc := range map[string]struct {
		patterns []string
		ignored  []string
		kept     []string
	}{
		"bare name applies at any depth": {
			// The case this feature was built for: Thunderbird's Mork index
			// databases sit beside the mail, at several depths.
			patterns: []string{"*.msf"},
			ignored: []string{
				"INBOX.msf",
				"[Gmail].sbd/Drafts.msf",
				"a/b/c/Sent Mail.msf",
			},
			kept: []string{"INBOX/cur/1781.eml", "notes.txt", "msf", "x.msf.eml"},
		},
		"the suffix from the first dot ignores case": {
			patterns: []string{"*.fb2", "*.fb2.zip", "build/*.log", "msgFilterRules.dat"},
			ignored: []string{
				"Book.FB2", "a/b/Book.Fb2", "Book.FB2.ZIP", "Book.fb2.Zip",
				"build/APP.LOG", "msgFilterRules.DAT",
			},
			kept: []string{"Book.FB2.txt", "msgfilterrules.dat", "Build/app.log", "fb2"},
		},
		"a pattern with a slash is anchored to the directory": {
			patterns: []string{"build/*.log"},
			ignored:  []string{"build/app.log"},
			kept:     []string{"src/build/app.log", "build/nested/app.log", "app.log"},
		},
		"double star spans any number of segments": {
			patterns: []string{"**/logs/*.txt"},
			ignored:  []string{"logs/a.txt", "src/logs/b.txt", "a/b/logs/c.txt"},
			kept:     []string{"logs/a.md", "logs/deep/a.txt"},
		},
		"trailing double star takes a whole subtree": {
			patterns: []string{"caches/**"},
			ignored:  []string{"caches/a", "caches/deep/b/c.bin"},
			kept:     []string{"caches", "other/caches/a"},
		},
		"single character and character class": {
			patterns: []string{"log[0-9].txt", "?.tmp"},
			ignored:  []string{"log1.txt", "log5.txt", "a.tmp", "deep/7.tmp"},
			kept:     []string{"logX.txt", "log10.txt", "ab.tmp"},
		},
		"negation re-includes what an earlier rule took": {
			patterns: []string{"*.log", "!important.log"},
			ignored:  []string{"app.log", "deep/error.log"},
			kept:     []string{"important.log", "deep/important.log"},
		},
		"order decides, so a later rule wins": {
			patterns: []string{"*.log", "!keep/*.log", "keep/noisy.log"},
			ignored:  []string{"app.log", "keep/noisy.log"},
			kept:     []string{"keep/quiet.log"},
		},
		"star does not cross a slash": {
			patterns: []string{"a/*"},
			ignored:  []string{"a/b"},
			kept:     []string{"a/b/c"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			ig, err := CompileIgnore(tc.patterns)
			if err != nil {
				t.Fatalf("CompileIgnore(%v): %v", tc.patterns, err)
			}
			for _, p := range tc.ignored {
				if !ig.Match(p) {
					t.Errorf("%q should be ignored by %v", p, tc.patterns)
				}
			}
			for _, p := range tc.kept {
				if ig.Match(p) {
					t.Errorf("%q should be kept by %v", p, tc.patterns)
				}
			}
		})
	}
}

// A workspace that configures nothing must behave exactly as it did before the
// option existed.
func TestNoPatternsIgnoresNothing(t *testing.T) {
	ig, err := CompileIgnore(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ig.Empty() {
		t.Error("a workspace with no patterns reports rules")
	}
	for _, p := range []string{"anything.msf", "a/b/c.eml", ""} {
		if ig.Match(p) {
			t.Errorf("%q was ignored by an empty pattern list", p)
		}
	}
}

// The zero value is what a Resolved carries before Load fills it in, and what
// a caller that never configured anything passes around.
func TestZeroIgnoreIsUsable(t *testing.T) {
	var ig Ignore
	if !ig.Empty() {
		t.Error("zero Ignore is not empty")
	}
	if ig.Match("anything.msf") {
		t.Error("zero Ignore matched")
	}
}

// A malformed pattern is a mistake in a hand-edited registry, and the run has
// not walked anything yet. Failing at startup is the cheapest moment to say so.
func TestMalformedPatternsAreRejected(t *testing.T) {
	for name, patterns := range map[string][]string{
		"unclosed class":  {"log[0-9.txt"},
		"empty entry":     {""},
		"blank entry":     {"   "},
		"negates nothing": {"!"},
		"slash only":      {"/"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CompileIgnore(patterns); err == nil {
				t.Fatalf("expected %v to be rejected", patterns)
			}
		})
	}
}

// An error has to name the entry, because a registry lists patterns without
// numbering them and the operator has to find the one that is wrong.
func TestMalformedPatternErrorNamesTheEntry(t *testing.T) {
	_, err := CompileIgnore([]string{"*.msf", "*.tmp", "log[0-9.txt"})
	if err == nil {
		t.Fatal("expected the third entry to be rejected")
	}
	if got := err.Error(); !contains(got, "entry 3") {
		t.Errorf("error %q does not point at entry 3", got)
	}
}

func TestDirectoryPatternsMatchContentsAndAllowExceptions(t *testing.T) {
	for _, tc := range []struct {
		patterns      []string
		ignored, kept []string
	}{
		{[]string{"caches/"}, []string{"caches/a", "caches/deep/b", "src/caches/a"}, []string{"caches", "src/caches", "cache/a"}},
		{[]string{"build/caches/"}, []string{"build/caches/a", "build/caches/deep/b"}, []string{"src/build/caches/a", "build/caches"}},
		{[]string{"caches/", "!caches/keep.txt"}, []string{"caches/a", "other/caches/keep.txt"}, []string{"caches/keep.txt"}},
		{[]string{"**/cache?/"}, []string{"src/caches/a", "caches/deep/b"}, []string{"cache/a", "caches"}},
	} {
		ig, err := CompileIgnore(tc.patterns)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range tc.ignored {
			if !ig.Match(p) {
				t.Errorf("%v should ignore %q", tc.patterns, p)
			}
		}
		for _, p := range tc.kept {
			if ig.Match(p) {
				t.Errorf("%v should keep %q", tc.patterns, p)
			}
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
