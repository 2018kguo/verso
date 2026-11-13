//go:build !linux

package main

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ponytail: no live process-tree sampling off Linux. PeakRSS is the largest
// child's ru_maxrss after exit, and memory/disk ceilings are not enforced.
// Use Linux for stress/resource probes; this exists for local macOS fidelity runs.
func execute(ctx context.Context, timeout time.Duration, memory, disk int64, dir string, args ...string) execution {
	start := time.Now()
	r := execution{Exit: -1}
	if len(args) == 0 {
		r.Failure = "start"
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &limitedBuffer{limit: 8 << 20}
	stderr := &limitedBuffer{limit: 1 << 20}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr, cmd.Dir = out, stderr, dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 250 * time.Millisecond
	cmd.Env = append(os.Environ(), "OMP_NUM_THREADS=1")
	err := cmd.Run()
	if cmd.ProcessState != nil {
		r.Exit = cmd.ProcessState.ExitCode()
		if u, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok && u.Maxrss > 0 {
			r.PeakRSS, r.RSSObserved = u.Maxrss, true // bytes on darwin
		}
	}
	switch {
	case ctx.Err() != nil:
		r.Failure = "timeout"
	case err != nil && r.Exit == -1:
		r.Failure = "start"
	case err != nil:
		r.Failure = "exit"
	}
	var a, b bool
	r.Stdout, a = out.value()
	r.Stderr, b = stderr.value()
	if a || b {
		r.Failure = "output_limit"
	}
	r.WallMS = time.Since(start).Milliseconds()
	return r
}
