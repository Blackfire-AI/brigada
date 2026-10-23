package testsupport

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// ParseProcStat reads the state letter and the process group id out of
// the content of a /proc/<pid>/stat file. The comm field is in
// parentheses and may hold spaces and parentheses itself, so the fields
// are counted from the last ')': state is the first after it, then ppid,
// then pgrp.
func ParseProcStat(data []byte) (state byte, pgrp int, ok bool) {
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return 0, 0, false
	}
	fields := bytes.Fields(data[i+1:])
	if len(fields) < 3 || len(fields[0]) != 1 {
		return 0, 0, false
	}
	pgrp, err := strconv.Atoi(string(fields[2]))
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], pgrp, true
}

// ProcGone reports whether pid is out of the process table — dead, or
// a zombie awaiting its reaper, which is as gone as a killed tree's
// members ever look from outside. Where /proc cannot be read, the kill
// syscall alone decides.
func ProcGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return true
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	state, _, ok := ParseProcStat(data)
	return ok && state == 'Z'
}

// GroupGone reports whether process group pgid holds no live process:
// no member at all, or only zombies awaiting a reaper that is not the
// caller (an engine spawned by an exiting client, say). Darwin answers
// EPERM for a group of zombies only, so any kill error counts as gone
// there. Linux answers 0 while unreaped zombies remain, so /proc is
// scanned for members of the group and only non-zombie ones count.
func GroupGone(pgid int) bool {
	if err := syscall.Kill(-pgid, 0); err != nil {
		return true
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		// No /proc (darwin): the kill succeeded, so a live member exists.
		return false
	}
	for _, e := range entries {
		if n := e.Name(); n == "" || n[0] < '0' || n[0] > '9' {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // exited between the listing and the read
		}
		state, pg, ok := ParseProcStat(data)
		if ok && pg == pgid && state != 'Z' {
			return false
		}
	}
	return true
}
