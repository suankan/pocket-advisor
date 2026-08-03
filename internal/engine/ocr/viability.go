// Package ocr wraps Tesseract and the image viability gate. Both the PDF
// rasterisation path and standalone images use it, which is why it lives
// outside internal/engine/pdf (ingestion-design.md §5.4).
package ocr

import (
	"bytes"
	"errors"
	"image"
	"unicode"

	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// ErrUnavailable is returned when OCR is called in a build without it linked.
//
// Declared here rather than in the stub because callers must be able to test
// for it in both builds: with -tags ocr it is simply never returned.
var ErrUnavailable = errors.New("ocr not available in this build")

// Viability thresholds. Email corpora are full of images that are not
// documents: tracking pixels, logos, signature graphics, social icons.
// Sending them to OCR wastes the scarcest resource in the cluster and floods
// Tier 3 with noise chunks that degrade retrieval precision (§5.4).
const (
	MinDimension = 200
	MinArea      = 40_000
	MinBytes     = 8 << 10

	// MinOCRWords is the post-OCR gate: how many word-like tokens the
	// extracted text must contain to be worth indexing.
	//
	// This replaces a 20-alphanumeric-character threshold that was far too
	// weak. Measured against a real corpus, OCR over a *photograph* — a room,
	// a building exterior — reliably produces hundreds of tokens and zero
	// words: "| | | | | fare я — = р = ——,_ | —< ГИ". That passed the old
	// gate easily, and such chunks then scored spuriously against unrelated
	// questions, because a cross-encoder cannot meaningfully rank character
	// soup and lands near-arbitrarily around zero.
	//
	// Five is where the corpus separates. Below it sits photography and
	// letterhead fragments; at and above it sits real content — payment
	// screenshots ("Reference no. E1907241453 Amount $4,333.55"), forms,
	// scanned correspondence. The cost is a handful of logo images whose few
	// words (a firm or school name) already appear in the surrounding
	// document text.
	MinOCRWords = 5

	// minWordRunes is what counts as a word. Shorter runs are the noise OCR
	// hallucinates out of edges and texture.
	minWordRunes = 4
)

// Viable reports whether an image is worth OCRing, and why not if it is not.
// width/height of 0 mean "unknown", in which case they are measured here.
func Viable(data []byte, width, height int) (bool, string) {
	if len(data) < MinBytes {
		return false, "below_min_bytes"
	}
	if width == 0 || height == 0 {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			width, height = cfg.Width, cfg.Height
		}
	}
	if width > 0 && height > 0 {
		if width < MinDimension || height < MinDimension {
			return false, "below_min_dimension"
		}
		if width*height < MinArea {
			return false, "below_min_area"
		}
	}
	return true, ""
}

// EnoughText applies the post-hoc half of the gate: an image that OCRs to
// almost nothing — or to nothing meaningful — is recorded as not viable
// rather than retried.
//
// Counting characters is not enough. A photograph yields plenty of letters
// and no words, so the test is how many *word-like* tokens survive: runs of
// at least minWordRunes letters. Digits deliberately do not count toward a
// word, since OCR noise is dense in stray digits, but a token may mix them.
func EnoughText(s string) bool {
	words, run := 0, 0
	flush := func() {
		if run >= minWordRunes {
			words++
		}
		run = 0
	}
	for _, r := range s {
		switch {
		case unicode.IsLetter(r):
			run++
		case unicode.IsDigit(r):
			// Neither extends nor breaks a word: "E1907241453" is not a word,
			// but "Grammar2" should still count as one.
		default:
			flush()
			if words >= MinOCRWords {
				return true
			}
		}
	}
	flush()
	return words >= MinOCRWords
}
