//go:build !ocr

package ocr

import (
	"context"
	"image"

	"github.com/suankan/pocket-advisor/internal/limits"
)

// Available reports whether this build has a real OCR engine linked.
//
// Builds without -tags ocr link this stub so the C toolchain does not silently
// become a dependency of every build (§8.3).
//
// Callers treat ErrUnavailable as a declined outcome, not a failure: the
// document is recorded SKIPPED rather than retried three times into the DLQ.
const Available = false

type Engine struct{}

func NewEngine(*limits.CPU, string) *Engine { return &Engine{} }

func (e *Engine) Close() error { return nil }

func (e *Engine) Image(context.Context, image.Image) (string, error) { return "", ErrUnavailable }

func (e *Engine) Bytes(context.Context, []byte) (string, error) { return "", ErrUnavailable }

// Word mirrors the linked build's type so callers compile either way.
type Word struct {
	Text       string
	Box        image.Rectangle
	Confidence float64
	Line       image.Rectangle
}

const MinWordConfidence = 80.0

func (e *Engine) ImageWords(context.Context, image.Image, float64) ([]Word, error) {
	return nil, ErrUnavailable
}
