// Package health serves a tiny HTTP endpoint for systemd / Docker / k8s
// liveness + readiness checks. It exposes:
//
//   GET /healthz   →  200 OK with no body (liveness — process is alive)
//   GET /readyz    →  200 OK if MQTT is connected, 503 otherwise
//   GET /metrics   →  JSON snapshot of the bridge's counters
//
// The server is read-only and binds to localhost-only by default. Set
// LB_HEALTH_ADDR=0.0.0.0:9090 to expose it container-wide for Docker
// healthchecks.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// StatsFunc returns whatever metric snapshot the embedder cares to expose
// at /metrics. Decoupled as a function rather than a typed interface so
// the bridge's stats struct stays in its own package.
type StatsFunc func() any

// MQTTStatus reports the broker connection state. Implemented by mqtt.Client.
type MQTTStatus interface {
	IsConnected() bool
}

type Server struct {
	addr   string
	stats  StatsFunc
	mqtt   MQTTStatus
	logger *slog.Logger
}

func New(addr string, stats StatsFunc, mqtt MQTTStatus, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		addr:   addr,
		stats:  stats,
		mqtt:   mqtt,
		logger: logger.With("component", "health"),
	}
}

// Serve blocks until ctx is done. The HTTP server has a 5s shutdown
// grace period — long enough for in-flight checks to drain, short enough
// to not delay daemon shutdown noticeably.
func (s *Server) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.mqtt != nil && !s.mqtt.IsConnected() {
			http.Error(w, "MQTT disconnected", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.stats())
	})

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.logger.Info("health server listening", "addr", s.addr)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}
