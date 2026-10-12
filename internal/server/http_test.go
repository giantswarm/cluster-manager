package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/cluster-manager/internal/api"
)

func TestProbesAndMCPWithoutOAuth(t *testing.T) {
	srv, err := New(Config{Addr: "127.0.0.1:0", MCPEnabled: true}, api.NewMCPServer(nil, "test"), nil)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(path string) int {
		resp, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, http.StatusOK, get("/healthz"))
	assert.Equal(t, http.StatusOK, get("/readyz"))
	assert.Equal(t, http.StatusNotFound, get("/api/v1/clusters"), "there is no REST API")
	assert.NotEqual(t, http.StatusNotFound, get("/mcp"), "the MCP endpoint is mounted")
}

// A body beyond the bound is refused, and the endpoint keeps answering.
func TestMCPBodyIsBounded(t *testing.T) {
	srv, err := New(Config{Addr: "127.0.0.1:0", MCPEnabled: true}, api.NewMCPServer(nil, "test"), nil)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	post := func(body string) int {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", maxMCPBodyBytes) + `"}}`
	assert.GreaterOrEqual(t, post(big), 400)
	assert.Equal(t, http.StatusOK, post(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
}
