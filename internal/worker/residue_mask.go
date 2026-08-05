package worker

import (
	"image"
	"image/color"
	"image/draw"

	"github.com/suankan/pocket-advisor/internal/engine/pdf"
)

// ResidueDPI is what a page is rendered at to look for text the layer misses.
//
// Lower than RasterDPI because this is a reading task on born-digital output,
// not a scan: the marks being recovered were drawn as vectors, so they are
// sharp at any resolution and gain nothing from the extra pixels a scan needs.
const ResidueDPI = 200

// maskBoxes paints out every region the text layer already covers.
//
// Boxes arrive in PDF points with the origin at the bottom left; the image has
// pixels with the origin at the top left, hence the scale and the flip. Each is
// grown by a pixel because a glyph's ink can sit a fraction outside its
// reported box, and a surviving sliver of a masked letter is exactly the kind
// of fragment OCR turns into a spurious word.
func maskBoxes(src image.Image, boxes []pdf.CharBox, dpi int) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(b)
	draw.Draw(dst, b, src, b.Min, draw.Src)

	white := &image.Uniform{color.RGBA{255, 255, 255, 255}}
	s := float64(dpi) / 72.0
	h := float64(b.Dy())
	for _, cb := range boxes {
		r := image.Rect(
			int(cb.Left*s)-1, int(h-cb.Top*s)-1,
			int(cb.Right*s)+1, int(h-cb.Bottom*s)+1,
		).Canon().Intersect(b)
		if r.Empty() {
			continue
		}
		draw.Draw(dst, r, white, image.Point{}, draw.Src)
	}
	return dst
}

// minInkPixels is how much dark pixel a masked page must still carry before it
// is worth OCRing. One word of 11pt text at ResidueDPI inks on the order of a
// thousand pixels, so this is far below anything readable and only rejects
// pages that masking left effectively blank.
const minInkPixels = 200

// hasInk reports whether masking left anything behind worth reading.
//
// This is the whole reason the pass is affordable. Every page of every digital
// document reaches here, and OCR is by far the most expensive thing in the
// pipeline — but on a document whose text layer is complete, masking erases the
// page, and answering "is this blank" costs a scan of pixels rather than a call
// into Tesseract. Pages are sampled on a grid, because a page that holds real
// text holds far more than one pixel of it and there is no need to look at
// every one to find out.
func hasInk(img image.Image) bool {
	const stride = 3
	b := img.Bounds()
	ink := 0
	for y := b.Min.Y; y < b.Max.Y; y += stride {
		for x := b.Min.X; x < b.Max.X; x += stride {
			r, g, bl, _ := img.At(x, y).RGBA()
			// Mid-grey and darker. Anti-aliased edges of a real glyph clear
			// this comfortably; scanner background and page tint do not.
			if (r+g+bl)/3 < 0x8000 {
				ink++
				if ink*stride*stride >= minInkPixels {
					return true
				}
			}
		}
	}
	return false
}

// readable reports whether recovered text carries anything a search could
// match: at least one run of two or more letters or digits.
//
// What this rejects is the lone character — the sliver of a masked glyph whose
// ink sat a fraction outside the box, or a mark on a logo that resembles a
// letter. Those are the residue pass's only false positives in practice, they
// mean nothing to a reader and nothing to a query, and spliced into a sentence
// they are worse than absent. A real word clears this trivially, including the
// short logo words ("Bank", "OPTUS") that are correct if unexciting.
func readable(s string) bool {
	run := 0
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r > 127:
			run++
			if run >= 2 {
				return true
			}
		default:
			run = 0
		}
	}
	return false
}
