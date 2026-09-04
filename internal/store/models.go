// Package store provides PostgreSQL-backed persistence for goindex, including
// schema migrations, a pgx connection pool, and typed data access.
package store

import "time"

// Category is a Newznab-style category. Parent categories have ParentID nil.
type Category struct {
	ID          int    `json:"id"`
	ParentID    *int   `json:"parent_id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Group is an indexed newsgroup with scan-position bookkeeping.
type Group struct {
	ID               int64     `json:"id"`
	Name             string    `json:"name"`
	Active           bool      `json:"active"`
	LastScannedHigh  int64     `json:"last_scanned_high"`
	BackfillLow      int64     `json:"backfill_low"`
	BackfillComplete bool      `json:"backfill_complete"`
	// BackfillTargetDays overrides the global backfill day cutoff for this
	// group when non-nil (0 = no day bound).
	BackfillTargetDays *int `json:"backfill_target_days,omitempty"`
	// BackfillTargetArticles overrides the global per-pass article cap for this
	// group when non-nil (0 = unlimited).
	BackfillTargetArticles *int64 `json:"backfill_target_articles,omitempty"`

	// Per-group scan progress/error reporting (#114). These summarise the most
	// recent scan/backfill pass for the group.
	// LastScanAt is when the most recent pass completed (nil = never scanned).
	LastScanAt *time.Time `json:"last_scan_at,omitempty"`
	// LastScanBackfill is true when the most recent pass was a backfill.
	LastScanBackfill bool `json:"last_scan_backfill"`
	// LastScanArticles/LastScanParts are the articles pulled and parts inserted
	// by the most recent pass.
	LastScanArticles int64 `json:"last_scan_articles"`
	LastScanParts    int64 `json:"last_scan_parts"`
	// ServerHigh is the server's high-water article number observed during the
	// most recent pass; ServerHigh - LastScannedHigh is the forward lag.
	ServerHigh int64 `json:"server_high"`
	// LastScanError is the error message from the most recent pass ('' on
	// success); LastScanErrorAt is when that error occurred (nil = last pass ok).
	LastScanError   string     `json:"last_scan_error,omitempty"`
	LastScanErrorAt *time.Time `json:"last_scan_error_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Part is a single article header row belonging to a multi-part binary.
type Part struct {
	ID            int64
	GroupID       int64
	ArticleNumber int64
	MessageID     string
	Subject       string
	Poster        string
	PostedAt      *time.Time
	Bytes         int64
	PartNumber    int
	TotalParts    int
	NormSubject   string
	BinaryID      *int64
	CreatedAt     time.Time
}

// Binary is a collection of parts forming one posted file set.
type Binary struct {
	ID             int64
	GroupID        int64
	NormSubject    string
	Poster         string
	TotalParts     int
	CollectedParts int
	TotalBytes     int64
	PostedAt       *time.Time
	Complete       bool
	Released       bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// CollectionKey is non-empty when this binary represents a multi-file
	// collection; CollectionFiles is the number of files in it.
	CollectionKey   string
	CollectionFiles int
}

// Release is a searchable, categorized, human-named item.
type Release struct {
	ID              int64      `json:"id"`
	GUID            string     `json:"guid"`
	Name            string     `json:"name"`
	OriginalSubject string     `json:"original_subject"`
	SearchName      string     `json:"search_name"`
	CategoryID      *int       `json:"category_id,omitempty"`
	GroupID         *int64     `json:"group_id,omitempty"`
	BinaryID        *int64     `json:"binary_id,omitempty"`
	Poster          string     `json:"poster"`
	TotalParts      int        `json:"total_parts"`
	SizeBytes       int64      `json:"size_bytes"`
	PostedAt        *time.Time `json:"posted_at,omitempty"`
	ReleaseHash     string     `json:"-"`
	PPStatus        string     `json:"pp_status"`
	NFO             *string    `json:"nfo,omitempty"`
	Grabs           int64      `json:"grabs"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// ReleaseMetadata is optional external metadata matched to a release (TV show
// or movie): title, year, season/episode, cover art, and an overview.
type ReleaseMetadata struct {
	ReleaseID  int64     `json:"release_id"`
	Title      string    `json:"title"`
	Year       *int      `json:"year,omitempty"`
	Season     *int      `json:"season,omitempty"`
	Episode    *int      `json:"episode,omitempty"`
	Source     string    `json:"source"`
	ExternalID string    `json:"external_id"`
	PosterURL  string    `json:"poster_url"`
	Overview   string    `json:"overview"`
	Matched    bool      `json:"matched"`
	FetchedAt  time.Time `json:"fetched_at"`
}

// Segment is one NZB segment (article) within a release file.
type Segment struct {
	MessageID string `json:"message_id"`
	Bytes     int64  `json:"bytes"`
	Number    int    `json:"number"`
}

// ReleaseFile is a single file within a release plus its ordered segments.
type ReleaseFile struct {
	ID        int64     `json:"id"`
	ReleaseID int64     `json:"release_id"`
	FileName  string    `json:"file_name"`
	SizeBytes int64     `json:"size_bytes"`
	Segments  []Segment `json:"segments"`
	CreatedAt time.Time `json:"created_at"`
}

// Role constants for user accounts.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Post-processing status constants.
const (
	PPPending = "pending"
	PPDone    = "done"
	PPFailed  = "failed"
)

// User is a local account.
type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	Role         string    `json:"role"`
	RateLimit    int       `json:"rate_limit"`
	Active       bool      `json:"active"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// APIKey is a Newznab API key belonging to a user.
type APIKey struct {
	ID         int64      `json:"id"`
	UserID     int64      `json:"user_id"`
	APIKey     string     `json:"api_key"`
	Label      string     `json:"label"`
	Active     bool       `json:"active"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}
