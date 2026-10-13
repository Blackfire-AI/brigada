package lifecycle

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Lock is the single-instance lock (§13): an exclusive flock on a file
// beside the store. The kernel releases it when the process dies, however
// it dies, so a killed engine never wedges its state directory; the pid
// written into the file is for the message a second engine prints, not
// for the decision itself.
type Lock struct {
	f *os.File
}

// AcquireLock takes the exclusive lock at path. A lock another engine
// holds is refused with its pid named (§13: "a second engine refuses
// with a clear message naming the live one's pid").
func AcquireLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open the engine lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid := lockedPid(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			if pid > 0 {
				return nil, fmt.Errorf("another engine (pid %d) is already running on this state directory (lock %s); refusing to start a second", pid, path)
			}
			return nil, fmt.Errorf("another engine holds the lock %s; refusing to start a second", path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if err := writePid(f, os.Getpid()); err != nil {
		f.Close()
		return nil, fmt.Errorf("write our pid into %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock and closes its file. The lock file itself
// stays: removing it would let a second engine create a fresh file and
// lock that, two engines in one state directory for a moment.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err1 := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	err2 := l.f.Close()
	l.f = nil
	if err1 != nil {
		return err1
	}
	return err2
}

// writePid truncates the lock file and writes pid into it, under the
// lock this process holds.
func writePid(f *os.File, pid int) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := f.WriteString(strconv.Itoa(pid) + "\n"); err != nil {
		return err
	}
	return f.Sync()
}

// lockedPid reads the holder's pid from a lock file this process could
// not lock: whatever the holder last wrote. 0 when it holds nothing
// readable — the message then names the lock alone, never a guess.
func lockedPid(f *os.File) int {
	data, err := io.ReadAll(f)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// EnginePid reads the pid of the engine holding the lock at path, or 0
// when none does. Callers that need certainty take the lock instead;
// this is for messages and tests.
func EnginePid(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		// Someone holds it exclusively: the pid inside is the live one's.
		return lockedPid(f)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return 0
}
