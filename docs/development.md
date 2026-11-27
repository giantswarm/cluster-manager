# Developing on cluster-manager

## Identity and cluster creation

Creator RBAC uses an email username only when the provider verified that email.
An unverified or absent email leaves the creator out of the generated binding
and explains the omission in the result's RBAC note.

Without a local Dex provider, automatic cluster OIDC configuration requires one
unambiguous trusted issuer marked `openIDProvider`. Multiple marked issuers are
refused rather than selecting an authority by list order. This configuration
flag permits provider ID-token recovery; it is not a JWT token-type assertion.

## Checks

Run `go test ./...` and the repository's pre-commit checks before submitting changes.

Caller-client construction failures stop the operation and never select ServiceAccount clients.
Expired caller tokens remain on Kubernetes requests so the API server's rejection is returned.
Caller clients and their expiry-aware cache come from the independent `mcp-toolkit/k8s` module.
Local cluster configuration and explicit application clients belong to cluster-manager.

OAuth provider, storage and cleanup construction uses `mcp-oauth/oauthconfig` with
resolved settings. Existing flags and environment precedence apply before setup.
MCP challenges advertise the endpoint's protected-resource metadata URL, and
downstream authentication uses provider ID tokens only after validation.
