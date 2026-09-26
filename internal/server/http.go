// Package server assembles the single HTTP listener: health endpoints and the
// MCP streamable-HTTP endpoint (optionally behind OAuth). cluster-manager has
// no REST API: MCP is its only surface (bumblebee-plans#46 D2).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Config configures the listener.
type Config struct {
	Addr       string
	MCPEnabled bool
	MCPPath    string
	// CommitPath is where the commit MCP server is served with the GitHub
	// pin (OAuth.GitHub): the endpoint of the App-pinned registration
	// (empty: DefaultCommitPath).
	CommitPath string
	// OAuth, when set, makes the server an OAuth 2.1 resource server: the MCP
	// endpoint requires a bearer token the platform IdP
	// issued (forwarded by muster / sent by the portal) or this server's own,
	// and every call carries the caller's identity. Off: anonymous, acting as
	// the ServiceAccount — only for a server nothing but a trusted proxy can
	// reach.
	OAuth *OAuthConfig
}

// DefaultCommitPath is the commit MCP path.
const DefaultCommitPath = "/commit/mcp"

// Server is the assembled HTTP server.
type Server struct {
	http  *http.Server
	oauth *oauthRuntime
	log   *slog.Logger
}

// New builds the server: mcpSrv on the main path behind the IdP guard, and,
// with the GitHub pin, commitSrv on the commit path behind the GitHub guard
// and the IdP guard.
func New(cfg Config, mcpSrv, commitSrv *mcpserver.MCPServer, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.MCPPath == "" {
		cfg.MCPPath = "/mcp"
	}
	if cfg.CommitPath == "" {
		cfg.CommitPath = DefaultCommitPath
	}
	pinned := cfg.OAuth != nil && cfg.OAuth.GitHub != nil
	if pinned != (commitSrv != nil) {
		return nil, fmt.Errorf("the commit MCP server and the GitHub pin go together: the commit path is served behind the pin only")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s := &Server{log: log}
	if cfg.OAuth != nil {
		o, err := newOAuth(*cfg.OAuth, cfg.MCPPath, log)
		if err != nil {
			return nil, err
		}
		o.register(mux)
		s.oauth = o
	}

	if cfg.MCPEnabled && mcpSrv != nil {
		mux.Handle(cfg.MCPPath, s.guard(mcpserver.NewStreamableHTTPServer(mcpSrv,
			mcpserver.WithEndpointPath(cfg.MCPPath),
		)))
	}
	if cfg.MCPEnabled && pinned {
		mux.Handle(cfg.CommitPath, s.oauth.protectCommit(mcpserver.NewStreamableHTTPServer(commitSrv,
			mcpserver.WithEndpointPath(cfg.CommitPath),
		)))
	}

	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           otelhttp.NewHandler(mux, "cluster-manager", otelhttp.WithFilter(traced)),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: MCP streams outlive any fixed value.
		IdleTimeout: 120 * time.Second,
	}
	return s, nil
}

// traced leaves the probes out of the traces: the kubelet calls them every
// few seconds and they carry no caller.
func traced(r *http.Request) bool {
	return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
}

// guard requires an authenticated caller when OAuth is on.
func (s *Server) guard(next http.Handler) http.Handler {
	if s.oauth == nil {
		return next
	}
	return s.oauth.protect(next)
}

// Handler exposes the mux (tests).
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Run serves until ctx is done, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("listening", "addr", s.http.Addr)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if s.oauth != nil {
		s.oauth.shutdown(shutdownCtx)
	}
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	s.log.Info("server stopped")
	return nil
}
