package monitor

// ShipperStats is a snapshot of the active shipper's lifetime counters.
//
// It exists because a telemetry SDK is the worst possible place for silent loss:
// when the shipper starts throwing events away, the system that would have told
// you about it is the system that broke. Consumers can surface these on their own
// /healthcheck or metrics endpoint and see the loss from the outside.
type ShipperStats struct {
	// Enqueued is the number of events accepted into the shipper's buffer.
	// Events only counted toward stdout (no IngestURL) never reach a shipper and
	// are not counted here.
	Enqueued int64

	// Dropped is the number of events the shipper lost and will never deliver:
	// refused because the buffer was full, unserializable, quarantined as
	// malformed, abandoned after retries, refused with a 401/403/404 without a
	// spool, or evicted from a full spool. A counter that only tracked the
	// full-buffer case would report 0 while an expired API key silently
	// discarded every batch.
	Dropped int64

	// Flushed is the number of events the ingest endpoint accepted.
	Flushed int64

	// Quarantined is the part of Dropped that ingest refused as malformed even
	// after bisection isolated them one by one. It points at a defect in the
	// emitting code rather than in the pipeline; with a spool, the events are
	// kept in poison.ndjson so it can be found.
	Quarantined int64

	// Spooled is the number of events written to the disk spool (0 without one).
	Spooled int64

	// Pending is the number of events on disk waiting for delivery, and
	// PendingBytes their size. Pending climbing while Flushed stands still is
	// what a Monitor outage looks like from the service's side.
	Pending      int64
	PendingBytes int64
}

// Stats reports the active shipper's counters.
//
// Returns the zero value when no shipper is running (no IngestURL configured, or
// Init was never called) — that is "nothing is being shipped", not "nothing was
// lost". Counters are per-shipper: a re-Init points the process at a new
// destination and starts fresh.
func Stats() ShipperStats {
	s := globalShipper.Load()
	if s == nil {
		return ShipperStats{}
	}
	st := ShipperStats{
		Enqueued:    s.enqueued.Load(),
		Dropped:     s.dropped.Load(),
		Flushed:     s.flushed.Load(),
		Quarantined: s.quarantined.Load(),
	}
	if sp := s.spool; sp != nil {
		st.Spooled = sp.spooled.Load()
		st.Pending = sp.pendingLines.Load()
		st.PendingBytes = sp.pendingBytes.Load()
	}
	return st
}
