//go:build !ocr

package ocr

import "image"

// Available reports whether this build has a real OCR engine linked.
//
// Only the document-extractor image is built with -tags ocr and the tesseract
// runtime. Everything else links this stub so the C toolchain does not
// silently become a dependency of the whole system (§8.3).
//
// Callers treat ErrUnavailable as a declined outcome, not a failure: the
// document is recorded SKIPPED rather than retried three times into the DLQ.
const Available = false

type Engine struct{}

func NewEngine(int, string) *Engine { return &Engine{} }

func (e *Engine) Close() error { return nil }

func (e *Engine) Image(image.Image) (string, error) { return "", ErrUnavailable }

func (e *Engine) Bytes([]byte) (string, error) { return "", ErrUnavailable }
