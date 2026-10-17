// Package charset turns the bytes of a text that may not be UTF-8 into text,
// saying how (ingestion-design.md §6.3).
//
// A wrong charset yields valid UTF-8 from wrong bytes, which is silent
// corruption of indexed evidence, so a charset is taken from the bytes only
// where they leave no doubt: a byte-order mark, a declaration the file makes
// itself, or the one family of undeclared legacy text a corpus holds in
// quantity — Russian in Windows-1251 or KOI8-R — when its letter statistics
// tell the two apart unmistakably. Anything else is read as UTF-8 with each
// byte sequence that does not decode replaced by U+FFFD, which marks exactly
// what could not be read.
package charset

import (
	"bytes"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
	utf16 "golang.org/x/text/encoding/unicode"
)

// How names the way text was obtained from bytes.
type How string

const (
	// UTF8 is bytes that were valid UTF-8 as they stood.
	UTF8 How = "utf-8"
	// BOM is UTF-16 announced by a byte-order mark.
	BOM How = "bom"
	// Declared is a charset the file names itself, in an HTML meta tag or an
	// XML declaration.
	Declared How = "declared"
	// Detected is Russian text in Windows-1251 or KOI8-R, recognised by its
	// letters.
	Detected How = "detected"
	// Replaced is UTF-8 with what does not decode replaced by U+FFFD.
	Replaced How = "replaced"
)

// Decoded is text and how it was obtained. Charset names the charset read
// under, empty for UTF8 and Replaced.
type Decoded struct {
	Text    string
	How     How
	Charset string
}

// Note is the label a document records when its text was not plain UTF-8:
// "detected:windows-1251", "declared:koi8-r", "bom:utf-16le", "replaced".
// It is empty for UTF8.
func (d Decoded) Note() string {
	switch d.How {
	case UTF8:
		return ""
	case Replaced:
		return string(Replaced)
	}
	return string(d.How) + ":" + d.Charset
}

// Decode reads data as text.
func Decode(data []byte) Decoded {
	if utf8.Valid(data) {
		return Decoded{Text: string(data), How: UTF8}
	}
	if d, ok := byBOM(data); ok {
		return d
	}
	if d, ok := byDeclaration(data); ok {
		return d
	}
	if d, ok := Russian(data); ok {
		return d
	}
	return Decoded{Text: strings.ToValidUTF8(string(data), "\uFFFD"), How: Replaced}
}

func byBOM(data []byte) (Decoded, bool) {
	var enc encoding.Encoding
	var name string
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
		enc, name = utf16.UTF16(utf16.LittleEndian, utf16.ExpectBOM), "utf-16le"
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		enc, name = utf16.UTF16(utf16.BigEndian, utf16.ExpectBOM), "utf-16be"
	default:
		return Decoded{}, false
	}
	out, err := enc.NewDecoder().Bytes(data)
	if err != nil {
		return Decoded{}, false
	}
	return Decoded{Text: string(out), How: BOM, Charset: name}, true
}

// declaration finds a charset an HTML meta tag or an XML declaration names in
// the head of a file.
var declaration = regexp.MustCompile(`(?i)(?:<meta[^>]*charset\s*=\s*["']?|<\?xml[^>]*encoding\s*=\s*["'])([a-z0-9_.:\-]+)`)

func byDeclaration(data []byte) (Decoded, bool) {
	m := declaration.FindSubmatch(data[:min(len(data), 4096)])
	if m == nil {
		return Decoded{}, false
	}
	label := strings.ToLower(string(m[1]))
	enc, err := htmlindex.Get(label)
	if err != nil {
		return Decoded{}, false
	}
	name, _ := htmlindex.Name(enc)
	if name == "utf-8" {
		// A file declaring UTF-8 that is not UTF-8 declares nothing usable.
		return Decoded{}, false
	}
	out, err := enc.NewDecoder().Bytes(data)
	if err != nil || !utf8.Valid(out) {
		return Decoded{}, false
	}
	return Decoded{Text: string(out), How: Declared, Charset: name}, true
}

// Russian reads data as Russian text in Windows-1251 or KOI8-R when its
// letters say which, unmistakably.
//
// The two put the Cyrillic alphabet in the same 64 bytes with the cases
// swapped: Windows-1251 has capitals in 0xC0–0xDF and small letters above,
// KOI8-R the reverse. Running text is mostly small letters, so read under the
// right charset most of its Russian words are in small letters, and under the
// wrong one most are in capitals. Both readings are made, and only letters in
// words are counted — runs of at least three Cyrillic letters outside markup
// — which is how Russian reads and how a Western accent read as Cyrillic
// never does: it lands as one letter among Latin ones. A reading is taken
// when there are enough such letters to judge, nearly all the Cyrillic it
// reads is in words, its share of small letters is high, and the other
// reading's is low.
func Russian(data []byte) (Decoded, bool) {
	type reading struct {
		name    string
		text    string
		share   float64
		inWords int
		ok      bool
	}
	var readings []reading
	for _, c := range []struct {
		name string
		cm   *charmap.Charmap
	}{{"windows-1251", charmap.Windows1251}, {"koi8-r", charmap.KOI8R}} {
		out, err := c.cm.NewDecoder().Bytes(data)
		if err != nil {
			return Decoded{}, false
		}
		text := string(out)
		cyrillic, inWords, lower := 0, 0, 0
		run, runLower := 0, 0
		endRun := func() {
			if run >= minWord {
				inWords += run
				lower += runLower
			}
			run, runLower = 0, 0
		}
		for _, r := range withoutMarkup(text) {
			if unicode.IsLetter(r) && unicode.Is(unicode.Cyrillic, r) {
				cyrillic++
				run++
				if unicode.IsLower(r) {
					runLower++
				}
				continue
			}
			endRun()
		}
		endRun()
		share := 0.0
		if inWords > 0 {
			share = float64(lower) / float64(inWords)
		}
		ok := inWords >= minCyrillic && float64(inWords) >= inWordsShare*float64(cyrillic)
		readings = append(readings, reading{c.name, text, share, inWords, ok})
	}
	w, k := readings[0], readings[1]
	switch {
	case w.ok && w.share >= confident && k.share <= doubtful:
		return Decoded{Text: w.text, How: Detected, Charset: w.name}, true
	case k.ok && k.share >= confident && w.share <= doubtful:
		return Decoded{Text: k.text, How: Detected, Charset: k.name}, true
	}
	return Decoded{}, false
}

// The bounds Russian judges by. Running Russian text has about nine small
// letters in ten, so read under the wrong charset about nine capitals in ten.
const (
	minWord      = 3
	minCyrillic  = 20
	inWordsShare = 0.8
	confident    = 0.7
	doubtful     = 0.3
)

// markup matches HTML and XML tags, whose Latin names would otherwise outweigh
// the Cyrillic text of a short page.
var markup = regexp.MustCompile(`<[^>]*>`)

func withoutMarkup(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	return markup.ReplaceAllString(s, " ")
}
