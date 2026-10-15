//go:build ocr

package ocr

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/suankan/pocket-advisor/internal/limits"
)

// An image Leptonica cannot decode reaches Tesseract as no image at all, and
// gosseract reports that as an unset image rather than a decode failure. It has
// to come back as ErrUnreadable, so a worker declines it instead of retrying
// bytes that cannot change their answer, while a decodable image does not.
func TestBytesNamesImagesTheEngineCannotDecode(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 240, 240))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	img.SetGray(10, 10, color.Gray{})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	valid := buf.Bytes()

	e := NewEngine(limits.NewCPU(1), "eng")
	for _, tc := range []struct {
		name       string
		data       []byte
		unreadable bool
	}{
		{"unknown format", bytes.Repeat([]byte("not an image "), 1000), true},
		{"truncated png", valid[:len(valid)/2], true},
		{"png", valid, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.Bytes(context.Background(), tc.data)
			if got := errors.Is(err, ErrUnreadable); got != tc.unreadable {
				t.Fatalf("err = %v, unreadable = %v, want %v", err, got, tc.unreadable)
			}
		})
	}
}
