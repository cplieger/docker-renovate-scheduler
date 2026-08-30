package main

// --- Trigger payload ---
//
// The request line one `run` client sends; the wire is
// scheduler/v4/trigger's contract.

// runPayload is the request line a `run` client sends after connecting.
type runPayload struct {
	// Repos are positional repository slugs restricting the run; empty means
	// Renovate's own repositories / autodiscover configuration decides.
	Repos []string `json:"repos,omitempty"`
	// Env is the client's complete environment, forwarded to the Renovate
	// child with its RENOVATE_* values unchanged — what preserves the
	// documented passthrough, so a `docker exec -e RENOVATE_X=… … run`
	// override rides along. The daemon rewrites one entry, DUMB_INIT_SETSID
	// to 0, keeping the child in the process group it created (see
	// withDumbInitInGroup). The socket is same-user and in-container, so
	// this crosses no trust boundary the process environment doesn't already
	// cross. Neither the library nor the daemon ever logs it (credentials).
	Env []string `json:"env,omitempty"`
}
