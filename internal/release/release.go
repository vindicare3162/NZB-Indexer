package release

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"strconv"

	"github.com/google/uuid"
	"github.com/vindicare/goindex/internal/store"
)

// Repo is the subset of the store the release builder needs.
type Repo interface {
	ListCompleteUnreleasedBinaries(ctx context.Context, limit int) ([]store.Binary, error)
	CreateRelease(ctx context.Context, in store.ReleaseInput) (store.Release, bool, error)
	MarkBinariesReleased(ctx context.Context, ids []int64) error
}

// Options controls release building.
type Options struct {
	// BatchLimit bounds how many binaries are promoted per Build call.
	BatchLimit int
}

// Builder promotes complete binaries into releases.
type Builder struct {
	repo Repo
	log  *slog.Logger
	opts Options
}

// New creates a release Builder.
func New(repo Repo, log *slog.Logger, opts Options) *Builder {
	if opts.BatchLimit <= 0 {
		opts.BatchLimit = 500
	}
	if log == nil {
		log = slog.Default()
	}
	return &Builder{repo: repo, log: log, opts: opts}
}

// Result summarises one build pass.
type Result struct {
	Created    int
	Duplicates int
	Processed  int
}

// Build promotes complete, unreleased binaries into releases. Binaries whose
// release hash collides with an existing release are treated as duplicates:
// the binary is still marked released (so it is not reprocessed) but no new
// release row is created.
func (b *Builder) Build(ctx context.Context) (Result, error) {
	var res Result

	bins, err := b.repo.ListCompleteUnreleasedBinaries(ctx, b.opts.BatchLimit)
	if err != nil {
		return res, fmt.Errorf("list complete binaries: %w", err)
	}

	var releasedIDs []int64
	for _, bin := range bins {
		res.Processed++

		// For a collection binary the norm_subject is the collection key
		// ("base/count"); name it from the collection base. Post-processing may
		// later replace this with a real name recovered from the PAR2.
		subject := bin.NormSubject
		if bin.CollectionKey != "" {
			subject = collectionBaseName(bin.CollectionKey)
		}
		name := CleanName(subject)
		if name == "" {
			name = subject
		}
		cat := Categorize(name)
		hash := ReleaseHash(name, bin.TotalBytes, bin.GroupID)

		in := store.ReleaseInput{
			GUID:            uuid.NewString(),
			Name:            name,
			OriginalSubject: subject,
			SearchName:      SearchName(name),
			CategoryID:      &cat,
			GroupID:         &bin.GroupID,
			BinaryID:        &bin.ID,
			Poster:          bin.Poster,
			TotalParts:      bin.TotalParts,
			SizeBytes:       bin.TotalBytes,
			PostedAt:        bin.PostedAt,
			ReleaseHash:     hash,
		}

		_, created, err := b.repo.CreateRelease(ctx, in)
		if err != nil {
			return res, fmt.Errorf("create release for binary %d: %w", bin.ID, err)
		}
		if created {
			res.Created++
		} else {
			res.Duplicates++
		}
		releasedIDs = append(releasedIDs, bin.ID)
	}

	if err := b.repo.MarkBinariesReleased(ctx, releasedIDs); err != nil {
		return res, fmt.Errorf("mark binaries released: %w", err)
	}

	b.log.Info("release build pass complete",
		"processed", res.Processed, "created", res.Created, "duplicates", res.Duplicates)
	return res, nil
}

// ReleaseHash computes a stable deduplication fingerprint from the normalized
// name, a coarse size bucket, and the group. Two posts of the same content
// (identical name, near-identical size, same group) collapse to one release.
//
// The size is bucketed to the nearest 1% so trivial byte differences between
// re-posts do not defeat deduplication.
func ReleaseHash(name string, sizeBytes, groupID int64) string {
	search := SearchName(name)
	bucket := sizeBucket(sizeBytes)

	h := sha1.New()
	h.Write([]byte(search))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(bucket, 10)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(groupID, 10)))
	return hex.EncodeToString(h.Sum(nil))
}

// sizeBucket rounds a byte count to a coarse bucket (nearest 1%) so re-posts
// with minor size drift hash identically. Zero and tiny sizes map to 0.
func sizeBucket(size int64) int64 {
	if size <= 0 {
		return 0
	}
	// Round to the nearest 1% of the value using its order of magnitude.
	f := float64(size)
	step := math.Pow(10, math.Floor(math.Log10(f))-1) // ~1% granularity
	if step < 1 {
		step = 1
	}
	return int64(math.Round(f/step)) * int64(step)
}
