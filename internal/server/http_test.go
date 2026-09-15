package server

import (
	"net/http"
	"net/http/httptest"
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
