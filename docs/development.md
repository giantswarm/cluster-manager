# Developing on cluster-manager

Run `go test ./...` and the repository's pre-commit checks before submitting changes.

Caller-client construction failures stop the operation and never select ServiceAccount clients.
Expired caller tokens remain on Kubernetes requests so the API server's rejection is returned.
