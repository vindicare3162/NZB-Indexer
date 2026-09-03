package newznab

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/vindicare/goindex/internal/store"
)

// buildQuery combines the free-text q with TV season/episode hints so that a
// tvsearch for a specific episode narrows the text match. This is a pragmatic
// approach for a header-only indexer that lacks a scene-name database.
func buildQuery(q url.Values) string {
	base := strings.TrimSpace(q.Get("q"))

	season := strings.TrimSpace(q.Get("season"))
	ep := strings.TrimSpace(q.Get("ep"))
	if season != "" {
		// Render as SxxEyy when both present, else just the season token.
		if n, err := strconv.Atoi(season); err == nil {
			token := "s" + pad2(n)
			if ep != "" {
				if en, err := strconv.Atoi(ep); err == nil {
					token += "e" + pad2(en)
				}
			}
			base = strings.TrimSpace(base + " " + token)
		}
	}
	return base
}

// pad2 zero-pads a small integer to at least two digits.
func pad2(n int) string {
	s := strconv.Itoa(n)
	if len(s) < 2 {
		return "0" + s
	}
	return s
}

// parseCategories parses a comma-separated cat parameter into ints, ignoring
// invalid entries.
func parseCategories(s string) []int {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// parseInt parses s as an int, returning def on failure.
func parseInt(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

// buildCapsCategories groups flat categories into parent/subcat structure.
func buildCapsCategories(cats []store.Category) capsCategories {
	// Index children by parent id.
	children := map[int][]capsSubcat{}
	var parents []store.Category
	for _, c := range cats {
		if c.ParentID == nil {
			parents = append(parents, c)
		} else {
			children[*c.ParentID] = append(children[*c.ParentID], capsSubcat{ID: c.ID, Name: c.Name})
		}
	}

	var out capsCategories
	for _, p := range parents {
		out.Category = append(out.Category, capsCategory{
			ID:     p.ID,
			Name:   p.Name,
			Subcat: children[p.ID],
		})
	}
	return out
}

// writeXML marshals v as an XML document with the standard header.
func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(v)
}

// writeError writes a Newznab error document. Newznab errors are returned with
// HTTP 200 and an <error> body carrying a code; some clients also accept
// non-200 statuses, but 200 is the widely-compatible choice.
func writeError(w http.ResponseWriter, code int, desc string) {
	writeXML(w, http.StatusOK, nnError{Code: code, Description: desc})
}
