package email

import (
	"hash/crc32"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// Member names older than UTF-8 are bytes in whatever code page the tool that
// wrote them used, and nothing records which. A ZIP made on a Russian Windows
// desktop carries CP866; a mail client may put raw KOI8-R in a filename
// parameter. Stored as they are, those bytes are not text PostgreSQL will
// accept, and one such name used to fail its whole archive.
//
// Names are recovered in order of authority: a UTF-8 flag or Unicode copy the
// container itself provides, then a charset the message declares, and only
// then detection. Detection decodes every unflagged name of one container as
// a single sample under each candidate encoding, because one archive's names
// share one encoding and a pooled sample says far more than one short name.
// It accepts an encoding only when the result reads as names in one script
// and clearly better than any other candidate's different reading; otherwise
// the invalid bytes become U+FFFD. Unlike body text, a name is not indexed
// evidence and the original bytes are kept beside it (Child.RawName), so a
// wrong reading is cosmetic and recoverable rather than silent corruption.

type nameCandidate struct {
	label string
	enc   encoding.Encoding
	// han reports whether a Han character is in the encoding's most
	// frequent tier; nil where a right reading yields no Han at all.
	han func(rune) bool
}

// nameCandidates are the legacy encodings detection tries. Order only breaks
// ties between candidates that read a sample identically, so the most
// widespread member of each family comes first.
var nameCandidates = []nameCandidate{
	{"ibm866", charmap.CodePage866, nil},
	{"windows-1251", charmap.Windows1251, nil},
	{"koi8-r", charmap.KOI8R, nil},
	{"koi8-u", charmap.KOI8U, nil},
	{"iso-8859-5", charmap.ISO8859_5, nil},
	{"x-mac-cyrillic", charmap.MacintoshCyrillic, nil},
	{"windows-1252", charmap.Windows1252, nil},
	{"windows-1250", charmap.Windows1250, nil},
	{"ibm437", charmap.CodePage437, nil},
	{"ibm850", charmap.CodePage850, nil},
	{"ibm852", charmap.CodePage852, nil},
	{"shift_jis", japanese.ShiftJIS, jisLevel1},
	{"gbk", simplifiedchinese.GBK, gb2312Level1},
	{"big5", traditionalchinese.Big5, big5Frequent},
	// Korean names are written in Hangul; Hanja in a reading is a sign of
	// Chinese bytes, not Korean ones.
	{"euc-kr", korean.EUCKR, nil},
}

const (
	// nameAcceptScore is the least share of a sample's non-ASCII characters
	// that must sit in well-formed words of one script.
	nameAcceptScore = 0.7
	// nameMargin is how far the accepted reading must lead the best
	// candidate that reads the sample differently.
	nameMargin = 0.15
)

// resolveNames makes every child's Filename valid UTF-8 without NUL. Names
// that are not valid UTF-8 are decoded together as one sample; hint is a
// charset the container declares for its text, tried before detection.
func resolveNames(children []Child, hint string) {
	var idx []int
	var raw []string
	for i, c := range children {
		if !utf8.ValidString(c.Filename) {
			idx = append(idx, i)
			raw = append(raw, c.Filename)
		}
	}
	if len(raw) > 0 {
		names, label := decodeNames(raw, hint)
		for k, i := range idx {
			children[i].RawName = []byte(raw[k])
			children[i].NameEncoding = label
			children[i].Filename = names[k]
		}
	}
	for i := range children {
		children[i].Filename = strings.ReplaceAll(children[i].Filename, "\x00", "")
	}
}

// decodeNames decodes raw names that share one unknown encoding. It returns
// the decoded names and the encoding's label, or the names with invalid bytes
// replaced and an empty label when no encoding is clearly right.
func decodeNames(raw []string, hint string) ([]string, string) {
	if c, ok := hintCandidate(hint); ok {
		if names, ok := decodeAll(raw, c.enc); ok && nameScore(strings.Join(names, "\n"), c.han) >= nameAcceptScore {
			return names, c.label
		}
	}

	type reading struct {
		label  string
		names  []string
		joined string
		score  float64
	}
	var readings []reading
	for _, c := range nameCandidates {
		names, ok := decodeAll(raw, c.enc)
		if !ok {
			continue
		}
		joined := strings.Join(names, "\n")
		readings = append(readings, reading{c.label, names, joined, nameScore(joined, c.han)})
	}
	best := -1
	for i, r := range readings {
		if best < 0 || r.score > readings[best].score {
			best = i
		}
	}
	if best >= 0 && readings[best].score >= nameAcceptScore {
		rival := 0.0
		for _, r := range readings {
			if r.joined != readings[best].joined && r.score > rival {
				rival = r.score
			}
		}
		if readings[best].score-rival >= nameMargin {
			return readings[best].names, readings[best].label
		}
	}

	out := make([]string, len(raw))
	for i, s := range raw {
		out[i] = strings.ToValidUTF8(s, "�")
	}
	return out, ""
}

// hintCandidate resolves a declared charset to a candidate, keeping the Han
// tier of a known one.
func hintCandidate(hint string) (nameCandidate, bool) {
	// The WHATWG index reads US-ASCII as Windows-1252, which a message
	// declaring ASCII never meant for its 8-bit bytes.
	if h := strings.ToLower(strings.TrimSpace(hint)); h == "" || h == "us-ascii" || h == "ascii" {
		return nameCandidate{}, false
	}
	enc, err := htmlindex.Get(hint)
	if err != nil {
		return nameCandidate{}, false
	}
	label, _ := htmlindex.Name(enc)
	for _, c := range nameCandidates {
		if c.label == label {
			return c, true
		}
	}
	return nameCandidate{label, enc, func(r rune) bool {
		return gb2312Level1(r) || jisLevel1(r) || big5Frequent(r)
	}}, true
}

// decodeAll decodes every name, failing when any byte has no mapping: the
// decoders mark those with U+FFFD, and a name containing one is not a reading
// of the bytes at all.
func decodeAll(raw []string, enc encoding.Encoding) ([]string, bool) {
	out := make([]string, len(raw))
	for i, s := range raw {
		d, err := enc.NewDecoder().String(s)
		if err != nil || strings.ContainsRune(d, utf8.RuneError) {
			return nil, false
		}
		out[i] = d
	}
	return out, true
}

// nameScore rates how much decoded text reads like names: the share of its
// non-ASCII characters that are ordinary letters in well-formed words of the
// sample's dominant script. ASCII is ignored because every candidate decodes
// it identically.
//
// A word is a run of letters and marks. It is well formed when its letters
// share one script family, no lower-case letter is followed by a capital, a
// Latin word is not made of accented letters alone, a lone CJK character is
// not glued to Latin letters, and its Han characters are in the candidate's
// most frequent tier. Those tests separate a right reading from a wrong one:
// a wrong single-byte code page yields symbols, a mix of alphabets, inverted
// case (KOI8-R read as Windows-1251), or words of accented Latin letters
// alone (Windows-1251 read as Windows-1252); a wrong multi-byte encoding
// yields obscure characters, often swallowing a neighbouring ASCII letter.
// Rare letters (Cyrillic outside the common alphabets, unusual Hangul
// syllables) do not spoil a word but do not count for it either.
func nameScore(s string, han func(rune) bool) float64 {
	nonASCII := 0
	families := map[string]int{}
	var word []rune
	flush := func() {
		defer func() { word = word[:0] }()
		n, plain, rare := 0, 0, 0
		family := ""
		var prev rune
		ok := true
		for _, r := range word {
			if r < utf8.RuneSelf {
				if unicode.IsLetter(r) {
					plain++
				}
			} else {
				n++
				if f := scriptFamily(r); f != "" {
					if family != "" && f != family {
						ok = false
					}
					family = f
				}
				switch {
				case r >= 0xFF61 && r <= 0xFF9F: // half-width katakana
					ok = false
				case unicode.Is(unicode.Lm, r) && r != 'ー' && !unicode.Is(unicode.Han, r):
					// Spacing accents such as ˇ and ˘ are letters to Unicode,
					// but in a name they are a byte read in the wrong table.
					ok = false
				case unicode.Is(unicode.Han, r):
					if han == nil || !han(r) {
						ok = false
					}
				case rareLetter(r):
					rare++
				}
			}
			if unicode.IsUpper(r) && unicode.IsLower(prev) {
				ok = false
			}
			prev = r
		}
		if n == 0 {
			return
		}
		nonASCII += n
		switch {
		case plain > 0 && family != "" && family != "latin" && family != "cjk":
			ok = false
		case plain > 0 && family == "cjk" && n < 2:
			ok = false
		case family == "latin" && plain == 0 && n >= 3:
			ok = false
		}
		if ok {
			families[family] += n - rare
		}
	}
	for _, r := range s {
		if r == utf8.RuneError {
			return -1
		}
		if unicode.IsLetter(r) || unicode.IsMark(r) {
			word = append(word, r)
			continue
		}
		flush()
		if r >= utf8.RuneSelf {
			nonASCII++
		}
	}
	flush()
	if nonASCII == 0 {
		return 0
	}
	dominant := 0
	for _, n := range families {
		dominant = max(dominant, n)
	}
	return float64(dominant) / float64(nonASCII)
}

// scriptFamily groups scripts that legitimately share one word: Japanese
// mixes kanji and kana, Korean may mix Hangul and Hanja.
func scriptFamily(r rune) string {
	switch {
	case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Bopomofo):
		return "cjk"
	case unicode.Is(unicode.Latin, r):
		return "latin"
	case unicode.Is(unicode.Cyrillic, r):
		return "cyrillic"
	case unicode.Is(unicode.Greek, r):
		return "greek"
	case unicode.Is(unicode.Arabic, r):
		return "arabic"
	case unicode.Is(unicode.Hebrew, r):
		return "hebrew"
	case unicode.Is(unicode.Thai, r):
		return "thai"
	}
	return ""
}

// rareLetter reports letters that real names seldom use but wrong readings
// produce: Cyrillic outside the Russian, Ukrainian and Belarusian alphabets,
// and Hangul syllables with the rarest vowels or final clusters.
func rareLetter(r rune) bool {
	switch {
	case r >= 'А' && r <= 'я', r == 'Ё', r == 'ё':
		return false
	case strings.ContainsRune("ЄєІіЇїҐґЎў", r):
		return false
	case unicode.Is(unicode.Cyrillic, r):
		return true
	case r >= 0xAC00 && r <= 0xD7A3:
		s := int(r - 0xAC00)
		vowel, final := s%588/28, s%28
		// ㅒ ㅙ ㅞ; ㄳ ㄵ ㄽ ㄾ ㄿ ㅀ
		return vowel == 3 || vowel == 10 || vowel == 15 ||
			final == 3 || final == 5 || (final >= 12 && final <= 15)
	}
	return false
}

// The Han tiers: each national set's first, most frequent level. A right
// reading in that encoding keeps almost entirely to it; a wrong one lands
// mostly outside it.

func gb2312Level1(r rune) bool {
	b, err := simplifiedchinese.GBK.NewEncoder().String(string(r))
	return err == nil && len(b) == 2 && b[0] >= 0xB0 && b[0] <= 0xD7 && b[1] >= 0xA1
}

func jisLevel1(r rune) bool {
	b, err := japanese.ShiftJIS.NewEncoder().String(string(r))
	return err == nil && len(b) == 2 && (b[0] >= 0x88 && b[0] < 0x98 || b[0] == 0x98 && b[1] <= 0x72)
}

func big5Frequent(r rune) bool {
	b, err := traditionalchinese.Big5.NewEncoder().String(string(r))
	if err != nil || len(b) != 2 {
		return false
	}
	code := int(b[0])<<8 | int(b[1])
	return code >= 0xA440 && code <= 0xC67E
}

// zipUnicodeName returns the UTF-8 name an Info-ZIP Unicode Path extra field
// (0x7075) carries, provided its CRC matches the header name: a tool that
// renamed the entry without updating the extra leaves a stale copy behind.
func zipUnicodeName(name string, extra []byte) (string, bool) {
	for len(extra) >= 4 {
		id := int(extra[0]) | int(extra[1])<<8
		size := int(extra[2]) | int(extra[3])<<8
		if 4+size > len(extra) {
			return "", false
		}
		field := extra[4 : 4+size]
		extra = extra[4+size:]
		if id != 0x7075 || len(field) < 5 || field[0] != 1 {
			continue
		}
		crc := uint32(field[1]) | uint32(field[2])<<8 | uint32(field[3])<<16 | uint32(field[4])<<24
		if crc32.ChecksumIEEE([]byte(name)) != crc || !utf8.Valid(field[5:]) {
			return "", false
		}
		return string(field[5:]), true
	}
	return "", false
}

// extendedParam reads an RFC 2231 charset-tagged parameter, such as
// filename*=koi8-r”%F4..., that mime.ParseMediaType drops because its
// charset is neither US-ASCII nor UTF-8, and decodes it under the declared
// charset. An unknown charset leaves the raw bytes for detection.
func extendedParam(header, key string) string {
	segs := map[int]string{}
	encoded := map[int]bool{}
	for _, field := range splitParams(header) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		rest, ok := strings.CutPrefix(k, key+"*")
		if !ok {
			continue
		}
		n, enc := 0, true
		if rest != "" {
			rest, enc = strings.CutSuffix(rest, "*")
			i, err := strconv.Atoi(rest)
			if err != nil {
				continue
			}
			n = i
		}
		segs[n] = strings.Trim(strings.TrimSpace(v), `"`)
		encoded[n] = enc
	}
	if !encoded[0] {
		return ""
	}
	parts := strings.SplitN(segs[0], "'", 3)
	if len(parts) != 3 {
		return ""
	}
	charset := parts[0]
	segs[0] = parts[2]
	var raw strings.Builder
	for i := 0; ; i++ {
		s, ok := segs[i]
		if !ok {
			break
		}
		if encoded[i] {
			d, err := url.PathUnescape(s)
			if err != nil {
				return ""
			}
			s = d
		}
		raw.WriteString(s)
	}
	enc, err := htmlindex.Get(charset)
	if err != nil {
		return raw.String()
	}
	out, err := enc.NewDecoder().String(raw.String())
	if err != nil {
		return raw.String()
	}
	return out
}

// splitParams splits a header value on the semicolons outside quotes.
func splitParams(s string) []string {
	var out []string
	quoted, start := false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case '\\':
			if quoted {
				i++
			}
		case ';':
			if !quoted {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}
