package nntp

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseOverviewLine(t *testing.T) {
	// Standard XOVER/OVER format: number\tsubject\tfrom\tdate\tmsgid\trefs\tbytes\tlines
	line := "12345\t\"Some.Release.mkv\" yEnc (1/50)\tposter@example.com\tMon, 02 Jan 2006 15:04:05 -0700\t<abc@host>\t\t524288\t400"
	ov, ok := parseOverviewLine(line)
	if !ok {
		t.Fatal("expected line to parse")
	}
	if ov.ArticleNumber != 12345 {
		t.Errorf("ArticleNumber = %d, want 12345", ov.ArticleNumber)
	}
	if ov.Subject != `"Some.Release.mkv" yEnc (1/50)` {
		t.Errorf("Subject = %q", ov.Subject)
	}
	if ov.From != "poster@example.com" {
		t.Errorf("From = %q", ov.From)
	}
	if ov.MessageID != "abc@host" {
		t.Errorf("MessageID = %q, want abc@host (brackets stripped)", ov.MessageID)
	}
	if ov.Bytes != 524288 {
		t.Errorf("Bytes = %d, want 524288", ov.Bytes)
	}
	if ov.Date.IsZero() {
		t.Error("Date should have parsed")
	}
}

func TestParseOverviewLineTooFewFields(t *testing.T) {
	if _, ok := parseOverviewLine("123\tsubject\tfrom"); ok {
		t.Error("expected short line to be rejected")
	}
}

func TestParseOverviewLineSanitizesInvalidUTF8(t *testing.T) {
	// A subject and poster carrying a raw Latin-1 byte (0xfc = 'ü') — invalid
	// UTF-8. The parsed fields must be valid UTF-8 so they can be stored in a
	// UTF-8 TEXT column without failing the insert.
	line := "1\tGr\xfc\xdfe.Release.mkv\tp\xfcster@x\tMon, 02 Jan 2006 15:04:05 -0700\t<m@h>\t\t100\t1"
	ov, ok := parseOverviewLine(line)
	if !ok {
		t.Fatal("expected line to parse")
	}
	if !utf8.ValidString(ov.Subject) {
		t.Errorf("Subject is not valid UTF-8: %q", ov.Subject)
	}
	if !utf8.ValidString(ov.From) {
		t.Errorf("From is not valid UTF-8: %q", ov.From)
	}
	// Valid ASCII content is preserved around the replaced bytes.
	if !strings.Contains(ov.Subject, "Release.mkv") {
		t.Errorf("Subject lost its ASCII content: %q", ov.Subject)
	}
}

func TestParseNNTPDate(t *testing.T) {
	cases := []string{
		"Mon, 02 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 -0700",
		"2 Jan 2006 15:04:05 -0700",
	}
	for _, s := range cases {
		if _, err := parseNNTPDate(s); err != nil {
			t.Errorf("parseNNTPDate(%q) failed: %v", s, err)
		}
	}
	if _, err := parseNNTPDate("not a date"); err == nil {
		t.Error("expected error for unparseable date")
	}
}

func TestParsedDateValue(t *testing.T) {
	ts, err := parseNNTPDate("Mon, 02 Jan 2006 15:04:05 +0000")
	if err != nil {
		t.Fatal(err)
	}
	if !ts.Equal(time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)) {
		t.Errorf("parsed time = %v", ts)
	}
}
