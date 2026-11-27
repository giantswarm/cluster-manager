package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResourceSpecificOAuthChallenge(t *testing.T) {
	runtime, err := newOAuth(OAuthConfig{Settings: googleSettings(t)}, "/mcp", slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() { runtime.shutdown(context.WithoutCancel(t.Context())) })
	mux := http.NewServeMux()
	runtime.register(mux)
	mux.Handle("/mcp", runtime.protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("missing credential accepted") })))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	metadataURL := "https://manager.example.test/.well-known/oauth-protected-resource/mcp"
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Contains(t, response.Header().Get("WWW-Authenticate"), `resource_metadata="`+metadataURL+`"`)
	require.NotContains(t, response.Header().Get("WWW-Authenticate"), "error=")
	metadata := httptest.NewRecorder()
	mux.ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, metadataURL, nil))
	require.Equal(t, http.StatusOK, metadata.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(metadata.Body.Bytes(), &body))
	require.Equal(t, "https://manager.example.test/mcp", body["resource"])
}
