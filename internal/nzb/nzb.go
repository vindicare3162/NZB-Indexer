// Package nzb generates spec-compliant NZB XML documents from a release's
// stored article segments. The NZB format is defined by the Newzbin DTD at
// http://www.newzbin.com/DTD/2003/nzb.
package nzb

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

// Namespace is the NZB XML namespace.
const Namespace = "http://www.newzbin.com/DTD/2003/nzb"

// docType is the DOCTYPE line emitted before the root element.
const docType = `<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">`

// Segment is one article within a file.
type Segment struct {
	// MessageID is the article message-id without angle brackets.
	MessageID string
	// Bytes is the article size in bytes.
	Bytes int64
	// Number is the 1-based segment ordinal within the file.
	Number int
}

// File describes one posted file and its segments.
type File struct {
	// Poster is the From header of the post.
	Poster string
	// Subject is the article subject (should contain the yEnc filename).
	Subject string
	// Date is the posting time.
	Date time.Time
	// Groups are the newsgroups the file was posted to.
	Groups []string
	// Segments are the article segments, in order.
	Segments []Segment
}

// Build renders the given files into an NZB XML document (including the XML
// declaration and DOCTYPE). Segments within each file are emitted in the order
// provided; callers should pre-sort by segment number.
func Build(files []File) ([]byte, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("nzb: no files to render")
	}

	doc := xmlNZB{Xmlns: Namespace}
	for _, f := range files {
		xf := xmlFile{
			Poster:  f.Poster,
			Date:    f.Date.Unix(),
			Subject: f.Subject,
		}
		for _, g := range f.Groups {
			if g = strings.TrimSpace(g); g != "" {
				xf.Groups.Group = append(xf.Groups.Group, g)
			}
		}
		for _, seg := range f.Segments {
			if seg.MessageID == "" {
				continue
			}
			xf.Segments.Segment = append(xf.Segments.Segment, xmlSegment{
				Bytes:  seg.Bytes,
				Number: seg.Number,
				Value:  seg.MessageID,
			})
		}
		if len(xf.Segments.Segment) == 0 {
			continue // skip files with no usable segments
		}
		doc.Files = append(doc.Files, xf)
	}

	if len(doc.Files) == 0 {
		return nil, fmt.Errorf("nzb: no files had usable segments")
	}

	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("nzb: marshal: %w", err)
	}

	var b strings.Builder
	b.WriteString(xml.Header) // <?xml version="1.0" encoding="UTF-8"?>\n
	b.WriteString(docType)
	b.WriteByte('\n')
	b.Write(body)
	b.WriteByte('\n')
	return []byte(b.String()), nil
}

// --- XML marshalling types ---

type xmlNZB struct {
	XMLName xml.Name  `xml:"nzb"`
	Xmlns   string    `xml:"xmlns,attr"`
	Files   []xmlFile `xml:"file"`
}

type xmlFile struct {
	Poster   string      `xml:"poster,attr"`
	Date     int64       `xml:"date,attr"`
	Subject  string      `xml:"subject,attr"`
	Groups   xmlGroups   `xml:"groups"`
	Segments xmlSegments `xml:"segments"`
}

type xmlGroups struct {
	Group []string `xml:"group"`
}

type xmlSegments struct {
	Segment []xmlSegment `xml:"segment"`
}

type xmlSegment struct {
	Bytes  int64  `xml:"bytes,attr"`
	Number int    `xml:"number,attr"`
	Value  string `xml:",chardata"`
}
