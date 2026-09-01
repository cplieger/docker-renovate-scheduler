package main

type runPayload struct {
	Repos []string `json:"repos,omitempty"`
	// Env can contain credentials and must never be logged.
	Env []string `json:"env,omitempty"`
}
