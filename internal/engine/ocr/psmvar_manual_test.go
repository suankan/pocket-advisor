//go:build ocr && manual

package ocr

import (
	"os"
	"strings"
	"testing"

	"github.com/otiai10/gosseract/v2"
)

// tessedit_pageseg_mode via SetVariable is applied AFTER Init, unlike
// SetPageSegMode which Init then resets.
func TestPSMViaSetVariable(t *testing.T) {
	path := os.Getenv("PNG_PATH")
	if path == "" {
		t.Skip("set PNG_PATH")
	}
	for _, psm := range []string{"0", "3", "6"} {
		c := gosseract.NewClient()
		_ = c.SetLanguage("osd", "eng")
		_ = c.SetVariable(gosseract.SettableVariable("tessedit_pageseg_mode"), psm)
		_ = c.SetImage(path)
		txt, err := c.Text()
		c.Close()
		out := strings.TrimSpace(txt)
		if len(out) > 220 {
			out = out[:220]
		}
		t.Logf("tessedit_pageseg_mode=%s err=%v len=%d\n%s\n---", psm, err, len(txt), out)
	}
}
