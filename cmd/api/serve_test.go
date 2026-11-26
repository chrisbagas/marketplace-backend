package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A request that is in flight when the shutdown signal arrives must finish.
func TestServeDrainsInFlightRequest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // serve() listens itself; we only needed a free port

	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(500 * time.Millisecond)
		io.WriteString(w, "done")
	})
	srv := &http.Server{Addr: addr, Handler: mux}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, 5*time.Second) }()

	type result struct {
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		var resp *http.Response
		var err error
		for range 50 { // wait until the listener is up
			resp, err = http.Get("http://" + addr + "/slow")
			if err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			res <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		res <- result{string(b), err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow request never started")
	}
	cancel() // simulates SIGTERM while the request is running

	r := <-res
	if r.err != nil || r.body != "done" {
		t.Fatalf("in-flight request was cut: body=%q err=%v", r.body, r.err)
	}
	if err := <-served; err != nil {
		t.Fatalf("serve returned %v, want nil", err)
	}
	if _, err := http.Get("http://" + addr + "/slow"); err == nil {
		t.Fatal("server still accepts connections after shutdown")
	}
}
