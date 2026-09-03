package metrics

import "strings"

// RoutePattern maps a concrete request path to a bounded, low-cardinality route
// label. It collapses variable segments (release GUIDs, ids) into fixed
// patterns so the metric series count stays small regardless of traffic. Any
// path that does not match a known shape is bucketed as "other".
func RoutePattern(path string) string {
	switch {
	case path == "/metrics":
		return "/metrics"
	case path == "/api" || path == "/api/":
		return "/api" // Newznab endpoint
	}

	// REST API under /api/v1. Normalise the variable tail segments.
	if rest, ok := trimPrefix(path, "/api/v1/"); ok {
		return "/api/v1/" + normalizeRest(rest)
	}

	if path == "/" {
		return "/"
	}
	// Everything else is the embedded SPA / static assets.
	return "spa"
}

func trimPrefix(s, prefix string) (string, bool) {
	if strings.HasPrefix(s, prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// normalizeRest maps the REST sub-path to a stable pattern with :id/:guid
// placeholders in the variable positions.
func normalizeRest(rest string) string {
	seg := strings.Split(rest, "/")
	switch seg[0] {
	case "releases":
		// releases, releases/{guid}, releases/{guid}/nzb
		switch len(seg) {
		case 1:
			return "releases"
		case 2:
			return "releases/:guid"
		default:
			if seg[2] == "nzb" {
				return "releases/:guid/nzb"
			}
			return "releases/:guid/*"
		}
	case "apikeys":
		if len(seg) > 1 {
			return "apikeys/:id"
		}
		return "apikeys"
	case "admin":
		return "admin/" + normalizeAdmin(seg[1:])
	default:
		// Fixed public/session endpoints (login, me, health, ready, setup,
		// categories) are already low-cardinality; return as-is.
		return seg[0]
	}
}

// normalizeAdmin maps admin sub-paths, replacing id segments with :id.
func normalizeAdmin(seg []string) string {
	if len(seg) == 0 {
		return ""
	}
	switch seg[0] {
	case "groups":
		// groups, groups/bulk, groups/{id}, groups/{id}/backfill
		switch len(seg) {
		case 1:
			return "groups"
		case 2:
			if seg[1] == "bulk" {
				return "groups/bulk"
			}
			return "groups/:id"
		default:
			return "groups/:id/" + seg[2]
		}
	case "servers":
		if len(seg) > 1 {
			return "servers/:id"
		}
		return "servers"
	case "users":
		if len(seg) > 1 {
			return "users/:id"
		}
		return "users"
	case "postprocess":
		return strings.Join(seg, "/") // postprocess, postprocess/retry-failed
	default:
		// scan, backfill, schedule, status, stats, logs, discover, etc.
		return seg[0]
	}
}
