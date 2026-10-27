//go:build !windows

package procs

// processTable has no fast path off Windows; per-process gopsutil reads are cheap there.
func processTable() (map[int32]tableEntry, bool) { return nil, false }
