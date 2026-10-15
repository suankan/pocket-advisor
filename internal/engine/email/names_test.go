package email

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

func encodeAll(t *testing.T, enc encoding.Encoding, names []string) []string {
	t.Helper()
	out := make([]string, len(names))
	for i, n := range names {
		s, err := enc.NewEncoder().String(n)
		if err != nil {
			t.Fatalf("encode %q: %v", n, err)
		}
		out[i] = s
	}
	return out
}

var (
	cyrillicNames = []string{"Отчёт за квартал.docx", "Фотографии/Лето на даче.jpg", "Договор аренды (копия).pdf", "Справка с работы.pdf"}
	japaneseNames = []string{"請求書のコピー.pdf", "会議の議事録.docx", "資料/表示設定.txt"}
)

// Each legacy encoding a container is likely to carry is recognised from its
// names alone, including single short names where the script makes it clear.
func TestDecodeNamesRecognisesLegacyEncodings(t *testing.T) {
	cases := []struct {
		label string
		enc   encoding.Encoding
		names []string
	}{
		{"ibm866", charmap.CodePage866, cyrillicNames},
		{"ibm866", charmap.CodePage866, []string{"Мама.jpg"}},
		{"windows-1251", charmap.Windows1251, cyrillicNames},
		{"windows-1251", charmap.Windows1251, []string{"Привет.txt"}},
		{"koi8-r", charmap.KOI8R, cyrillicNames},
		{"windows-1250", charmap.Windows1250, []string{"Łódź zdjęcia.jpg"}},
		{"ibm852", charmap.CodePage852, []string{"Łódź zdjęcia.jpg"}},
		{"windows-1252", charmap.Windows1252, []string{"Straße.pdf"}},
		{"shift_jis", japanese.ShiftJIS, japaneseNames},
		{"shift_jis", japanese.ShiftJIS, []string{"会議資料.pdf"}},
		{"gbk", simplifiedchinese.GBK, []string{"年度报告.pdf", "会议记录.docx", "工资单/三月.xlsx"}},
		{"big5", traditionalchinese.Big5, []string{"年度報告.pdf", "會議記錄.docx", "薪資單/三月.xlsx"}},
	}
	for _, c := range cases {
		t.Run(c.label+"/"+c.names[0], func(t *testing.T) {
			raw := encodeAll(t, c.enc, c.names)
			got, label := decodeNames(raw, "")
			if label != c.label {
				t.Fatalf("detected %q, want %q (names %q)", label, c.label, got)
			}
			for i := range got {
				if got[i] != c.names[i] {
					t.Errorf("name %d = %q, want %q", i, got[i], c.names[i])
				}
			}
		})
	}
}

// Latin code pages that agree on a name are not rivals: whichever is named,
// the reading is the same.
func TestDecodeNamesAcceptsAgreeingLatinCodePages(t *testing.T) {
	for _, enc := range []encoding.Encoding{charmap.Windows1252, charmap.CodePage437, charmap.CodePage850} {
		raw := encodeAll(t, enc, []string{"Café menu.txt", "Résumé été.pdf"})
		got, label := decodeNames(raw, "")
		if label == "" || got[0] != "Café menu.txt" || got[1] != "Résumé été.pdf" {
			t.Errorf("%v: got %q under %q", enc, got, label)
		}
	}
}

// When no reading is clearly right the name is kept readable where it can be
// and the rest becomes U+FFFD, never a guess: not for bytes no candidate
// reads well, and not when two candidates read them equally well but
// differently.
func TestDecodeNamesReplacesWhatItCannotRead(t *testing.T) {
	cases := map[string]string{
		"unreadable": "scan\x01\xff\xfe\x80.bin",
		// The same bytes are common characters in both GBK and Big5.
		"ambiguous": encodeAll(t, simplifiedchinese.GBK, []string{"年度报告.pdf"})[0],
		// Lower-case Windows-1251 is upper-case KOI8.
		"case-inverse": encodeAll(t, charmap.Windows1251, []string{"привет.txt"})[0],
	}
	for name, raw := range cases {
		got, label := decodeNames([]string{raw}, "")
		if label != "" {
			t.Errorf("%s: accepted %q, reading %q", name, label, got)
			continue
		}
		if !utf8.ValidString(got[0]) || !strings.HasSuffix(got[0], filepath.Ext(raw)) || !strings.Contains(got[0], "�") {
			t.Errorf("%s: name = %q, want the readable part kept and invalid bytes replaced", name, got[0])
		}
	}
}

// A charset the message declares is tried before detection.
func TestDecodeNamesPrefersTheDeclaredCharset(t *testing.T) {
	raw := encodeAll(t, charmap.KOI8U, []string{"Звіт.pdf"})
	got, label := decodeNames(raw, "KOI8-U")
	if label != "koi8-u" || got[0] != "Звіт.pdf" {
		t.Errorf("got %q under %q", got, label)
	}
}

// Every name leaves the package valid UTF-8 without NUL, and only recovered
// names carry their original bytes.
func TestResolveNamesKeepsRawBytesOnlyForRecoveredNames(t *testing.T) {
	raw := encodeAll(t, charmap.CodePage866, cyrillicNames[:2])
	children := []Child{
		{Filename: "plain\x00name.txt"},
		{Filename: raw[0]},
		{Filename: raw[1]},
	}
	resolveNames(children, "")
	if children[0].Filename != "plainname.txt" || children[0].RawName != nil {
		t.Errorf("plain child = %+v", children[0])
	}
	for i, c := range children[1:] {
		if c.Filename != cyrillicNames[i] || c.NameEncoding != "ibm866" || string(c.RawName) != raw[i] {
			t.Errorf("child %d = %q %q raw %x", i+1, c.Filename, c.NameEncoding, c.RawName)
		}
	}
}

func zipWith(t *testing.T, entries []zip.FileHeader) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := range entries {
		w, err := zw.CreateHeader(&entries[i])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("member " + entries[i].Name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func unicodePathExtra(raw, name string) []byte {
	field := []byte{1, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(field[1:], crc32.ChecksumIEEE([]byte(raw)))
	field = append(field, name...)
	extra := []byte{0x75, 0x70, 0, 0}
	binary.LittleEndian.PutUint16(extra[2:], uint16(len(field)))
	return append(extra, field...)
}

// A ZIP written by a DOS-era tool carries OEM code page names without the
// UTF-8 flag. The archive's names are read together, the folder part is
// dropped after decoding, and a valid Unicode Path extra field wins over
// detection while a stale one is ignored.
func TestUnrollZipDecodesLegacyMemberNames(t *testing.T) {
	raw := encodeAll(t, charmap.CodePage866, cyrillicNames)
	stale := encodeAll(t, charmap.CodePage866, []string{"Старое имя.txt"})[0]
	data := zipWith(t, []zip.FileHeader{
		{Name: raw[0], NonUTF8: true},
		{Name: raw[1], NonUTF8: true},
		{Name: raw[2], NonUTF8: true},
		{Name: raw[3], NonUTF8: true},
		{Name: "already-utf8 Ünïcode.txt"},
		{Name: stale, NonUTF8: true, Extra: unicodePathExtra(stale, "Новое имя.txt")},
		{Name: raw[0] + "x", NonUTF8: true, Extra: unicodePathExtra("different", "Не то имя.txt")},
	})
	children, err := UnrollArchive(data, "synthetic.zip")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Отчёт за квартал.docx", "Лето на даче.jpg", "Договор аренды (копия).pdf", "Справка с работы.pdf",
		"already-utf8 Ünïcode.txt", "Новое имя.txt", "Отчёт за квартал.docxx"}
	if len(children) != len(want) {
		t.Fatalf("got %d children, want %d", len(children), len(want))
	}
	for i, c := range children {
		if c.Filename != want[i] {
			t.Errorf("member %d = %q, want %q", i, c.Filename, want[i])
		}
	}
	if children[1].NameEncoding != "ibm866" || string(children[1].RawName) != raw[1] {
		t.Errorf("detected member records %q raw %x, want ibm866 and its full raw path", children[1].NameEncoding, children[1].RawName)
	}
	if children[4].RawName != nil || children[5].RawName != nil {
		t.Error("a UTF-8 name, whether given directly or by a Unicode Path field, must not be marked as recovered")
	}
}

// Shift-JIS can end a character with the backslash byte; separators are only
// recognised once the path is text.
func TestUnrollTarDecodesBeforeSplittingPaths(t *testing.T) {
	raw := encodeAll(t, japanese.ShiftJIS, japaneseNames)
	if !strings.Contains(raw[2], `\`) {
		t.Fatal("fixture lost its backslash trail byte")
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range raw {
		body := []byte("member")
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg, Format: tar.FormatGNU}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	children, err := UnrollArchive(buf.Bytes(), "synthetic.tar")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"請求書のコピー.pdf", "会議の議事録.docx", "表示設定.txt"}
	for i, c := range children {
		if c.Filename != want[i] {
			t.Errorf("member %d = %q, want %q", i, c.Filename, want[i])
		}
	}
}

// Attachment names arrive declared (RFC 2231 with any charset), or as raw
// 8-bit bytes read with the message's own text charset.
func TestParseEmailDecodesLegacyAttachmentNames(t *testing.T) {
	koi := encodeAll(t, charmap.KOI8R, []string{"Счёт на оплату.pdf"})[0]
	var pct strings.Builder
	for _, b := range []byte(encodeAll(t, charmap.Windows1251, []string{"Акт сверки.xlsx"})[0]) {
		if b < 0x80 && b != ' ' && b != '%' {
			pct.WriteByte(b)
		} else {
			pct.WriteString("%" + strings.ToUpper(string("0123456789abcdef"[b>>4])+string("0123456789abcdef"[b&15])))
		}
	}
	msg := "From: a@example.com\r\nTo: b@example.com\r\nSubject: synthetic\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=XX\r\n\r\n" +
		"--XX\r\nContent-Type: text/plain; charset=koi8-r\r\n\r\nbody\r\n" +
		"--XX\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"" + koi + "\"\r\n\r\n%PDF-1.4 one\r\n" +
		"--XX\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename*=windows-1251''" + pct.String() + "\r\n\r\ntwo\r\n" +
		"--XX--\r\n"
	p, err := ParseEmail([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Children) != 2 {
		t.Fatalf("got %d children", len(p.Children))
	}
	if c := p.Children[0]; c.Filename != "Счёт на оплату.pdf" || c.NameEncoding != "koi8-r" {
		t.Errorf("raw 8-bit name = %q under %q", c.Filename, c.NameEncoding)
	}
	if c := p.Children[1]; c.Filename != "Акт сверки.xlsx" || c.RawName != nil {
		t.Errorf("RFC 2231 name = %q (raw %x); a declared charset is not a recovery", c.Filename, c.RawName)
	}
}
