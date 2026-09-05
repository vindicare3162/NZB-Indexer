package server

import (
	"log/slog"

	"github.com/vindicare/goindex/internal/config"
	"github.com/vindicare/goindex/internal/notify"
)

// buildNotifier constructs the webhook notification service from config (#137).
// It always returns a usable service; when no destination is enabled the
// service is inert (Emit is a no-op) but safe to Run and query for history.
func buildNotifier(cfg config.Config, log *slog.Logger) *notify.Service {
	dests := make([]notify.Destination, 0, len(cfg.Notify.Webhooks))
	for _, w := range cfg.Notify.Webhooks {
		if w.URL == "" {
			continue
		}
		events := make([]notify.EventType, 0, len(w.Events))
		for _, e := range w.Events {
			events = append(events, notify.EventType(e))
		}
		dests = append(dests, notify.Destination{
			Name:    w.Name,
			URL:     w.URL,
			Secret:  w.Secret,
			Events:  events,
			Enabled: w.Enabled,
		})
	}
	return notify.New(dests, notify.Options{
		Timeout:     cfg.Notify.Timeout,
		MaxAttempts: cfg.Notify.MaxAttempts,
	}, log)
}
