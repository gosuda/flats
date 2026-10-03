// Package forkwatch unwedges child processes stuck between fork and exec.
//
// On macOS a fork in a multi-threaded process can leave the child spinning
// in Network.framework's atfork handler, on a lock another parent thread
// held at fork time (golang/go#56784). The child never execs, so the parent
// waits in syscall.forkExec forever while holding syscall.ForkLock; on
// darwin every new socket takes that lock too, so the whole process loses
// its networking. Flats hit this on a real tailnet. The watchdog finds such
// children (they never set P_EXEC) and kills them: the parent's fork then
// returns and the caller sees the child exit. It never forks itself.
package forkwatch

import (
	"context"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// pExec is P_EXEC from <sys/proc.h>: the process has called exec.
const pExec = 0x00004000

// StuckAfter is how long a child may stay between fork and exec. A normal
// fork+exec takes milliseconds.
const StuckAfter = 10 * time.Second

// Start runs the watchdog until ctx ends. logf may be nil.
func Start(ctx context.Context, logf func(string, ...any)) {
	go func() {
		t := time.NewTicker(StuckAfter / 2)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			pids, err := Stuck(os.Getpid(), StuckAfter, time.Now())
			if err != nil {
				continue
			}
			for _, pid := range pids {
				if err := syscall.Kill(pid, syscall.SIGKILL); err == nil && logf != nil {
					logf("killed child process %d stuck between fork and exec for over %s (macOS fork bug)", pid, StuckAfter)
				}
			}
		}
	}()
}

// Stuck returns children of parent that are older than age and have not
// called exec.
func Stuck(parent int, age time.Duration, now time.Time) ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	return stuckIn(procs, parent, age, now), nil
}

func stuckIn(procs []unix.KinfoProc, parent int, age time.Duration, now time.Time) []int {
	var out []int
	for i := range procs {
		p := &procs[i]
		if int(p.Eproc.Ppid) != parent || p.Proc.P_flag&pExec != 0 {
			continue
		}
		started := time.Unix(p.Proc.P_starttime.Sec, int64(p.Proc.P_starttime.Usec)*1000)
		if now.Sub(started) > age {
			out = append(out, int(p.Proc.P_pid))
		}
	}
	return out
}
