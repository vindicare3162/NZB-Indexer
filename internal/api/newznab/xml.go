// Package newznab implements a Newznab-compatible XML API so that tools like
// Sonarr, Radarr, and Prowlarr can search goindex and download NZBs.
package newznab

import "encoding/xml"

// The Newznab namespace used for <newznab:attr> elements.
const newznabNS = "http://www.newznab.com/DTD/2010/feeds/attributes/"

// --- caps (t=caps) ---

type caps struct {
	XMLName    xml.Name       `xml:"caps"`
	Server     capsServer     `xml:"server"`
	Limits     capsLimits     `xml:"limits"`
	Searching  capsSearching  `xml:"searching"`
	Categories capsCategories `xml:"categories"`
}

type capsServer struct {
	Version   string `xml:"version,attr"`
	Title     string `xml:"title,attr"`
	Strapline string `xml:"strapline,attr"`
	Email     string `xml:"email,attr,omitempty"`
	URL       string `xml:"url,attr,omitempty"`
}

type capsLimits struct {
	Max     int `xml:"max,attr"`
	Default int `xml:"default,attr"`
}

type capsSearching struct {
	Search      capsSearch `xml:"search"`
	TVSearch    capsSearch `xml:"tv-search"`
	MovieSearch capsSearch `xml:"movie-search"`
	AudioSearch capsSearch `xml:"audio-search"`
	BookSearch  capsSearch `xml:"book-search"`
}

type capsSearch struct {
	Available       string `xml:"available,attr"`       // "yes" | "no"
	SupportedParams string `xml:"supportedParams,attr"` // comma list
}

type capsCategories struct {
	Category []capsCategory `xml:"category"`
}

type capsCategory struct {
	ID     int           `xml:"id,attr"`
	Name   string        `xml:"name,attr"`
	Subcat []capsSubcat  `xml:"subcat"`
}

type capsSubcat struct {
	ID   int    `xml:"id,attr"`
	Name string `xml:"name,attr"`
}

// --- search feed (RSS with newznab attrs) ---

type rss struct {
	XMLName      xml.Name `xml:"rss"`
	Version      string   `xml:"version,attr"`
	NewznabXMLNS string   `xml:"xmlns:newznab,attr"`
	AtomXMLNS    string   `xml:"xmlns:atom,attr"`
	Channel      channel  `xml:"channel"`
}

type channel struct {
	Title       string      `xml:"title"`
	Description string      `xml:"description"`
	Link        string      `xml:"link"`
	Response    nnResponse  `xml:"newznab:response"`
	Items       []item      `xml:"item"`
}

type nnResponse struct {
	Offset int `xml:"offset,attr"`
	Total  int `xml:"total,attr"`
}

type item struct {
	Title     string     `xml:"title"`
	GUID      string     `xml:"guid"`
	Link      string     `xml:"link"`
	Comments  string     `xml:"comments,omitempty"`
	PubDate   string     `xml:"pubDate"`
	Category  string     `xml:"category,omitempty"`
	Enclosure enclosure  `xml:"enclosure"`
	Attrs     []nnAttr   `xml:"newznab:attr"`
}

type enclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

type nnAttr struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

// --- error response ---

type nnError struct {
	XMLName     xml.Name `xml:"error"`
	Code        int      `xml:"code,attr"`
	Description string   `xml:"description,attr"`
}
