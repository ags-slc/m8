package cmd

import (
	"runtime/debug"
	"testing"
)

func TestVersionInfo(t *testing.T) {
	vcs := []debug.BuildSetting{
		{Key: "vcs.revision", Value: "7b2059f97efc099245fac046040769171343bad6"},
		{Key: "vcs.time", Value: "2026-10-25T21:12:57Z"},
		{Key: "vcs.modified", Value: "false"},
	}
	dirty := []debug.BuildSetting{
		{Key: "vcs.revision", Value: "7b2059f97efc099245fac046040769171343bad6"},
		{Key: "vcs.time", Value: "2026-10-25T21:12:57Z"},
		{Key: "vcs.modified", Value: "true"},
	}

	for _, tc := range []struct {
		name                string
		ldflags             [3]string // version, commit, date; empty keeps the default
		info                *debug.BuildInfo
		wantV, wantC, wantD string
	}{
		{
			name:  "go install from the proxy: a module version, no VCS metadata",
			info:  &debug.BuildInfo{Main: debug.Module{Version: "v0.4.0"}},
			wantV: "0.4.0", wantC: "none", wantD: "unknown",
		},
		{
			name:  "go build in a clean checkout",
			info:  &debug.BuildInfo{Main: debug.Module{Version: "v0.4.1-0.20261104010203-7b2059f97efc"}, Settings: vcs},
			wantV: "0.4.1-0.20261104010203-7b2059f97efc", wantC: "7b2059f", wantD: "2026-10-25T21:12:57Z",
		},
		{
			name:  "go build in a dirty checkout",
			info:  &debug.BuildInfo{Main: debug.Module{Version: "v0.4.1-0.20261104010203-7b2059f97efc+dirty"}, Settings: dirty},
			wantV: "0.4.1-0.20261104010203-7b2059f97efc+dirty", wantC: "7b2059f-dirty", wantD: "2026-10-25T21:12:57Z",
		},
		{
			name:  "a build with no version recorded",
			info:  &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			wantV: "dev", wantC: "none", wantD: "unknown",
		},
		{
			name:  "no build info at all",
			wantV: "dev", wantC: "none", wantD: "unknown",
		},
		{
			name:    "a release build: -ldflags wins over the build info",
			ldflags: [3]string{"0.4.0", "7b2059f", "2026-10-25T21:20:00Z"},
			info:    &debug.BuildInfo{Main: debug.Module{Version: "v0.4.0-dirty"}, Settings: dirty},
			wantV:   "0.4.0", wantC: "7b2059f", wantD: "2026-10-25T21:20:00Z",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, p := range []*string{&version, &commit, &date} {
				if tc.ldflags[i] != "" {
					old := *p
					*p = tc.ldflags[i]
					t.Cleanup(func() { *p = old })
				}
			}
			v, c, d := versionInfo(tc.info)
			if v != tc.wantV || c != tc.wantC || d != tc.wantD {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)", v, c, d, tc.wantV, tc.wantC, tc.wantD)
			}
		})
	}
}
