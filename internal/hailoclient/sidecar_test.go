package hailoclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnsureNoopWhenHealthy(t *testing.T) {
	srv := fakeSidecar(t, true)
	defer srv.Close()
	var spawned int32
	s := NewSidecar(New(srv.URL, time.Second), func() error { atomic.AddInt32(&spawned, 1); return nil }, time.Second)
	if err := s.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if spawned != 0 {
		t.Fatal("spawned although already healthy")
	}
}

func TestEnsureSpawnsThenWaitsForHealth(t *testing.T) {
	var up int32
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&up) == 0 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{"enabled":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	spawn := func() error { go func() { time.Sleep(150 * time.Millisecond); atomic.StoreInt32(&up, 1) }(); return nil }
	s := NewSidecar(New(srv.URL, time.Second), spawn, 3*time.Second)
	if err := s.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureRefusesWithoutSpawn(t *testing.T) {
	s := NewSidecar(New("http://127.0.0.1:1", 200*time.Millisecond), nil, time.Second)
	if err := s.Ensure(context.Background()); err == nil || !errors.Is(err, ErrNoSidecarCmd) {
		t.Fatalf("want ErrNoSidecarCmd, got %v", err)
	}
}

func TestEnsureSpawnFailureIsLoud(t *testing.T) {
	s := NewSidecar(New("http://127.0.0.1:1", 200*time.Millisecond), func() error { return errors.New("boom") }, time.Second)
	if err := s.Ensure(context.Background()); err == nil {
		t.Fatal("spawn error must propagate")
	}
}

func TestEnsureTimesOutIfNeverHealthy(t *testing.T) {
	s := NewSidecar(New("http://127.0.0.1:1", 200*time.Millisecond), func() error { return nil }, 600*time.Millisecond)
	if err := s.Ensure(context.Background()); err == nil {
		t.Fatal("a sidecar that never answers must time out, not hang")
	}
}
