package main

// Live metrics for the dashboard: active connection counts (entry clients, NAT
// clients) and per-second relay bandwidth (derived from the mux's cumulative
// byte counters). A 1 Hz sampler keeps a short history for a mini graph.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"deepseekaiworker/internal/admin"
	"deepseekaiworker/internal/mux"
)

var (
	metricEntry int64 // active entry (client) connections
	metricNat   int64 // active NAT client connections

	mtx     sync.Mutex
	ring    []admin.RatePoint
	curUp   int64 // bytes/sec
	curDown int64
)

func metricEntryInc() { atomic.AddInt64(&metricEntry, 1) }
func metricEntryDec() { atomic.AddInt64(&metricEntry, -1) }
func metricNatInc()   { atomic.AddInt64(&metricNat, 1) }
func metricNatDec()   { atomic.AddInt64(&metricNat, -1) }

// runMetricsDaemon samples relay throughput once per second and keeps history.
func runMetricsDaemon(ctx context.Context) {
	prevUp, prevDown := mux.MetricsUp(), mux.MetricsDown()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			up, down := mux.MetricsUp(), mux.MetricsDown()
			du, dd := up-prevUp, down-prevDown
			prevUp, prevDown = up, down
			e := atomic.LoadInt64(&metricEntry)
			n := atomic.LoadInt64(&metricNat)
			p := admin.RatePoint{T: now.Unix(), Up: du, Down: dd, Conns: e + n}
			mtx.Lock()
			ring = append(ring, p)
			if len(ring) > 60 {
				ring = ring[len(ring)-60:]
			}
			curUp, curDown = du, dd
			mtx.Unlock()
		}
	}
}

// metricSnapshot returns the current state for the dashboard.
func metricSnapshot() admin.ConnMetrics {
	e := atomic.LoadInt64(&metricEntry)
	n := atomic.LoadInt64(&metricNat)
	mtx.Lock()
	defer mtx.Unlock()
	return admin.ConnMetrics{
		Entry:   e,
		Nat:     n,
		Total:   e + n,
		Up:      curUp,
		Down:    curDown,
		History: append([]admin.RatePoint(nil), ring...),
	}
}
