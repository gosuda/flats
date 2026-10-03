package forkwatch

import (
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func proc(pid, ppid int, flag int32, started time.Time) unix.KinfoProc {
	var p unix.KinfoProc
	p.Proc.P_pid = int32(pid)
	p.Proc.P_flag = flag
	p.Proc.P_starttime = unix.NsecToTimeval(started.UnixNano())
	p.Eproc.Ppid = int32(ppid)
	return p
}

func TestStuckSelectsOldChildrenThatNeverExeced(t *testing.T) {
	now := time.Now()
	old, fresh := now.Add(-time.Minute), now.Add(-time.Second)
	procs := []unix.KinfoProc{
		proc(10, 1, 0, old),     // wedged child: old, never exec'd
		proc(11, 1, pExec, old), // a normal worker (exec'd)
		proc(12, 1, 0, fresh),   // between fork and exec right now
		proc(13, 99, 0, old),    // another process's child
	}
	if got := stuckIn(procs, 1, StuckAfter, now); !slices.Equal(got, []int{10}) {
		t.Errorf("stuck = %v, want [10]", got)
	}
}

// The kernel sets P_EXEC on a child that exec'd, so real workers are never
// selected.
func TestExecedChildIsNotStuck(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	pids, err := Stuck(os.Getpid(), 0, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pids, cmd.Process.Pid) {
		t.Errorf("exec'd child %d reported as stuck", cmd.Process.Pid)
	}
}
