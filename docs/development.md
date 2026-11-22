# Developing on cluster-manager

Run `go test ./...` and the repository's pre-commit checks before submitting changes.

Caller-client construction failures stop the operation and never select ServiceAccount clients.
Expired caller tokens remain on Kubernetes requests so the API server's rejection is returned.

OAuth provider, storage and cleanup construction uses `mcp-oauth/oauthconfig` with
resolved settings. Existing flags and environment precedence apply before setup.
MCP challenges advertise the endpoint's protected-resource metadata URL, and
downstream authentication uses provider ID tokens only after validation.
