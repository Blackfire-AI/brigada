package notify

import (
	"context"
	"sync"
)

// Note is one recorded notification: what the banner would have shown.
type Note struct {
	Title string
	Body  string
}

// Fake is the recording notifier the tests stand in for the OS tools:
// every Notify appends to its list. Err, when set, is what every Notify
// returns — the delivery-loop tests use it to play a broken notifier.
type Fake struct {
	mu    sync.Mutex
	notes []Note
	// Err is returned by every Notify while set.
	Err error
}

// Notify records the notification and returns f's Err.
func (f *Fake) Notify(_ context.Context, title, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, Note{Title: title, Body: body})
	return f.Err
}

// Notes returns the notifications recorded so far, in order.
func (f *Fake) Notes() []Note {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Note(nil), f.notes...)
}

// Name is the log name.
func (f *Fake) Name() string { return "fake" }
