package proxylog

import (
	"errors"
	"testing"
	"time"
)

// TestStoreRing keeps the newest maxRecords records in order once the
// store is full, and counts every change.
func TestStoreRing(t *testing.T) {
	t.Parallel()
	var store Store
	stamp := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	const extra = 3
	for i := range maxRecords + extra {
		store.Add(Record{TS: stamp.Add(time.Duration(i) * time.Second)})
	}
	records, _ := store.Snapshot()
	if len(records) != maxRecords || store.Version() != maxRecords+extra {
		t.Fatalf("%d records at version %d, want %d at %d", len(records), store.Version(), maxRecords,
			maxRecords+extra)
	}
	for i, record := range records {
		if want := stamp.Add(time.Duration(i+extra) * time.Second); !record.TS.Equal(want) {
			t.Fatalf("record %d at %s, want %s", i, record.TS, want)
		}
	}
	store.Reset("pod-2")
	if records, pod := store.Snapshot(); len(records) != 0 || pod != "pod-2" {
		t.Errorf("after Reset: %d records for %q, want none for pod-2", len(records), pod)
	}
}

// errLogs is an error a follower keeps in a store.
var errLogs = errors.New("kubectl logs: exit status 1")

// TestStoreErr keeps the error across a Reset for the same pod, and drops
// it for another pod.
func TestStoreErr(t *testing.T) {
	t.Parallel()
	var store Store
	store.Reset("pod-1")
	store.SetErr(errLogs)
	if store.Reset("pod-1"); store.Err() == nil {
		t.Error("Reset for the same pod dropped the error, want it kept")
	}
	if store.Reset("pod-2"); store.Err() != nil {
		t.Errorf("after Reset for another pod: %v, want no error", store.Err())
	}
}
