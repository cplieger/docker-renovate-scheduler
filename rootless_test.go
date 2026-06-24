package main

import "testing"

func TestRootlessCacheLikelyUnwritable(t *testing.T) {
	const customUID = 568

	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	noEnv := env(nil)

	tests := []struct {
		getenv func(string) string
		name   string
		euid   int
		want   bool
	}{
		{
			name: "default image UID never warns",
			euid: defaultImageUID, getenv: noEnv, want: false,
		},
		{
			name: "root never warns",
			euid: 0, getenv: noEnv, want: false,
		},
		{
			name: "custom UID with no cache redirection warns",
			euid: customUID, getenv: noEnv, want: true,
		},
		{
			name: "custom UID with GOCACHE but no forwarding still warns",
			euid: customUID, getenv: env(map[string]string{"GOCACHE": "/data/.cache/go-build"}), want: true,
		},
		{
			name: "custom UID with npm_config_cache but no forwarding still warns",
			euid: customUID, getenv: env(map[string]string{"npm_config_cache": "/data/.npm"}), want: true,
		},
		{
			name: "custom UID mitigated via RENOVATE_CUSTOM_ENV_VARIABLES does not warn",
			euid: customUID, getenv: env(map[string]string{"RENOVATE_CUSTOM_ENV_VARIABLES": `{"GOCACHE":"/data/.cache/go-build"}`}), want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rootlessCacheLikelyUnwritable(tc.euid, tc.getenv)
			if got != tc.want {
				t.Errorf("rootlessCacheLikelyUnwritable(euid=%d) = %v, want %v", tc.euid, got, tc.want)
			}
		})
	}
}
