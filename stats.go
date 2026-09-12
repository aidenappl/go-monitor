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
	// refused because the buffer was full, unserializable, or in a batch
	// abandoned after retries / rejected with a 4xx. A counter that only tracked
	// the full-buffer case would report 0 while an expired API key silently
	// discarded every batch.
	Dropped int64

	// Flushed is the number of events the ingest endpoint accepted (a response
	// under 400). Enqueued minus Flushed minus Dropped is what is still in flight.
	Flushed int64
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
	return ShipperStats{
		Enqueued: s.enqueued.Load(),
		Dropped:  s.dropped.Load(),
		Flushed:  s.flushed.Load(),
	}
}
