package dashboard

import (
	"strings"
	"testing"
	"time"
)

func TestCommas(t *testing.T) {
	cases := map[int64]string{
		0: "0", 7: "7", 999: "999", 1000: "1,000",
		1284: "1,284", 12345: "12,345", 1234567: "1,234,567",
	}
	for in, want := range cases {
		if got := commas(in); got != want {
			t.Errorf("commas(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestBytesHuman(t *testing.T) {
	cases := map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.0 KB",
		1536: "1.5 KB", 1048576: "1.0 MB", 883000000: "842.1 MB",
	}
	for in, want := range cases {
		if got := bytesHuman(in); got != want {
			t.Errorf("bytesHuman(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestElapsed(t *testing.T) {
	cases := map[time.Duration]string{
		0:                              "00:00:00",
		45 * time.Second:               "00:00:45",
		4*time.Minute + 17*time.Second: "00:04:17",
		2*time.Hour + 3*time.Minute:    "02:03:00",
	}
	for in, want := range cases {
		if got := elapsed(in); got != want {
			t.Errorf("elapsed(%v) = %q, want %q", in, got, want)
		}
	}
}

// The bar is fixed width regardless of value, because a bar that changes width
// makes the whole column jitter on every repaint.
func TestBarIsFixedWidthAndClamped(t *testing.T) {
	for _, frac := range []float64{-1, 0, 0.5, 1, 2} {
		got := bar(frac, 10)
		if n := len([]rune(got)); n != 10 {
			t.Errorf("bar(%v, 10) has %d runes, want 10 (%q)", frac, n, got)
		}
	}
	if got := bar(0, 10); strings.Contains(got, "█") {
		t.Errorf("bar(0) = %q, want no filled cells", got)
	}
	if got := bar(1, 10); strings.Contains(got, "░") {
		t.Errorf("bar(1) = %q, want no empty cells", got)
	}
}

func TestShortStripsSubjectPrefix(t *testing.T) {
	if got := short("ingest.pdfs.raw"); got != "pdfs.raw" {
		t.Errorf("short() = %q, want %q", got, "pdfs.raw")
	}
}
