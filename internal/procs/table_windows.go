//go:build windows

package procs

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// WHY: gopsutil's Process.Ppid() on Windows takes a full CreateToolhelp32Snapshot per
// call (~10ms each), making TakeSnapshot O(n) snapshots — about 4s for 440 processes,
// well past the 2s hook budget. One Toolhelp32 walk yields pid, ppid and exe name for
// every process at once.
func processTable() (map[int32]tableEntry, bool) {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, false
	}
	defer windows.CloseHandle(h)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if err := windows.Process32First(h, &e); err != nil {
		return nil, false
	}
	out := map[int32]tableEntry{}
	for {
		out[int32(e.ProcessID)] = tableEntry{PPID: int32(e.ParentProcessID), Name: windows.UTF16ToString(e.ExeFile[:])}
		if err := windows.Process32Next(h, &e); err != nil {
			break
		}
	}
	return out, true
}
