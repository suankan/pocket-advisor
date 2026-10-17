package charset

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
	utf16 "golang.org/x/text/encoding/unicode"
)

const russian = "Добрый день! Высылаю договор и счёт за май. Прошу подтвердить получение письма до пятницы.\n"

func encode(t *testing.T, cm *charmap.Charmap, s string) []byte {
	t.Helper()
	out, err := cm.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Each way of reading text is taken where it applies, in order, and named.
func TestDecodeSaysHowItReadTheBytes(t *testing.T) {
	wide, err := utf16.UTF16(utf16.LittleEndian, utf16.UseBOM).NewEncoder().Bytes([]byte(russian))
	if err != nil {
		t.Fatal(err)
	}
	declared := append([]byte(`<html><head><meta charset="koi8-r"></head><body>`), encode(t, charmap.KOI8R, "Привет")...)
	for name, tc := range map[string]struct {
		data    []byte
		how     How
		charset string
		text    string
	}{
		"utf-8":               {[]byte(russian), UTF8, "", russian},
		"utf-16 with a BOM":   {wide, BOM, "utf-16le", russian},
		"declared in html":    {declared, Declared, "koi8-r", `<html><head><meta charset="koi8-r"></head><body>Привет`},
		"windows-1251":        {encode(t, charmap.Windows1251, russian), Detected, "windows-1251", russian},
		"koi8-r":              {encode(t, charmap.KOI8R, russian), Detected, "koi8-r", russian},
		"russian among latin": {encode(t, charmap.KOI8R, "From the meeting notes, see below.\n\n"+russian+"\nRegards, the office\nSent from my phone. Please consider the environment before printing this email.\n"), Detected, "koi8-r", ""},
		"western, accented":   {encode(t, charmap.Windows1252, strings.Repeat("Café menu: crème brûlée, déjà vu, “quoted”, première été à Montréal.\n", 5)), Replaced, "", ""},
		"too few to judge":    {encode(t, charmap.Windows1251, "Re: Счёт\n"), Replaced, "", ""},
	} {
		got := Decode(tc.data)
		if got.How != tc.how || got.Charset != tc.charset {
			t.Errorf("%s: read as %s %q, want %s %q", name, got.How, got.Charset, tc.how, tc.charset)
		}
		if tc.text != "" && strings.TrimPrefix(got.Text, "\ufeff") != tc.text {
			t.Errorf("%s: text %q", name, got.Text)
		}
	}
}

// Western text keeps what it can and marks the rest; its note says so.
func TestDecodeReplacesWhatItCannotRead(t *testing.T) {
	got := Decode(encode(t, charmap.Windows1252, "That’s the Café.\n"))
	if got.Text != "That�s the Caf�.\n" || got.Note() != "replaced" {
		t.Fatalf("%q, note %q", got.Text, got.Note())
	}
	if n := Decode([]byte("plain")).Note(); n != "" {
		t.Errorf("utf-8 noted %q", n)
	}
	if n := Decode(encode(t, charmap.Windows1251, russian)).Note(); n != "detected:windows-1251" {
		t.Errorf("detected noted %q", n)
	}
}

// A Russian page's markup does not hide its text: the Latin of its tags is
// left out of the count.
func TestRussianLooksPastMarkup(t *testing.T) {
	page := `<html><head><title></title><style>body { font-family: Arial, sans-serif; }</style></head><body><p class="message-body">` + russian + `</p></body></html>`
	got, ok := Russian(encode(t, charmap.Windows1251, page))
	if !ok || got.Charset != "windows-1251" || !strings.Contains(got.Text, "договор") {
		t.Fatalf("ok %v, %+v", ok, got)
	}
}
