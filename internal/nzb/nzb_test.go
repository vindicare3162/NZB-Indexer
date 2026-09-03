package nzb

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestBuildStructureAndRoundTrip(t *testing.T) {
	files := []File{
		{
			Poster:  "poster@example.com",
			Subject: `"Great.Movie.2024.mkv" yEnc (1/3)`,
			Date:    time.Unix(1_700_000_000, 0),
			Groups:  []string{"alt.binaries.movies", "alt.binaries.test"},
			Segments: []Segment{
				{MessageID: "seg1@host", Bytes: 100, Number: 1},
				{MessageID: "seg2@host", Bytes: 200, Number: 2},
				{MessageID: "seg3@host", Bytes: 150, Number: 3},
			},
		},
	}

	data, err := Build(files)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := string(data)

	// Header and DOCTYPE present.
	if !strings.HasPrefix(s, xml.Header) {
		t.Error("missing XML declaration")
	}
	if !strings.Contains(s, "<!DOCTYPE nzb PUBLIC") {
		t.Error("missing DOCTYPE")
	}
	if !strings.Contains(s, Namespace) {
		t.Errorf("missing namespace %q", Namespace)
	}

	// Round-trip parse and validate counts.
	var parsed struct {
		XMLName xml.Name `xml:"nzb"`
		Files   []struct {
			Poster   string `xml:"poster,attr"`
			Date     int64  `xml:"date,attr"`
			Subject  string `xml:"subject,attr"`
			Groups   struct {
				Group []string `xml:"group"`
			} `xml:"groups"`
			Segments struct {
				Segment []struct {
					Bytes  int64  `xml:"bytes,attr"`
					Number int    `xml:"number,attr"`
					Value  string `xml:",chardata"`
				} `xml:"segment"`
			} `xml:"segments"`
		} `xml:"file"`
	}
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(parsed.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(parsed.Files))
	}
	f := parsed.Files[0]
	if f.Poster != "poster@example.com" {
		t.Errorf("poster = %q", f.Poster)
	}
	if f.Date != 1_700_000_000 {
		t.Errorf("date = %d", f.Date)
	}
	if len(f.Groups.Group) != 2 {
		t.Errorf("groups = %d, want 2", len(f.Groups.Group))
	}
	if len(f.Segments.Segment) != 3 {
		t.Fatalf("segments = %d, want 3", len(f.Segments.Segment))
	}
	if f.Segments.Segment[1].Number != 2 || f.Segments.Segment[1].Value != "seg2@host" {
		t.Errorf("segment 2 = %+v", f.Segments.Segment[1])
	}
	if f.Segments.Segment[0].Bytes != 100 {
		t.Errorf("segment 1 bytes = %d, want 100", f.Segments.Segment[0].Bytes)
	}
}

func TestBuildSkipsEmptySegments(t *testing.T) {
	files := []File{
		{
			Poster:  "p",
			Subject: "s",
			Segments: []Segment{
				{MessageID: "", Bytes: 10, Number: 1}, // dropped
				{MessageID: "keep@host", Bytes: 20, Number: 2},
			},
		},
	}
	data, err := Build(files)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := strings.Count(string(data), "<segment "); got != 1 {
		t.Errorf("expected 1 segment after dropping empty, got %d:\n%s", got, data)
	}
}

func TestBuildErrors(t *testing.T) {
	if _, err := Build(nil); err == nil {
		t.Error("expected error for no files")
	}
	// A file with only empty segments yields no usable files.
	if _, err := Build([]File{{Poster: "p", Segments: []Segment{{MessageID: ""}}}}); err == nil {
		t.Error("expected error when no files have usable segments")
	}
}

func TestSafeFilename(t *testing.T) {
	if got := safeFilename(`bad/name:with*chars?`); strings.ContainsAny(got, `/:*?`) {
		t.Errorf("safeFilename left illegal chars: %q", got)
	}
	if got := safeFilename(""); got != "release" {
		t.Errorf("empty name = %q, want release", got)
	}
}
