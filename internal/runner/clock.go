package runner

import "time"

// Clock is the runner's sense of time (§5.6): the wall it reads the
// worker's activity against, and the tickers its heartbeats beat on.
// Both are injectable so a test can move a run ten minutes in a
// millisecond of real time instead of sleeping the stall out. The zero
// Clock is the real one; Config.defaults fills a nil Now or NewTicker
// in, so a jobRun never checks for them again.
type Clock struct {
	// Now reports the current time; every timestamp the run keeps —
	// the activity clock, the stall checks — comes from here.
	Now func() time.Time
	// NewTicker is time.NewTicker behind the Ticker interface, so a
	// manual clock can fire the beats itself.
	NewTicker func(d time.Duration) Ticker
}

// Ticker is the channel-and-stop face of time.Ticker: what a heartbeat
// loop needs and nothing more, so a manual clock's ticker is one
// buffered channel.
type Ticker interface {
	Chan() <-chan time.Time
	Stop()
}

// realTicker adapts a *time.Ticker to the Ticker interface.
type realTicker struct{ *time.Ticker }

func (t realTicker) Chan() <-chan time.Time { return t.C }

// realClock returns the wall-clock Clock: time.Now and time.NewTicker.
func realClock() Clock {
	return Clock{
		Now:       func() time.Time { return time.Now() },
		NewTicker: func(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} },
	}
}

// fill replaces the nil halves of a Clock with the real ones, so a
// caller only injects what its test needs to fake.
func (c *Clock) fill() {
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now() }
	}
	if c.NewTicker == nil {
		c.NewTicker = func(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }
	}
}
