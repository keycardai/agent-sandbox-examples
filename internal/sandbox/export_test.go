package sandbox

import "time"

// ErrZoneNotReady lets tests match CreateZone giving up.
var ErrZoneNotReady = errZoneNotReady

// SetZonePolling sets how often CreateZone checks a new zone and how long it
// waits, so tests need not wait minutes.
func (k *Keycard) SetZonePolling(every, limit time.Duration) { k.zonePoll, k.zoneWait = every, limit }
