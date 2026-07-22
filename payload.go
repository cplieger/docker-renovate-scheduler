package main

// --- Trigger payload ---
//
// The request one `run` client submits to the daemon over the scheduler
// library's trigger broker (scheduler/v2/trigger: newline-JSON wire, one
// request line per connection, queued/started/done events back). Client and
// daemon ship in the same binary inside the same image, so there is no
// version skew to negotiate. Keep added fields optional (omitempty)
// regardless: an unset field is simply absent from the frame, and the
// argless sibling schedulers share this wire shape with an empty payload.

// runPayload is the request line a `run` client sends after connecting.
type runPayload struct {
	// Repos are positional repository slugs restricting the run; empty means
	// Renovate's own repositories / autodiscover configuration decides.
	Repos []string `json:"repos,omitempty"`
	// Env is the client's complete environment. The daemon starts the
	// Renovate child with exactly this environment, which is what preserves
	// the documented RENOVATE_* passthrough for triggered runs: a
	// `docker exec -e RENOVATE_X=… … run` override rides along unchanged.
	// The socket is same-user and in-container, so this crosses no trust
	// boundary the process environment doesn't already cross. Neither the
	// library nor the daemon ever logs it (it can carry credentials).
	Env []string `json:"env,omitempty"`
}
