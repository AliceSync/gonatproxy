package mux

// Process-wide tunnel byte accounting. Every byte relayed by the mux (via
// Stream.Relay / BridgeStreams) is added to a cumulative counter; the server's
// metrics sampler derives per-second bandwidth from the deltas. These counters
// only cover tunneled traffic (the HTTP console does not use the mux), so they
// reflect real relay throughput.

import "sync/atomic"

var (
	metricUp   int64
	metricDown int64
)

// addUp counts bytes relayed from the "source" direction (local→stream, a→b).
func addUp(n int) { atomic.AddInt64(&metricUp, int64(n)) }

// addDown counts bytes relayed in the return direction.
func addDown(n int) { atomic.AddInt64(&metricDown, int64(n)) }

// MetricsUp returns cumulative upstream bytes relayed by this process.
func MetricsUp() int64 { return atomic.LoadInt64(&metricUp) }

// MetricsDown returns cumulative downstream bytes relayed by this process.
func MetricsDown() int64 { return atomic.LoadInt64(&metricDown) }
