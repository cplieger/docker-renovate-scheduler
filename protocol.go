package main

// --- Trigger protocol (client <-> daemon, newline-delimited JSON) ---
//
// The `run` subcommand is a thin client: it forwards its request over the
// daemon's unix socket and waits for the result. Client and daemon ship in the
// same binary inside the same image, so there is no version skew to negotiate
// and the wire format carries no version field.

// wireRequest is the single request line a client sends after connecting.
type wireRequest struct {
	// Repos are positional repository slugs restricting the run; empty means
	// Renovate's own repositories / autodiscover configuration decides.
	Repos []string `json:"repos,omitempty"`
	// Env is the client's complete environment. The daemon starts the
	// Renovate child with exactly this environment, which is what preserves
	// the documented RENOVATE_* passthrough for triggered runs: a
	// `docker exec -e RENOVATE_X=… … run` override rides along unchanged.
	// The socket is same-user and in-container, so this crosses no trust
	// boundary the process environment doesn't already cross.
	Env []string `json:"env,omitempty"`
}

// wireEvent is one status line the daemon streams back. The client receives
// eventQueued on acceptance, eventStarted when the executor picks the request
// up (the gap between the two is queue wait behind an in-flight run), and
// exactly one eventDone as the final line.
type wireEvent struct {
	Event string `json:"event"`
	// Reason explains a not-OK outcome that isn't a plain Renovate failure
	// (queue full, cancelled by shutdown, base directory unwritable).
	Reason string `json:"reason,omitempty"`
	// DurationMs is the run's execution time on eventDone (0 when the request
	// never ran, e.g. cancelled or rejected).
	DurationMs int64 `json:"duration_ms,omitempty"`
	// OK is meaningful only on eventDone: the run's outcome (never omitted,
	// so a failed run is explicit on the wire).
	OK bool `json:"ok"`
}

const (
	eventQueued  = "queued"
	eventStarted = "started"
	eventDone    = "done"
)
