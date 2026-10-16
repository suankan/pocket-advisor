package workspace

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// Ignore is one workspace's answer to "which staged files are not documents?".
//
// A staging directory is rarely all corpus. Pointing a workspace at a mail
// client's profile brings its index databases along with the mail; pointing it
// at a working folder brings build output along with the writing. Those files
// are not failures to be classified — discovery would dutifully classify them —
// they are simply not what the workspace is about, and the person who chose the
// directory is the only one who can say so.
//
// The supported glob syntax is:
//
//	"*"      zero or more characters, stopping at a slash
//	"**"     zero or more whole path segments
//	"?"      exactly one character
//	"[abc]"  one character from the set
//	"!"      re-includes something an earlier rule ignored
//
// A pattern with no slash in it is matched against the file's name at any
// depth, which is what makes "*.msf" mean every .msf anywhere rather than only
// those sitting at the top. A pattern containing a slash is matched against
// the whole path relative to the configured directory it was found under.
//
// A pattern's suffix — everything from its first dot on, such as ".fb2" in
// "*.fb2" or ".fb2.zip" in "*.fb2.zip" — matches without regard to case, so
// "*.epub" also covers Book.EPUB: file systems and mail clients write the
// same extension in either case, and nobody means to ignore one spelling.
// What comes before the dot stays case-sensitive.
//
// Order decides: the last rule that matches a path wins, so a negation only
// undoes the rules written above it.
type Ignore struct {
	rules []ignoreRule
}

type ignoreRule struct {
	// segs is the pattern split on "/", or a single element when the pattern
	// matches a bare name.
	segs []string
	// name is set when the pattern carried no slash and therefore applies to
	// the file's own name at any depth.
	name      bool
	negate    bool
	directory bool
}

// CompileIgnore validates a workspace's patterns and prepares them for
// matching. An empty list ignores nothing.
//
// Patterns are checked here rather than at the first file they fail on: a
// typo in a registry is a startup mistake, and finding out about it after a
// walk has already uploaded half a mail profile is finding out too late.
func CompileIgnore(patterns []string) (Ignore, error) {
	var ig Ignore
	for i, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" {
			return Ignore{}, fmt.Errorf("ignore entry %d is empty", i+1)
		}

		var rule ignoreRule
		if rule.negate = strings.HasPrefix(p, "!"); rule.negate {
			p = strings.TrimSpace(strings.TrimPrefix(p, "!"))
			if p == "" {
				return Ignore{}, fmt.Errorf("ignore entry %d negates nothing", i+1)
			}
		}

		p = filepath.ToSlash(p)
		rule.directory = strings.HasSuffix(p, "/")
		p = strings.TrimSuffix(p, "/")
		if p == "" {
			return Ignore{}, fmt.Errorf("ignore entry %d has no pattern", i+1)
		}

		rule.name = !strings.Contains(p, "/")
		rule.segs = strings.Split(p, "/")
		for _, seg := range rule.segs {
			if seg == "**" {
				continue
			}
			// path.Match reports a malformed pattern — an unclosed [ , say —
			// independently of what it is matched against.
			if _, err := path.Match(seg, ""); err != nil {
				return Ignore{}, fmt.Errorf("ignore entry %d (%q): %w", i+1, raw, err)
			}
		}
		ig.rules = append(ig.rules, rule)
	}
	return ig, nil
}

// Empty reports whether this workspace ignores nothing, which lets callers
// skip the walk's per-file work entirely.
func (ig Ignore) Empty() bool { return len(ig.rules) == 0 }

// Match reports whether rel — a path relative to the directory it was found
// under — is excluded from the corpus.
func (ig Ignore) Match(rel string) bool {
	if len(ig.rules) == 0 {
		return false
	}
	segs := strings.Split(strings.TrimPrefix(filepath.ToSlash(rel), "./"), "/")
	base := segs[len(segs)-1]

	ignored := false
	for _, r := range ig.rules {
		// Later rules override earlier ones, so a rule that agrees with the
		// verdict so far still has to be evaluated to know it agrees.
		if r.matches(segs, base) {
			ignored = !r.negate
		}
	}
	return ignored
}

func (r ignoreRule) matches(segs []string, base string) bool {
	if r.directory {
		// Only ancestors are directories; the final segment is a file. A
		// bare directory name matches at any depth, while a path is anchored.
		for i := 1; i < len(segs); i++ {
			if r.name {
				if matchSegment(r.segs[0], segs[i-1]) {
					return true
				}
			} else if matchSegments(r.segs, segs[:i]) {
				return true
			}
		}
		return false
	}
	if r.name {
		return matchSegment(r.segs[0], base)
	}
	return matchSegments(r.segs, segs)
}

// matchSegment matches one pattern segment against one name, with the
// pattern's suffix from its first dot on compared case-insensitively. The name
// is tried at each of its dots, so "*.fb2.zip" finds Book.FB2.ZIP and a stem
// pattern can still span dots of its own.
func matchSegment(pat, name string) bool {
	if ok, err := path.Match(pat, name); err == nil && ok {
		return true
	}
	dot := strings.IndexByte(pat, '.')
	if dot < 0 {
		return false
	}
	stem, suffix := pat[:dot], strings.ToLower(pat[dot:])
	for i := 0; i < len(name); i++ {
		if name[i] != '.' {
			continue
		}
		if ok, _ := path.Match(stem, name[:i]); !ok {
			continue
		}
		if ok, _ := path.Match(suffix, strings.ToLower(name[i:])); ok {
			return true
		}
	}
	return false
}

// matchSegments matches a slash-split pattern against a slash-split path,
// where "**" stands for any run of segments including none.
func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// A trailing "**" is the "everything inside this" form, so it wants
			// something left to be inside of: "caches/**" covers the files under
			// caches, not a file that is itself named caches. In the middle of a
			// pattern it still spans nothing, which is what lets "**/logs/*.txt"
			// find logs/a.txt at the top as well as src/logs/b.txt below it.
			if len(pat) == 1 {
				return len(name) > 0
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if !matchSegment(pat[0], name[0]) {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}
