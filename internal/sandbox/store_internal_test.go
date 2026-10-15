package sandbox

import (
	"errors"
	"testing"
	"time"
)

// TestWithTimeout returns what a quick call returns, and gives up on a call
// that does not return in time.
func TestWithTimeout(t *testing.T) {
	t.Parallel()
	got, err := withTimeout(time.Minute, func() (string, error) { return "secret", nil })
	if err != nil || got != "secret" {
		t.Errorf("withTimeout = %q, %v; want the call's answer", got, err)
	}
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	_, err = withTimeout(10*time.Millisecond, func() (string, error) {
		<-block
		return "", nil
	})
	if !errors.Is(err, errKeychainTimeout) {
		t.Errorf("withTimeout = %v, want errKeychainTimeout", err)
	}
}
