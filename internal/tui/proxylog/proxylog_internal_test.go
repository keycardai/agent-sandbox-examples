package proxylog

import (
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
