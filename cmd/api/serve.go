package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"
)

// serve runs srv until ctx is cancelled, then stops accepting connections and
// gives in-flight requests up to grace to finish before closing them.
// A clean shutdown returns nil.
func serve(ctx context.Context, srv *http.Server, grace time.Duration) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh: // failed to start (e.g. port in use)
		return err
	case <-ctx.Done():
	}

	log.Printf("shutting down: draining requests (up to %s)", grace)
	shutCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		_ = srv.Close() // grace expired: cut the remaining connections
		return err
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
