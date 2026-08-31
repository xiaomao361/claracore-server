package main

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"claracore-server/internal/config"
)

func TestNewHTTPServerAppliesResourceBoundaries(t *testing.T) {
	handler := http.NewServeMux()
	limits := config.HTTP{
		ReadHeaderTimeout: 7 * time.Second,
		ReadTimeout:       19 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       43 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	srv := newHTTPServer("127.0.0.1:12345", handler, limits, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if srv.Addr != "127.0.0.1:12345" || srv.Handler != handler {
		t.Fatal("server address or handler was not preserved")
	}
	if srv.ReadHeaderTimeout != limits.ReadHeaderTimeout || srv.ReadTimeout != limits.ReadTimeout ||
		srv.WriteTimeout != limits.WriteTimeout || srv.IdleTimeout != limits.IdleTimeout ||
		srv.MaxHeaderBytes != limits.MaxHeaderBytes {
		t.Fatalf("server limits = %+v, want %+v", srv, limits)
	}
	if srv.ErrorLog == nil {
		t.Fatal("net/http errors are not routed through the application logger")
	}
}
