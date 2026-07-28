// Package office extracts text from OOXML and other structured office
// formats (ingestion-design.md §5.5).
//
// Pure Go, no CGo: OOXML is a zip of XML, so archive/zip plus encoding/xml
// covers it. That is why this is a separate binary from the extractor pool
// despite the topological similarity — it is free of the C-heap lifecycle
// concerns that dominate the OCR path.
package office

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Extract dispatches on the subtype discovery detected.
func Extract(data []byte, subtype string) (string, error) {
	switch strings.ToLower(subtype) {
	case "docx":
		return extractDocx(data)
	case "xlsx":
		return extractXlsx(data)
	case "pptx":
		return extractPptx(data)
	case "odt":
		return extractODT(data)
	case "rtf":
		return extractRTF(data), nil
	}
	return "", fmt.Errorf("unsupported office subtype %q", subtype)
}

func openZip(data []byte) (*zip.Reader, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open ooxml container: %w", err)
	}
	return zr, nil
}

func readEntry(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("entry %q not found", name)
}

// ---------- docx ----------

func extractDocx(data []byte) (string, error) {
	zr, err := openZip(data)
	if err != nil {
		return "", err
	}
	body, err := readEntry(zr, "word/document.xml")
	if err != nil {
		return "", err
	}
	return renderWordML(body), nil
}

// renderWordML walks the document stream, emitting paragraph text in document
// order and flattening table cells row-wise. Row structure is preserved
// because a date, counterparty and amount from one row must stay together for
// retrieval to match them (§5.5).
func renderWordML(b []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(b))
	var out strings.Builder
	var cell strings.Builder
	inCell := false
	var rowCells []string

	flushPara := func(s string) {
		if t := strings.TrimSpace(s); t != "" {
			out.WriteString(t)
			out.WriteByte('\n')
		}
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tc":
				inCell = true
				cell.Reset()
			case "tab":
				if inCell {
					cell.WriteByte(' ')
				} else {
					out.WriteByte('\t')
				}
			case "br":
				if !inCell {
					out.WriteByte('\n')
				}
			}
		case xml.CharData:
			if inCell {
				cell.Write(t)
			} else {
				out.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "tc":
				inCell = false
				rowCells = append(rowCells, strings.TrimSpace(cell.String()))
			case "tr":
				flushPara(strings.Join(rowCells, "\t"))
				rowCells = nil
			case "p":
				if !inCell {
					flushPara("")
					out.WriteByte('\n')
				}
			}
		}
	}
	return collapse(out.String())
}

// ---------- xlsx ----------

type sheetRef struct {
	name string
	path string
}

func extractXlsx(data []byte) (string, error) {
	zr, err := openZip(data)
	if err != nil {
		return "", err
	}

	shared := readSharedStrings(zr)
	sheets := resolveSheets(zr)

	var out strings.Builder
	for _, s := range sheets {
		raw, err := readEntry(zr, s.path)
		if err != nil {
			continue
		}
		rows := renderSheet(raw, shared)
		if len(rows) == 0 {
			continue
		}
		// Sheet name as a header so a row keeps its context after chunking.
		fmt.Fprintf(&out, "# %s\n", s.name)
		for _, r := range rows {
			out.WriteString(r)
			out.WriteByte('\n')
		}
		out.WriteByte('\n')
	}
	return collapse(out.String()), nil
}

func readSharedStrings(zr *zip.Reader) []string {
	raw, err := readEntry(zr, "xl/sharedStrings.xml")
	if err != nil {
		return nil
	}
	var doc struct {
		SI []struct {
			T string `xml:"t"`
			R []struct {
				T string `xml:"t"`
			} `xml:"r"`
		} `xml:"si"`
	}
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	out := make([]string, 0, len(doc.SI))
	for _, si := range doc.SI {
		if si.T != "" {
			out = append(out, si.T)
			continue
		}
		var b strings.Builder
		for _, r := range si.R {
			b.WriteString(r.T)
		}
		out = append(out, b.String())
	}
	return out
}

func resolveSheets(zr *zip.Reader) []sheetRef {
	// Map sheet display names to their parts via workbook.xml + rels.
	wb, err := readEntry(zr, "xl/workbook.xml")
	if err != nil {
		return fallbackSheets(zr)
	}
	var wbDoc struct {
		Sheets struct {
			Sheet []struct {
				Name string `xml:"name,attr"`
				RID  string `xml:"id,attr"`
			} `xml:"sheet"`
		} `xml:"sheets"`
	}
	if err := xml.Unmarshal(wb, &wbDoc); err != nil {
		return fallbackSheets(zr)
	}

	rels, err := readEntry(zr, "xl/_rels/workbook.xml.rels")
	if err != nil {
		return fallbackSheets(zr)
	}
	var relDoc struct {
		Rel []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(rels, &relDoc); err != nil {
		return fallbackSheets(zr)
	}
	byID := map[string]string{}
	for _, r := range relDoc.Rel {
		byID[r.ID] = strings.TrimPrefix(r.Target, "/")
	}

	var out []sheetRef
	for _, s := range wbDoc.Sheets.Sheet {
		target := byID[s.RID]
		if target == "" {
			continue
		}
		if !strings.HasPrefix(target, "xl/") {
			target = "xl/" + target
		}
		out = append(out, sheetRef{name: s.Name, path: target})
	}
	if len(out) == 0 {
		return fallbackSheets(zr)
	}
	return out
}

func fallbackSheets(zr *zip.Reader) []sheetRef {
	var out []sheetRef
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "xl/worksheets/") && strings.HasSuffix(f.Name, ".xml") {
			out = append(out, sheetRef{name: strings.TrimSuffix(strings.TrimPrefix(f.Name, "xl/worksheets/"), ".xml"), path: f.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// renderSheet emits one line per row, tab separated, with formulas resolved to
// their cached values.
func renderSheet(raw []byte, shared []string) []string {
	var doc struct {
		SheetData struct {
			Row []struct {
				Cells []struct {
					R  string `xml:"r,attr"`
					T  string `xml:"t,attr"`
					V  string `xml:"v"`
					IS struct {
						T string `xml:"t"`
					} `xml:"is"`
				} `xml:"c"`
			} `xml:"row"`
		} `xml:"sheetData"`
	}
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil
	}

	var rows []string
	for _, r := range doc.SheetData.Row {
		var cells []string
		empty := true
		for _, c := range r.Cells {
			var v string
			switch c.T {
			case "s":
				if i, err := strconv.Atoi(c.V); err == nil && i >= 0 && i < len(shared) {
					v = shared[i]
				}
			case "inlineStr":
				v = c.IS.T
			default:
				// "v" holds the cached value for formula cells too, which is
				// what we want: the number, not the expression.
				v = c.V
			}
			v = strings.TrimSpace(v)
			if v != "" {
				empty = false
			}
			cells = append(cells, v)
		}
		if empty {
			continue
		}
		rows = append(rows, strings.Join(cells, "\t"))
	}
	return rows
}

// ---------- pptx ----------

func extractPptx(data []byte) (string, error) {
	zr, err := openZip(data)
	if err != nil {
		return "", err
	}

	type slide struct {
		n    int
		path string
	}
	var slides []slide
	numRe := regexp.MustCompile(`slide(\d+)\.xml$`)
	for _, f := range zr.File {
		if m := numRe.FindStringSubmatch(f.Name); m != nil && strings.HasPrefix(f.Name, "ppt/slides/") {
			n, _ := strconv.Atoi(m[1])
			slides = append(slides, slide{n: n, path: f.Name})
		}
	}
	sort.Slice(slides, func(i, j int) bool { return slides[i].n < slides[j].n })

	var out strings.Builder
	for _, s := range slides {
		raw, err := readEntry(zr, s.path)
		if err != nil {
			continue
		}
		text := allCharData(raw, "t")
		notes := ""
		notePath := strings.Replace(s.path, "ppt/slides/", "ppt/notesSlides/notesSlide", 1)
		if nraw, err := readEntry(zr, notePath); err == nil {
			notes = allCharData(nraw, "t")
		}
		if strings.TrimSpace(text) == "" && strings.TrimSpace(notes) == "" {
			continue
		}
		fmt.Fprintf(&out, "# Slide %d\n%s\n", s.n, strings.TrimSpace(text))
		if strings.TrimSpace(notes) != "" {
			fmt.Fprintf(&out, "Notes: %s\n", strings.TrimSpace(notes))
		}
		out.WriteByte('\n')
	}
	return collapse(out.String()), nil
}

// ---------- odt ----------

func extractODT(data []byte) (string, error) {
	zr, err := openZip(data)
	if err != nil {
		return "", err
	}
	raw, err := readEntry(zr, "content.xml")
	if err != nil {
		return "", err
	}
	// ODF is also zip+XML: same reader, different element names.
	return collapse(allCharDataWithBreaks(raw, map[string]bool{"p": true, "h": true, "table-row": true})), nil
}

// ---------- rtf ----------

var (
	rtfControl = regexp.MustCompile(`\\[a-zA-Z]+-?\d* ?`)
	rtfHexEsc  = regexp.MustCompile(`\\'[0-9a-fA-F]{2}`)
	rtfGroups  = regexp.MustCompile(`[{}]`)
)

func extractRTF(data []byte) string {
	s := string(data)
	// Drop embedded binary groups before stripping control words.
	for _, junk := range []string{`\\pict`, `\\object`, `\\fonttbl`, `\\colortbl`, `\\stylesheet`} {
		s = dropGroup(s, junk)
	}
	s = rtfHexEsc.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, `\par`, "\n")
	s = strings.ReplaceAll(s, `\line`, "\n")
	s = rtfControl.ReplaceAllString(s, "")
	s = rtfGroups.ReplaceAllString(s, "")
	return collapse(s)
}

func dropGroup(s, marker string) string {
	re := regexp.MustCompile(`(?s)\{` + marker + `.*?\}`)
	return re.ReplaceAllString(s, "")
}

// ---------- shared ----------

func allCharData(raw []byte, elem string) string {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var b strings.Builder
	capture := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == elem {
				capture = true
			}
		case xml.CharData:
			if capture {
				b.Write(t)
			}
		case xml.EndElement:
			if t.Name.Local == elem {
				capture = false
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

func allCharDataWithBreaks(raw []byte, breakOn map[string]bool) string {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.CharData:
			b.Write(t)
		case xml.EndElement:
			if breakOn[t.Name.Local] {
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

var (
	multiBlank = regexp.MustCompile(`\n{3,}`)
	multiSpace = regexp.MustCompile(`[ \t]{2,}`)
)

func collapse(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = multiSpace.ReplaceAllString(s, " ")
	s = multiBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
