package postprocess

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/vindicare/goindex/internal/release"
	"github.com/vindicare/goindex/internal/store"
)

// Fetcher fetches an article body by message-id. The NNTP pool satisfies this.
type Fetcher interface {
	Body(ctx context.Context, messageID string) ([]byte, error)
}

// Repo is the subset of the store the post-processor needs.
type Repo interface {
	ListPendingReleases(ctx context.Context, limit int) ([]store.PendingRelease, error)
	ApplyPostProcessing(ctx context.Context, id int64, res store.ReleasePPResult) error
	SetReleasePPStatus(ctx context.Context, id int64, status string) error
}

// Options controls post-processing.
type Options struct {
	// BatchLimit bounds releases processed per Run call.
	BatchLimit int
	// MaxFetchPerRelease caps how many article bodies are downloaded for a
	// single release (bandwidth/connection budget). Zero means a small
	// default.
	MaxFetchPerRelease int
}

// Processor recovers real names and NFO text for releases.
type Processor struct {
	fetch Fetcher
	repo  Repo
	log   *slog.Logger
	opts  Options
}

// New creates a Processor.
func New(fetch Fetcher, repo Repo, log *slog.Logger, opts Options) *Processor {
	if opts.BatchLimit <= 0 {
		opts.BatchLimit = 100
	}
	if opts.MaxFetchPerRelease <= 0 {
		opts.MaxFetchPerRelease = 4
	}
	if log == nil {
		log = slog.Default()
	}
	return &Processor{fetch: fetch, repo: repo, log: log, opts: opts}
}

// Result summarises one post-processing pass.
type Result struct {
	Processed int
	Renamed   int
	NFOFound  int
	Failed    int
}

// Run post-processes pending releases: it fetches candidate PAR2/NFO segments,
// recovers the real name from PAR2, extracts NFO text, and applies the result.
func (p *Processor) Run(ctx context.Context) (Result, error) {
	var res Result

	pending, err := p.repo.ListPendingReleases(ctx, p.opts.BatchLimit)
	if err != nil {
		return res, fmt.Errorf("list pending releases: %w", err)
	}

	for _, pr := range pending {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Processed++
		out, err := p.processOne(ctx, pr)
		if err != nil {
			res.Failed++
			// Mark failed so it is not retried forever; log and continue.
			p.log.Warn("post-processing failed", "release", pr.Release.GUID, "err", err)
			if serr := p.repo.SetReleasePPStatus(ctx, pr.Release.ID, store.PPFailed); serr != nil {
				return res, serr
			}
			continue
		}
		if out.Name != "" {
			res.Renamed++
		}
		if out.NFO != nil {
			res.NFOFound++
		}
		if err := p.repo.ApplyPostProcessing(ctx, pr.Release.ID, out); err != nil {
			return res, fmt.Errorf("apply post-processing for %s: %w", pr.Release.GUID, err)
		}
	}

	p.log.Info("post-processing pass complete",
		"processed", res.Processed, "renamed", res.Renamed,
		"nfo_found", res.NFOFound, "failed", res.Failed)
	return res, nil
}

// processOne handles a single release, returning the outcome to apply.
func (p *Processor) processOne(ctx context.Context, pr store.PendingRelease) (store.ReleasePPResult, error) {
	var res store.ReleasePPResult

	// Identify candidate PAR2 and NFO segments by their subject filename.
	par2Segs, nfoSegs := classifySegments(pr.Segments)

	fetched := 0
	budget := p.opts.MaxFetchPerRelease

	// PAR2 first: try to recover the real filename set.
	for _, seg := range par2Segs {
		if fetched >= budget {
			break
		}
		fetched++
		body, err := p.fetch.Body(ctx, seg.MessageID)
		if err != nil {
			continue // try the next candidate
		}
		decoded, err := DecodeYenc(body)
		if err != nil {
			// Some servers return already-decoded bodies; try raw.
			decoded = body
		}
		names, err := ParsePar2Filenames(decoded)
		if err != nil || len(names) == 0 {
			continue
		}
		if best := bestReleaseName(names); best != "" {
			res.Name = best
			res.SearchName = release.SearchName(best)
			break
		}
	}

	// NFO next: capture the NFO text.
	for _, seg := range nfoSegs {
		if fetched >= budget {
			break
		}
		fetched++
		body, err := p.fetch.Body(ctx, seg.MessageID)
		if err != nil {
			continue
		}
		decoded, err := DecodeYenc(body)
		if err != nil {
			decoded = body
		}
		text := sanitizeNFO(decoded)
		if text != "" {
			res.NFO = &text
			break
		}
	}

	return res, nil
}

// classifySegments splits segments into PAR2 and NFO candidates based on the
// filename embedded in each segment's subject.
func classifySegments(segs []store.PartSegment) (par2, nfo []store.PartSegment) {
	for _, s := range segs {
		name := filenameFromSubject(s.Subject)
		switch {
		case LooksLikePAR2(name):
			par2 = append(par2, s)
		case LooksLikeNFO(name):
			nfo = append(nfo, s)
		}
	}
	return par2, nfo
}

// filenameFromSubject extracts a quoted filename from a subject, or returns the
// trimmed subject when no quotes are present.
func filenameFromSubject(subject string) string {
	if i := strings.IndexByte(subject, '"'); i >= 0 {
		if j := strings.IndexByte(subject[i+1:], '"'); j >= 0 {
			return subject[i+1 : i+1+j]
		}
	}
	return strings.TrimSpace(subject)
}

// bestReleaseName picks the most representative name from PAR2 filenames. It
// prefers the base name of the recovery set (strips the archive/volume
// extension) and returns the longest common-looking base.
func bestReleaseName(names []string) string {
	best := ""
	for _, n := range names {
		base := stripKnownExt(n)
		if len(base) > len(best) {
			best = base
		}
	}
	return strings.TrimSpace(best)
}

// reVolSuffix matches a PAR2 recovery-volume suffix like ".vol01+02".
var reVolSuffix = regexp.MustCompile(`(?i)\.vol\d+\+\d+$`)

// stripKnownExt removes a trailing archive/media/par2 extension (and any PAR2
// recovery-volume suffix) from a name, so all volumes of a set collapse to the
// same base name.
func stripKnownExt(name string) string {
	lower := strings.ToLower(name)
	for _, ext := range []string{".par2", ".rar", ".zip", ".7z", ".mkv", ".mp4", ".avi", ".nfo", ".sfv"} {
		if strings.HasSuffix(lower, ext) {
			name = name[:len(name)-len(ext)]
			// A PAR2 file may be "<base>.vol01+02.par2"; strip the vol suffix
			// left behind so recovery volumes share one base name.
			return reVolSuffix.ReplaceAllString(name, "")
		}
	}
	// Also strip .rNN / .partNN volume suffixes.
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		suffix := strings.ToLower(name[i+1:])
		if len(suffix) >= 2 && (suffix[0] == 'r' || strings.HasPrefix(suffix, "part")) {
			return name[:i]
		}
	}
	return name
}

// sanitizeNFO converts decoded NFO bytes into clean UTF-8 text. NFO files are
// commonly encoded in CP437 (box-drawing art); invalid UTF-8 bytes are
// replaced so the result is safe to store and display.
func sanitizeNFO(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	if utf8.Valid(data) {
		return strings.TrimSpace(string(data))
	}
	// Replace invalid bytes with the Unicode replacement character.
	var b strings.Builder
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 {
			b.WriteRune('\uFFFD')
			data = data[1:]
			continue
		}
		b.WriteRune(r)
		data = data[size:]
	}
	return strings.TrimSpace(b.String())
}
