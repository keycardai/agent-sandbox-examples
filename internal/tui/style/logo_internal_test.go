package style

import (
	"image/color"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
)

// TestLogo draws the mark at its size: on the page on a dark terminal,
// before and after the terminal reports its background, and on the tile on
// a light one.
func TestLogo(t *testing.T) {
	t.Parallel()
	for _, row := range keycardMark {
		if len(row) != markWidth {
			t.Fatalf("a row of the mark is %d pixels wide, want %d: %q", len(row), markWidth, row)
		}
	}
	if len(keycardMark) != markRows*half {
		t.Fatalf("the mark is %d pixels tall, want %d", len(keycardMark), markRows*half)
	}
	for _, tc := range []struct {
		name          string
		styles        Styles
		background    color.Color
		width, height int
	}{
		{"unknown background", New(true), nil, markWidth, markRows},
		{"dark", New(true), color.Black, markWidth, markRows},
		{"light", New(false), color.White, markWidth + logoPadX*half, markRows + half},
	} {
		logo := Logo(tc.styles, tc.background, Glint{})
		if lipgloss.Width(logo) != tc.width || lipgloss.Height(logo) != tc.height {
			t.Errorf("%s: logo is %dx%d, want %dx%d", tc.name, lipgloss.Width(logo), lipgloss.Height(logo), tc.width,
				tc.height)
		}
		if !strings.Contains(logo, "▀") || !strings.Contains(logo, "▄") {
			t.Errorf("%s: logo has no half blocks", tc.name)
		}
		if LogoWidth(tc.styles) != tc.width {
			t.Errorf("%s: LogoWidth = %d, want %d", tc.name, LogoWidth(tc.styles), tc.width)
		}
	}
}

// TestGlint sweeps the sheen across the logo, again each cycle while again
// is set, and stops it after the sweep once it is not.
func TestGlint(t *testing.T) {
	t.Parallel()
	styles := New(true)
	var glint Glint
	still := Logo(styles, color.Black, glint)
	start := time.Now()
	if glint.Start() == nil || glint.Start() != nil {
		t.Fatal("Start did not start one sweep")
	}
	if glint.Move(start.Add(sheenSweep/half), false) == nil || Logo(styles, color.Black, glint) == still {
		t.Error("halfway through the sweep, the logo shows no sheen")
	}
	again := start.Add(sheenCycle + sheenSweep)
	glint.Move(again, true)
	if glint.Move(again.Add(sheenSweep/half), true) == nil || Logo(styles, color.Black, glint) == still {
		t.Error("the sheen did not sweep again after a cycle")
	}
	if glint.Move(again.Add(sheenSweep), false) != nil || Logo(styles, color.Black, glint) != still {
		t.Error("the sheen kept going after the sweep, with no sweep again")
	}
}
