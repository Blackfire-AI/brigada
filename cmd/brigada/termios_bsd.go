//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package main

import "golang.org/x/sys/unix"

// tcgets and tcsets are this platform's termios ioctls: the requests
// enterRaw reads and sets the terminal's line settings with.
const (
	tcgets = unix.TIOCGETA
	tcsets = unix.TIOCSETA
)
