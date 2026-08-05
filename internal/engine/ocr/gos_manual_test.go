//go:build ocr && manual

package ocr

import (
	"os"
	"strings"
	"testing"

	"github.com/otiai10/gosseract/v2"
)

// The CLI reads this exact PNG cleanly. Which client setting breaks it?
func TestGosseractVariants(t *testing.T) {
	data, err := os.ReadFile(os.Getenv("PNG_PATH"))
	if err != nil {
		t.Skip("set PNG_PATH")
	}
	try := func(name string, cfg func(*gosseract.Client)) {
		c := gosseract.NewClient()
		defer c.Close()
		cfg(c)
		if err := c.SetImageFromBytes(data); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		txt, err := c.Text()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ok := strings.Contains(strings.ToUpper(txt), "COPYRIGHT")
		t.Logf("%-34s chars=%5d COPYRIGHT=%v | %q", name, len(txt), ok, first(txt, 60))
	}
	try("defaults", func(c *gosseract.Client) {})
	try("SetLanguage(eng,rus)", func(c *gosseract.Client) { _ = c.SetLanguage("eng", "rus") })
	try("SetLanguage(eng+rus) single arg", func(c *gosseract.Client) { _ = c.SetLanguage("eng+rus") })
	try("DisableOutput only", func(c *gosseract.Client) { _ = c.DisableOutput() })
	try("DisableOutput+SetLanguage", func(c *gosseract.Client) {
		_ = c.DisableOutput()
		_ = c.SetLanguage("eng", "rus")
	})
}

func first(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n]
	}
	return s
}
