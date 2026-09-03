package postprocess

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
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
	// FetchTimeout bounds a single article-body fetch, so a stalled read on the
	// NNTP connection cannot block the whole post-processing pass. Zero means a
	// sensible default.
	FetchTimeout time.Duration
	// Concurrency is how many releases are post-processed in parallel. Releases
	// are independent, so a bounded worker pool lets a pass use the NNTP
	// connection budget instead of fetching one release at a time. Zero means a
	// small default; it should not exceed the NNTP pool size.
	Concurrency int
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
	if opts.FetchTimeout <= 0 {
		opts.FetchTimeout = 30 * time.Second
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
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

	// Releases are independent (each fetches and applies its own result), so
	// process them with a bounded worker pool. This lets a pass use the NNTP
	// connection budget instead of fetching one release at a time, and a slow
	// release no longer blocks the rest.
	workers := p.opts.Concurrency
	if workers > len(pending) {
		workers = len(pending)
	}
	if workers < 1 {
		workers = 1
	}

	var (
		mu       sync.Mutex
		fatal    error // first non-per-release error (e.g. DB apply failure)
		wg       sync.WaitGroup
		jobs     = make(chan store.PendingRelease)
		stop     = make(chan struct{}) // closed once when a worker hits a fatal error
		stopOnce sync.Once
	)
	abort := func() { stopOnce.Do(func() { close(stop) }) }

	worker := func() {
		defer wg.Done()
		for pr := range jobs {
			if ctx.Err() != nil {
				return
			}
			out, retry := p.processOne(ctx, pr)
			if retry {
				// A needed fetch errored and nothing was recovered — likely a
				// transient failure. Mark failed; it will be retried on a later
				// pass until MaxPPAttempts is reached (tracked in the store).
				p.log.Warn("post-processing fetch failed; will retry", "release", pr.Release.GUID)
				mu.Lock()
				res.Processed++
				res.Failed++
				mu.Unlock()
				if serr := p.repo.SetReleasePPStatus(ctx, pr.Release.ID, store.PPFailed); serr != nil {
					mu.Lock()
					if fatal == nil {
						fatal = serr
					}
					mu.Unlock()
					abort()
					return
				}
				continue
			}
			if aerr := p.repo.ApplyPostProcessing(ctx, pr.Release.ID, out); aerr != nil {
				mu.Lock()
				if fatal == nil {
					fatal = fmt.Errorf("apply post-processing for %s: %w", pr.Release.GUID, aerr)
				}
				mu.Unlock()
				abort()
				return
			}
			mu.Lock()
			res.Processed++
			if out.Name != "" {
				res.Renamed++
			}
			if out.NFO != nil {
				res.NFOFound++
			}
			mu.Unlock()
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go worker()
	}
sendLoop:
	for _, pr := range pending {
		select {
		case <-ctx.Done():
			break sendLoop
		case <-stop: // a worker hit a fatal error; stop dispatching
			break sendLoop
		case jobs <- pr:
		}
	}
	close(jobs)
	wg.Wait()

	if fatal != nil {
		return res, fatal
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	p.log.Info("post-processing pass complete",
		"processed", res.Processed, "renamed", res.Renamed,
		"nfo_found", res.NFOFound, "failed", res.Failed,
		"workers", workers)
	return res, nil
}

// processOne handles a single release, returning the outcome to apply.
//
// Two strategies run within a shared fetch budget:
//  1. Subject-hinted: segments whose subject reveals a .par2/.nfo filename are
//     fetched directly (cheap, works for well-labelled posts).
//  2. Content-probed: when the release name looks obfuscated (random hex/
//     base64 with no real words) and PAR2 was not already recovered, probe the
//     remaining segments and identify PAR2 by its file magic rather than the
//     subject. This is what recovers real names for obfuscated releases.
func (p *Processor) processOne(ctx context.Context, pr store.PendingRelease) (store.ReleasePPResult, bool) {
	var res store.ReleasePPResult

	par2Segs, nfoSegs, otherSegs := classifySegments(pr.Segments)

	fetched := 0
	budget := p.opts.MaxFetchPerRelease
	fetchErr := false // a fetch we attempted actually errored (transient/retryable)

	fetchDecoded := func(messageID string) ([]byte, bool) {
		if fetched >= budget {
			return nil, false
		}
		fetched++
		// Bound each fetch so a stalled read can't block the whole pass.
		fctx, cancel := context.WithTimeout(ctx, p.opts.FetchTimeout)
		body, err := p.fetch.Body(fctx, messageID)
		cancel()
		if err != nil {
			fetchErr = true
			return nil, false
		}
		decoded, err := DecodeYenc(body)
		if err != nil {
			// Some servers return already-decoded bodies; fall back to raw.
			decoded = body
		}
		return decoded, true
	}

	// (1) Subject-hinted PAR2.
	for _, seg := range par2Segs {
		decoded, ok := fetchDecoded(seg.MessageID)
		if !ok {
			if fetched >= budget {
				break
			}
			continue
		}
		if best := namefromPar2(decoded); best != "" {
			res.Name = best
			res.SearchName = release.SearchName(best)
			break
		}
	}

	// (2) Content-probe for obfuscated releases when no PAR2 name yet.
	if res.Name == "" && isObfuscated(pr.Release.Name) {
		for _, seg := range otherSegs {
			if fetched >= budget {
				break
			}
			decoded, ok := fetchDecoded(seg.MessageID)
			if !ok {
				continue
			}
			if !HasPar2Magic(decoded) {
				continue
			}
			if best := namefromPar2(decoded); best != "" {
				res.Name = best
				res.SearchName = release.SearchName(best)
				break
			}
		}
	}

	// When a real name was recovered, re-categorize from it: the release may
	// have been built (and categorized) from an obfuscated name.
	if res.Name != "" {
		cat := release.Categorize(res.Name)
		res.CategoryID = &cat
	}

	// (3) NFO text.
	for _, seg := range nfoSegs {
		if fetched >= budget {
			break
		}
		decoded, ok := fetchDecoded(seg.MessageID)
		if !ok {
			continue
		}
		if text := sanitizeNFO(decoded); text != "" {
			res.NFO = &text
			break
		}
	}

	// Report a retry signal: nothing was recovered but a fetch we attempted
	// errored, so this is likely a transient failure worth retrying rather than
	// a release with nothing to recover.
	retry := fetchErr && res.Name == "" && res.NFO == nil
	return res, retry
}

// namefromPar2 parses PAR2 filenames from a decoded body and returns the best
// release name, or "" when the body is not usable PAR2.
func namefromPar2(decoded []byte) string {
	names, err := ParsePar2Filenames(decoded)
	if err != nil || len(names) == 0 {
		return ""
	}
	return bestReleaseName(names)
}

// classifySegments splits segments into subject-hinted PAR2, subject-hinted
// NFO, and everything else (candidates for content probing). Smaller segments
// are preferred first in the "other" bucket, since PAR2 index packets are
// typically among the smaller articles in a post.
func classifySegments(segs []store.PartSegment) (par2, nfo, other []store.PartSegment) {
	for _, s := range segs {
		name := filenameFromSubject(s.Subject)
		switch {
		case LooksLikePAR2(name):
			par2 = append(par2, s)
		case LooksLikeNFO(name):
			nfo = append(nfo, s)
		default:
			other = append(other, s)
		}
	}
	sort.SliceStable(other, func(i, j int) bool { return other[i].Bytes < other[j].Bytes })
	return par2, nfo, other
}

// reHexLike matches a token that is entirely hex digits (a common obfuscation).
var reHexLike = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)

// isObfuscated reports whether a release name looks like a random/obfuscated
// identifier (hex or base64-ish with no meaningful words), rather than a
// human-readable scene/release name. Such releases are the ones worth probing
// for a PAR2-recovered real name.
func isObfuscated(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	// A pure long hex string is almost certainly obfuscated.
	if reHexLike.MatchString(name) {
		return true
	}

	// Split on common separators and inspect the tokens.
	fields := strings.FieldsFunc(name, func(r rune) bool {
		return r == ' ' || r == '.' || r == '-' || r == '_'
	})
	if len(fields) == 0 {
		return true
	}

	// Count tokens that look like real words vs random tokens. A name is
	// considered obfuscated when it has no word-like tokens and at least one
	// long random-looking token.
	hasWord := false
	hasRandom := false
	for _, f := range fields {
		if isWordLike(f) {
			hasWord = true
			continue
		}
		// A long non-word token (hex, base64, letter/digit soup) is random.
		if len(f) >= 8 {
			hasRandom = true
		}
	}
	if hasWord {
		return false
	}
	return hasRandom
}

// isWordLike reports whether a token resembles a real word rather than a
// random/base64 identifier. A real word is a run of >=3 letters that is all
// lowercase, all uppercase, or Title-case (leading capital then lowercase),
// contains no digits, and includes at least one vowel. Mixed-case runs like
// "PERuop" or letter/digit soup like "4Mg2PER" are rejected.
func isWordLike(tok string) bool {
	if len(tok) < 3 {
		return false
	}
	hasDigit := false
	letters := 0
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letters++
		}
	}
	if hasDigit || letters < 3 || letters != len([]rune(tok)) {
		return false // real words in release names don't mix digits into a token
	}
	if !isWordCase(tok) {
		return false // reject mixed-case (base64-like) runs
	}
	return hasVowel(tok)
}

// isWordCase reports whether s is all-lower, all-upper, or Title-case.
func isWordCase(s string) bool {
	rs := []rune(s)
	allLower, allUpper := true, true
	for _, r := range rs {
		if r >= 'A' && r <= 'Z' {
			allLower = false
		}
		if r >= 'a' && r <= 'z' {
			allUpper = false
		}
	}
	if allLower || allUpper {
		return true
	}
	// Title-case: first upper, remainder lower.
	if rs[0] >= 'A' && rs[0] <= 'Z' {
		for _, r := range rs[1:] {
			if r >= 'A' && r <= 'Z' {
				return false
			}
		}
		return true
	}
	return false
}

func hasVowel(s string) bool {
	for _, r := range strings.ToLower(s) {
		switch r {
		case 'a', 'e', 'i', 'o', 'u', 'y':
			return true
		}
	}
	return false
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

// reVolSuffix matches a trailing archive/parity volume suffix such as
// ".vol01+02", ".part007", or ".r05", so all volumes of a set collapse to the
// same base name.
var reVolSuffix = regexp.MustCompile(`(?i)\.(vol\d+\+\d+|part\d+|r\d+|\d{2,3})$`)

// stripKnownExt removes a trailing archive/media/par2 extension and any volume
// suffix left behind (e.g. ".part1.rar" or ".vol01+02.par2"), so every file of
// a recovery set reduces to the same clean base name.
func stripKnownExt(name string) string {
	lower := strings.ToLower(name)
	for _, ext := range []string{".par2", ".rar", ".zip", ".7z", ".mkv", ".mp4", ".avi", ".nfo", ".sfv"} {
		if strings.HasSuffix(lower, ext) {
			name = name[:len(name)-len(ext)]
			// Strip any volume suffix now exposed (".part1", ".vol01+02",
			// ".r05") so recovery volumes share one base name.
			return reVolSuffix.ReplaceAllString(name, "")
		}
	}
	// No known extension: still strip a bare trailing volume suffix
	// (e.g. "<base>.r05" or "<base>.part07").
	return reVolSuffix.ReplaceAllString(name, "")
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
