package chart_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-poc/internal/chart"
)

func TestCheckPinned(t *testing.T) {
	t.Parallel()
	const (
		config  = "/repo/mise.toml"
		install = "/data/mise/installs/helm/4.3.0"
		helm    = install + "/darwin-arm64/helm"
		global  = "/home/u/.config/mise/config.toml"
		outside = `", outside "` + install + `"`
	)
	entry := func(source, installed string) string {
		return `[{"version": "4.3.0", "install_path": "` + install + `", ` +
			`"source": {"type": "mise.toml", "path": "` + source + `"}, ` +
			`"installed": ` + installed + `, "active": true}]`
	}
	valid := entry(config, "true")
	tests := []struct {
		name  string
		which string
		list  string
		want  string
	}{
		{name: "pinned", which: helm + "\n", list: valid},
		{
			name: "global source", which: helm, list: entry(global, "true"),
			want: `mise resolves helm from "` + global + `", not ` + config,
		},
		{
			name: "not installed", which: helm, list: entry(config, "false"),
			want: "helm 4.3.0 is not installed, run mise install",
		},
		{
			name: "outside install path", which: "/usr/local/bin/helm", list: valid,
			want: `mise resolves helm to "/usr/local/bin/helm` + outside,
		},
		{
			name: "sibling prefix", which: install + "-evil/helm", list: valid,
			want: `mise resolves helm to "` + install + `-evil/helm` + outside,
		},
		{
			name: "dot-dot escape", which: install + "/../4.3.0-evil/helm", list: valid,
			want: `mise resolves helm to "` + install + `-evil/helm` + outside,
		},
		{
			name: "two entries", which: helm, list: valid[:len(valid)-1] + "," + valid[1:],
			want: "printed 2 entries, not one",
		},
		{name: "no entries", which: helm, list: "[]", want: "printed 0 entries, not one"},
		{name: "malformed", which: helm, list: "[{", want: "printed something other than one entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := chart.CheckPinned(config, tt.which, []byte(tt.list))
			if tt.want == "" {
				if err != nil {
					t.Fatalf("CheckPinned: %v", err)
				}
				if got != helm {
					t.Errorf("CheckPinned = %q, want %q", got, helm)
				}
				return
			}
			if !errors.Is(err, chart.ErrHelm) {
				t.Fatalf("CheckPinned error = %v, want ErrHelm", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("CheckPinned error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}
