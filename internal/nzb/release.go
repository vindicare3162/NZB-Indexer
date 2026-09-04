package nzb

import (
	"context"
	"fmt"

	"github.com/vindicare/goindex/internal/store"
)

// Repo is the subset of the store the NZB builder needs.
type Repo interface {
	GetReleaseByGUID(ctx context.Context, guid string) (store.Release, error)
	GetReleaseSegments(ctx context.Context, releaseID int64) ([]store.PartSegment, error)
	GetGroupName(ctx context.Context, groupID int64) (string, error)
}

// Generator builds NZB documents for stored releases.
type Generator struct {
	repo Repo
}

// NewGenerator creates a Generator.
func NewGenerator(repo Repo) *Generator {
	return &Generator{repo: repo}
}

// ForGUID builds the NZB document for the release with the given public GUID.
// The whole release is emitted as a single NZB <file> whose segments are the
// release's ordered parts. (Per-file splitting is introduced once
// post-processing populates release_files.)
func (g *Generator) ForGUID(ctx context.Context, guid string) (data []byte, filename string, err error) {
	rel, err := g.repo.GetReleaseByGUID(ctx, guid)
	if err != nil {
		return nil, "", err
	}

	segs, err := g.repo.GetReleaseSegments(ctx, rel.ID)
	if err != nil {
		return nil, "", err
	}
	if len(segs) == 0 {
		return nil, "", fmt.Errorf("nzb: release %q has no segments (durable segments empty and no backing parts); it may predate durable-segment storage and its parts have been pruned — re-index the group or restore from backup", guid)
	}

	groupName := ""
	if rel.GroupID != nil {
		if gn, gerr := g.repo.GetGroupName(ctx, *rel.GroupID); gerr == nil {
			groupName = gn
		}
	}

	// Use the first segment's subject as the file subject (it carries the
	// yEnc filename); fall back to the release name.
	subject := rel.Name
	if len(segs) > 0 && segs[0].Subject != "" {
		subject = segs[0].Subject
	}

	file := File{
		Poster:  rel.Poster,
		Subject: subject,
	}
	if rel.PostedAt != nil {
		file.Date = *rel.PostedAt
	}
	if groupName != "" {
		file.Groups = []string{groupName}
	}
	for i, s := range segs {
		num := s.PartNumber
		if num <= 0 {
			num = i + 1
		}
		file.Segments = append(file.Segments, Segment{
			MessageID: s.MessageID,
			Bytes:     s.Bytes,
			Number:    num,
		})
	}

	data, err = Build([]File{file})
	if err != nil {
		return nil, "", err
	}
	return data, safeFilename(rel.Name) + ".nzb", nil
}

// safeFilename sanitises a release name for use as a download filename.
func safeFilename(name string) string {
	if name == "" {
		return "release"
	}
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\n', '\r', '\t':
			out = append(out, '_')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
