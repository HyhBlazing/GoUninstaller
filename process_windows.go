package main

import (
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Processes an uninstaller may leave behind on purpose (survey pages, the
// shell). They are followed but never waited for.
var ignoredDescendants = map[string]bool{
	"explorer.exe": true, "conhost.exe": true, "werfault.exe": true,
	"msedge.exe": true, "chrome.exe": true, "firefox.exe": true, "iexplore.exe": true,
	"brave.exe": true, "opera.exe": true, "360se.exe": true, "360chrome.exe": true,
	"qqbrowser.exe": true, "sogouexplorer.exe": true, "msedgewebview2.exe": true,
}

type trackedProc struct {
	name  string
	alive bool
}

// procTracker follows the process tree started by an uninstaller so that
// bootstrappers which exit immediately are still waited for.
type procTracker struct {
	root     uint32
	started  time.Time
	tracked  map[uint32]*trackedProc
	parents  map[uint32]bool // processes that were alive at the previous refresh
	sawChild bool
}

func newProcTracker(root uint32, started time.Time) *procTracker {
	t := &procTracker{root: root, started: started, tracked: map[uint32]*trackedProc{}, parents: map[uint32]bool{}}
	if root != 0 {
		t.parents[root] = true
	}
	return t
}

// refresh adopts new descendants and returns how many are still running
// (ignoring browsers and the shell). A child is only adopted from a parent
// that was alive at the previous refresh, which guards against PID reuse.
func (t *procTracker) refresh(rootAlive bool) int {
	if t.root == 0 {
		return 0
	}
	snap, e := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if e != nil {
		return 0
	}
	defer windows.CloseHandle(snap)
	type entry struct {
		parent uint32
		name   string
	}
	procs := map[uint32]entry{}
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for e = windows.Process32First(snap, &pe); e == nil; e = windows.Process32Next(snap, &pe) {
		procs[pe.ProcessID] = entry{pe.ParentProcessID, strings.ToLower(windows.UTF16ToString(pe.ExeFile[:]))}
	}
	for changed := true; changed; {
		changed = false
		for pid, p := range procs {
			if pid == 0 || pid == t.root || t.tracked[pid] != nil || !t.parents[p.parent] {
				continue
			}
			if !processCreatedAfter(pid, t.started) {
				continue
			}
			t.tracked[pid] = &trackedProc{name: p.name, alive: true}
			t.parents[pid] = true
			t.sawChild = true
			changed = true
		}
	}
	alive := 0
	next := map[uint32]bool{}
	if rootAlive {
		next[t.root] = true
	}
	for pid, tp := range t.tracked {
		if !tp.alive {
			continue
		}
		if p, ok := procs[pid]; !ok || p.name != tp.name {
			tp.alive = false // exited, or the PID now belongs to something else
			continue
		}
		next[pid] = true
		if !ignoredDescendants[tp.name] {
			alive++
		}
	}
	t.parents = next
	return alive
}

func processCreatedAfter(pid uint32, t time.Time) bool {
	h, e := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if e != nil {
		return true // cannot verify (protected process): trust the parent link
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return true
	}
	return !time.Unix(0, created.Nanoseconds()).Before(t.Add(-2 * time.Second))
}
