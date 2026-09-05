package store

import (
	"context"
	"fmt"
	"time"
)

// GroupHealthLevel is a coarse per-group health classification (#127).
type GroupHealthLevel string

const (
	// HealthOK means the group is scanning successfully and keeping up.
	HealthOK GroupHealthLevel = "ok"
	// HealthWarn means the group needs attention (behind, slow to refresh, or
	// a few recent failures) but is not broken.
	HealthWarn GroupHealthLevel = "warn"
	// HealthError means the group is failing repeatedly or badly stalled.
	HealthError GroupHealthLevel = "error"
	// HealthUnknown means there is not enough information yet (never scanned,
	// or inactive).
	HealthUnknown GroupHealthLevel = "unknown"
)

// GroupHealthThresholds parameterise the health classifier so operators can
// tune what counts as a warning or an error (#127). Zero values disable the
// corresponding check.
type GroupHealthThresholds struct {
	// LagWarn/LagError: forward article lag (server_high - last_scanned_high)
	// at or above which the group is warned/errored.
	LagWarn  int64
	LagError int64
	// StaleWarn/StaleError: age of the last successful pass at or above which
	// the group is warned/errored (a group not refreshed in a long time).
	StaleWarn  time.Duration
	StaleError time.Duration
	// FailuresWarn/FailuresError: consecutive failed passes at or above which
	// the group is warned/errored.
	FailuresWarn  int
	FailuresError int
}

// DefaultGroupHealthThresholds returns sensible defaults used when no explicit
// configuration is provided.
func DefaultGroupHealthThresholds() GroupHealthThresholds {
	return GroupHealthThresholds{
		LagWarn:       50_000,
		LagError:      500_000,
		StaleWarn:     6 * time.Hour,
		StaleError:    24 * time.Hour,
		FailuresWarn:  1,
		FailuresError: 5,
	}
}

// GroupHealth is a group's derived health for admin display (#127).
type GroupHealth struct {
	Level GroupHealthLevel `json:"level"`
	// Reasons lists the human-readable signals that drove the classification.
	Reasons []string `json:"reasons,omitempty"`
	// Lag is the forward article lag (0 when unknown).
	Lag int64 `json:"lag"`
}

// worst returns the more severe of two levels (error > warn > ok > unknown).
func worst(a, b GroupHealthLevel) GroupHealthLevel {
	rank := map[GroupHealthLevel]int{HealthUnknown: 0, HealthOK: 1, HealthWarn: 2, HealthError: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// ClassifyGroupHealth derives a group's health from its retained scan signals
// and the configured thresholds. now is injectable for deterministic tests.
// Inactive or never-successfully-scanned groups are HealthUnknown.
func ClassifyGroupHealth(g Group, t GroupHealthThresholds, now time.Time) GroupHealth {
	h := GroupHealth{Level: HealthUnknown}
	if g.ServerHigh > 0 {
		if lag := g.ServerHigh - g.LastScannedHigh; lag > 0 {
			h.Lag = lag
		}
	}
	if !g.Active {
		h.Reasons = append(h.Reasons, "group inactive")
		return h
	}

	level := HealthOK

	// Consecutive failures.
	if t.FailuresError > 0 && g.ConsecutiveFailures >= t.FailuresError {
		level = worst(level, HealthError)
		h.Reasons = append(h.Reasons, fmt.Sprintf("%d consecutive failures", g.ConsecutiveFailures))
	} else if t.FailuresWarn > 0 && g.ConsecutiveFailures >= t.FailuresWarn {
		level = worst(level, HealthWarn)
		h.Reasons = append(h.Reasons, fmt.Sprintf("%d consecutive failures", g.ConsecutiveFailures))
	}

	// Forward lag.
	if t.LagError > 0 && h.Lag >= t.LagError {
		level = worst(level, HealthError)
		h.Reasons = append(h.Reasons, fmt.Sprintf("%d articles behind", h.Lag))
	} else if t.LagWarn > 0 && h.Lag >= t.LagWarn {
		level = worst(level, HealthWarn)
		h.Reasons = append(h.Reasons, fmt.Sprintf("%d articles behind", h.Lag))
	}

	// Staleness of the last successful pass.
	if g.LastSuccessAt == nil {
		// Active but never succeeded yet: unknown unless failing (handled above).
		if level == HealthOK {
			h.Reasons = append(h.Reasons, "never scanned successfully")
			h.Level = HealthUnknown
			return h
		}
	} else {
		age := now.Sub(*g.LastSuccessAt)
		if t.StaleError > 0 && age >= t.StaleError {
			level = worst(level, HealthError)
			h.Reasons = append(h.Reasons, fmt.Sprintf("no success in %s", age.Round(time.Minute)))
		} else if t.StaleWarn > 0 && age >= t.StaleWarn {
			level = worst(level, HealthWarn)
			h.Reasons = append(h.Reasons, fmt.Sprintf("no success in %s", age.Round(time.Minute)))
		}
	}

	h.Level = level
	return h
}

// GroupHealthStats is a bounded, aggregate view of group freshness for metrics
// (#129). It deliberately carries only scalar aggregates — never per-group rows
// — so exposing it as Prometheus gauges cannot create unbounded cardinality.
type GroupHealthStats struct {
	// ActiveGroups is the number of active groups.
	ActiveGroups int64
	// GroupsBehind is how many active groups have positive forward lag.
	GroupsBehind int64
	// MaxLag is the largest forward lag (server_high - last_scanned_high) across
	// active groups (0 when none is behind or no head is known).
	MaxLag int64
	// TotalLag is the summed forward lag across active groups.
	TotalLag int64
	// GroupsFailing is how many active groups have >= 1 consecutive failure.
	GroupsFailing int64
	// MaxConsecutiveFailures is the largest consecutive-failure count.
	MaxConsecutiveFailures int64
	// OldestSuccessAgeSeconds is the age, in seconds, of the least-recently
	// successful active group's last success (0 when none has ever succeeded or
	// there are no active groups).
	OldestSuccessAgeSeconds float64
	// GroupsNeverScanned is how many active groups have never succeeded.
	GroupsNeverScanned int64
}

// GroupHealthStats returns the aggregate group-freshness signals in a single
// query for metrics scraping (#129).
func (s *Store) GroupHealthStats(ctx context.Context) (GroupHealthStats, error) {
	const q = `
SELECT
    count(*)                                                                          AS active,
    count(*) FILTER (WHERE server_high > 0 AND server_high - last_scanned_high > 0)    AS behind,
    coalesce(max(GREATEST(server_high - last_scanned_high, 0)) FILTER (WHERE server_high > 0), 0) AS max_lag,
    coalesce(sum(GREATEST(server_high - last_scanned_high, 0)) FILTER (WHERE server_high > 0), 0) AS total_lag,
    count(*) FILTER (WHERE consecutive_failures > 0)                                   AS failing,
    coalesce(max(consecutive_failures), 0)                                            AS max_fails,
    coalesce(max(EXTRACT(EPOCH FROM (now() - last_success_at)))::float8, 0)            AS oldest_success_age,
    count(*) FILTER (WHERE last_success_at IS NULL)                                    AS never_scanned
FROM groups
WHERE active = TRUE`
	var st GroupHealthStats
	err := s.pool.QueryRow(ctx, q).Scan(
		&st.ActiveGroups, &st.GroupsBehind, &st.MaxLag, &st.TotalLag,
		&st.GroupsFailing, &st.MaxConsecutiveFailures, &st.OldestSuccessAgeSeconds,
		&st.GroupsNeverScanned)
	if err != nil {
		return GroupHealthStats{}, fmt.Errorf("group health stats: %w", err)
	}
	return st, nil
}

// GroupStorage estimates the retained raw-part storage for one group in bytes
// (#127), used for the storage-impact health signal.
func (s *Store) GroupStorageBytes(ctx context.Context, ids []int64) (map[int64]int64, error) {
	out := make(map[int64]int64, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT group_id, COALESCE(SUM(bytes), 0) FROM parts WHERE group_id = ANY($1) GROUP BY group_id`,
		ids)
	if err != nil {
		return nil, fmt.Errorf("group storage bytes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var gid, bytes int64
		if err := rows.Scan(&gid, &bytes); err != nil {
			return nil, fmt.Errorf("scan group storage: %w", err)
		}
		out[gid] = bytes
	}
	return out, rows.Err()
}
