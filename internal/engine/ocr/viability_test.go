package ocr

import "testing"

// OCR over a photograph — a room, a building exterior — produces hundreds of
// tokens and no words. These are verbatim from a real corpus, where they
// passed the old 20-alphanumeric-character gate and then scored spuriously
// against unrelated questions.
func TestEnoughTextRejectsPhotographNoise(t *testing.T) {
	for name, s := range map[string]string{
		"kitchen photo":  "| | | | | fare я — = р = ——,_ | —< ГИ — <i я — Se |: - а!",
		"bedroom photo":  "ees ee Pee eo oo № \\ ь ee | \\ ee A Е oe Se к a ee Е",
		"entrance photo": "bo 2 | | 4 ' | | | | im = ] bad om | 1 | № № | 5 a 1",
		"camera noise":   "% т = Sen tl cs ™ © — ги oO i — © > =. = = и © -—— =",
		"mixed script":   "| ie a ™ a q gf, 3 А в. oaow Е соо S&° of Ff ie. = о Yo",
	} {
		if EnoughText(s) {
			t.Errorf("%s: accepted, but it contains no words", name)
		}
	}
}

// Real content must survive. Samples are whole extractions, which is what the
// gate actually sees — judged on a truncated preview even a 47-word payment
// screenshot looks marginal, and calibrating against previews would set the
// threshold far too low to catch photographs.
func TestEnoughTextKeepsRealContent(t *testing.T) {
	for name, s := range map[string]string{
		"payment screenshot": "Reference no. E1907241453 Amount $4,333.55 To LAKES GRAMMAR - AN ANGLICAN SCHOOL",
		"transfer receipt":   "< Transaction details & You paid - $226.65 to Vitality Dental Tuggerah",
		// Imperfect OCR of a phone banking screenshot. The bilingual language
		// pack confuses glyphs ("Бе" for "be", "С" for "C"), so a good
		// extraction is not a clean one — but a whole screenshot carries
		// plenty of words. In the corpus this document holds 47.
		"imperfect ocr": "7:43 atl 5G Gy < Transfer & pay Pay С | description won't Бе sent. " +
			"Please check details before paying. From Everyday Account available balance " +
			"To LAKES GRAMMAR ANGLICAN SCHOOL Amount Description Schedule payment",
		"cyrillic prose": "Привет Света пришел счет за воду где мы живем подтверди пожалуйста",
	} {
		if !EnoughText(s) {
			t.Errorf("%s: rejected, but it is real content", name)
		}
	}
}

// Digits neither make a word nor break one: a reference number is not a word,
// but a token that merely contains a digit still is.
func TestEnoughTextDigitHandling(t *testing.T) {
	if EnoughText("E1907241453 74363961321 8276075 4333 55 226 65") {
		t.Error("bare reference numbers should not count as words")
	}
	if !EnoughText("Grammar2 Anglican3 School4 Invoice5 Payment6") {
		t.Error("words containing digits should still count")
	}
}

func TestEnoughTextEmpty(t *testing.T) {
	if EnoughText("") || EnoughText("   \n\t ") {
		t.Error("empty extraction must not pass")
	}
}
