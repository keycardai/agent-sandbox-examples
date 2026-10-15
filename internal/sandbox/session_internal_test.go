package sandbox

import (
	"fmt"
	"io"
	"testing"
	"time"
)

// TestSetupKeepsUnsavedSettings takes the settings setup kept into the
// session when saving them failed, as when it saved them, and keeps the
// old ones when setup failed.
func TestSetupKeepsUnsavedSettings(t *testing.T) {
	t.Parallel()
	const zone = "https://zone-a.keycard.cloud"
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, zone},
		{fmt.Errorf("%w: the keychain is locked", ErrNotSaved), zone},
		{ErrInterrupted, ""},
	} {
		local := &Local{
			Root: t.TempDir(), Settings: Settings{}, Out: io.Discard,
			Store: NewStore(NoConfig, &Config{Zone: ConfigZone{URL: zone}}, nil),
		}
		session := local.NewSession("127.0.0.1:8480", time.Second)
		session.keepAfterSetup(tc.err)
		if got := session.Local().Settings[KeyZoneURL]; got != tc.want {
			t.Errorf("after setup ended with %v: zone %q, want %q", tc.err, got, tc.want)
		}
	}
}
