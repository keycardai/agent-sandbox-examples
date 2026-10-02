package sandbox_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

const (
	aliceEmail = "alice@acme.example"
	bobEmail   = "bob@acme.example"
)

func TestStateRoundTripsReadableByItsOwnerOnly(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sandbox", "state.json")
	state, err := sandbox.LoadState(path)
	if err != nil {
		t.Fatalf("a missing state file must load as empty: %v", err)
	}
	if state.Users == nil {
		t.Fatal("an empty state must take users")
	}
	state.ZoneID = testZone
	state.Users[bobEmail] = &sandbox.User{Email: bobEmail, Access: false}
	state.Users[aliceEmail] = &sandbox.User{Email: aliceEmail, Access: true}
	if err = state.Save(path); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("state file mode = %o, want 600", mode)
	}
	loaded, err := sandbox.LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ZoneID != testZone || len(loaded.Users) != 2 {
		t.Errorf("loaded %+v, want the saved zone and both users", loaded)
	}
}

func TestSortedUsersPutsUsersWithAccessFirst(t *testing.T) {
	t.Parallel()

	state := &sandbox.State{Users: map[string]*sandbox.User{
		"carol": {Email: "carol@acme.example", Access: false},
		"bob":   {Email: bobEmail, Access: true},
		"alice": {Email: aliceEmail, Access: false},
		"dave":  {Email: "dave@acme.example", Access: true},
	}}

	sorted := state.SortedUsers()
	got := make([]string, 0, len(sorted))
	for _, user := range sorted {
		got = append(got, user.Email)
	}
	want := []string{bobEmail, "dave@acme.example", aliceEmail, "carol@acme.example"}
	if !slices.Equal(got, want) {
		t.Errorf("SortedUsers = %v, want %v", got, want)
	}
}
