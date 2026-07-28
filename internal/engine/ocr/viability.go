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
	MinOCRChars  = 20
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
// almost nothing is recorded as not viable rather than retried.
func EnoughText(s string) bool {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
			if n >= MinOCRChars {
				return true
			}
		}
	}
	return false
}
