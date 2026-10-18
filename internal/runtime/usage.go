package runtime

import (
	"syscall"
	"time"
)

// reportUsage sends the worker's CPU and memory now and then every interval,
// as control lines that are never rate limited. It runs until the process
// exits; the parent takes the final CPU from the exit status.
func reportUsage(l *childLog, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if m, ok := usageSample(); ok {
			l.send(m)
		}
		<-t.C
	}
}

// usageSample is the process's own CPU time (user+system) and, where it is
// measured, its resident private anonymous memory.
func usageSample() (childMsg, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return childMsg{}, false
	}
	cpu := ru.Utime.Nano() + ru.Stime.Nano()
	m := childMsg{T: "usage", CPU: &cpu}
	if mem, ok := privateMemory(); ok {
		m.Mem = &mem
	}
	return m, true
}

// specAck is the worker's acknowledgement of its spec, with the CPU it used
// so far: a pre-started worker's warm-up, which the parent never counts as
// the flat's.
func specAck() childMsg {
	m := childMsg{T: "spec"}
	if s, ok := usageSample(); ok {
		m.CPU = s.CPU
	}
	return m
}
