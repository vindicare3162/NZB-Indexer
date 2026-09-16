package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vindicare/goindex/internal/config"
	"github.com/vindicare/goindex/internal/store"
)

var (
	fileCounterRE = regexp.MustCompile(`^[^\[\]]*\[(\d{1,6})\s*/\s*(\d{1,6})\]`)
	quotedNameRE   = regexp.MustCompile(`"([^"]+)"`)
	segPartRE      = regexp.MustCompile(`[\(\[](\d{1,6})\s*/\s*(\d{1,6})[\)\]]`)
	collectionVolExtRE = regexp.MustCompile(`(?i)(\.part\d{1,5}|\.vol\d{1,6}\+\d{1,6}|\.r\d{2,4}|\.\d{3,4})?\.(rar|par2|zip|7z|sfv|nfo|nzb|001)$`)
	trailingNumRE = regexp.MustCompile(`\.\d{3,4}$`)
)

func runFixCollections(args []string) error {
	fs := flag.NewFlagSet("goindex fix-collections", flag.ContinueOnError)
	configPath := fs.String("config", envOr("GOINDEX_CONFIG", ""), "path to YAML config file")
	batchSize := fs.Int("batch", 500, "binaries per batch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	st, err := store.Open(ctx, cfg.Database.DSNString(), cfg.Database.MaxConns)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// Step 1: Query stub binary IDs
	logger.Info("finding stub binaries")
	rows, err := st.Pool().Query(ctx, `SELECT DISTINCT r.binary_id FROM releases r WHERE r.size_bytes < 5000000 AND r.total_parts <= 3 AND r.pp_status = 'done' AND r.name ~ '^[A-Za-z0-9.]{15,}$' ORDER BY r.binary_id`)
	if err != nil {
		return fmt.Errorf("query stub binaries: %w", err)
	}
	var allIDs []int64
	for rows.Next() {
		var id int64; rows.Scan(&id); allIDs = append(allIDs, id)
	}
	rows.Close()
	logger.Info("found stub binaries", "count", len(allIDs))
	if len(allIDs) == 0 { logger.Info("nothing to fix"); return nil }

	// Step 2: Process in batches. For each batch, query parts with binary_id index + LIKE filter.
	updatedParts := 0
	affectedBins := make(map[int64]bool)
	for i := 0; i < len(allIDs); i += *batchSize {
		end := i + *batchSize; if end > len(allIDs) { end = len(allIDs) }
		batch := allIDs[i:end]

		prows, err := st.Pool().Query(ctx, `SELECT p.id, p.subject, p.binary_id FROM parts p WHERE p.binary_id = ANY($1) AND p.subject LIKE '%[%/%]%' ORDER BY p.binary_id, p.id`, batch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "query batch %d: %v", i, err)
			continue
		}

		for prows.Next() {
			var id, bid int64; var subject string
			prows.Scan(&id, &subject, &bid)
			affectedBins[bid] = true

			cm := fileCounterRE.FindStringSubmatch(subject)
			if cm == nil { continue }
			fileNum, _ := strconv.Atoi(cm[1]); fileTotal, _ := strconv.Atoi(cm[2])
			if fileTotal < 2 { continue }
			qm := quotedNameRE.FindStringSubmatch(subject)
			if qm == nil { continue }
			fileName := qm[1]

			// Guard: single multi-segment file, not a collection.
			segMatch := segPartRE.FindStringSubmatch(subject)
			if segMatch != nil {
				segTotal, _ := strconv.Atoi(segMatch[2])
				if fileNum == 1 && fileTotal == segTotal && segTotal > 0 { continue }
			}

			// Derive collection base name.
			base := collectionBaseName(fileName)
			if base == "" { base = fileName }
			key := base + "/" + strconv.Itoa(fileTotal)
			st.Pool().Exec(ctx, `UPDATE parts SET collection_key = $1, file_number = $2, collection_files = $3 WHERE id = $4`, key, fileNum, fileTotal, id)
			updatedParts++
		}
		prows.Close()
	}

	logger.Info("backfilled", "parts", updatedParts, "affected_binaries", len(affectedBins))

	// Step 3: Delete stub binaries and releases
	if len(affectedBins) > 0 {
		logger.Info("cleaning up stub binaries")
		var binList []int64
		for bid := range affectedBins { binList = append(binList, bid) }
		for i := 0; i < len(binList); i += *batchSize {
			end := i + *batchSize; if end > len(binList) { end = len(binList) }
			batch := binList[i:end]
			st.Pool().Exec(ctx, `UPDATE parts p SET binary_id = NULL FROM (SELECT unnest($1::bigint[]) AS id) b WHERE p.binary_id = b.id`, batch)
			st.Pool().Exec(ctx, `DELETE FROM releases r WHERE r.binary_id = ANY($1)`, batch)
			st.Pool().Exec(ctx, `DELETE FROM binaries b WHERE b.id = ANY($1)`, batch)
		}
	}

	logger.Info("fix-collections complete", "parts_updated", updatedParts, "binaries_cleaned", len(affectedBins))
	return nil
}

func collectionBaseName(fileName string) string {
	base := fileName
	if loc := trailingNumRE.FindStringIndex(base); loc != nil && loc[0] > 0 {
		stripped := base[:loc[0]]
		if collectionVolExtRE.MatchString(stripped) { return stripVolExt(stripped) }
	}
	if b := stripVolExt(base); b != "" { return b }
	if collectionVolExtRE.MatchString(base) {
		extIdx := strings.LastIndex(base, ".")
		if extIdx > 0 { return base[:extIdx] }
	}
	return ""
}

func stripVolExt(name string) string {
	m := collectionVolExtRE.FindStringSubmatch(name)
	if m == nil { return "" }
	return strings.TrimSuffix(name, m[0])
}

