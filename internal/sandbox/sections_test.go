package sandbox_test

import (
	"slices"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// TestSections names every section once, with a title and what it asks.
// The four sections setup can ask on their own are offered as a change, in
// the order Init asks them.
func TestSections(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	var changes []string
	for _, section := range sandbox.Sections() {
		if seen[section.ID] || section.Title == "" || section.About == "" || section.Short == "" {
			t.Errorf("section %+v: want it once, with a title, an about and a short text", section)
		}
		seen[section.ID] = true
		if section.Change != "" {
			changes = append(changes, section.ID)
		}
	}
	want := []string{sandbox.SectionAccount, sandbox.SectionZone, sandbox.SectionScenario, sandbox.SectionCluster}
	if !slices.Equal(changes, want) {
		t.Errorf("changes = %v, want %v", changes, want)
	}
	if got := sandbox.SectionOf(sandbox.SectionPolicy).Title; got != "Zone policy" {
		t.Errorf("SectionOf(policy).Title = %q, want Zone policy", got)
	}
}
