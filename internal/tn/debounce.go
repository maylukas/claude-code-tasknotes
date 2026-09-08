package tn

import (
	"time"
)

// --- Debounce ---

const dashboardDebounceInterval = 2 * time.Second

// debouncer coalesces rapid Trigger calls into at most one call to fn per
// interval. It never busy-loops: the worker goroutine blocks on the trigger
// channel (or a bounded sleep) between renders.
type debouncer struct {
	interval time.Duration
	fn       func()
	trigger  chan struct{}
}

func newDebouncer(interval time.Duration, fn func()) *debouncer {
	return &debouncer{interval: interval, fn: fn, trigger: make(chan struct{}, 1)}
}

// Trigger requests a run of fn, coalescing with any already-pending request.
func (d *debouncer) Trigger() {
	select {
	case d.trigger <- struct{}{}:
	default:
	}
}

// run processes triggers for the lifetime of the process. Call it in its
// own goroutine.
func (d *debouncer) run() {
	var last time.Time
	for range d.trigger {
		if since := time.Since(last); since < d.interval {
			time.Sleep(d.interval - since)
		}
		d.drainPending()
		d.fn()
		last = time.Now()
	}
}

// drainPending discards any triggers that piled up while we were debouncing,
// since the upcoming render already covers them.
func (d *debouncer) drainPending() {
	for {
		select {
		case <-d.trigger:
		default:
			return
		}
	}
}
