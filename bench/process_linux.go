//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RSS is a sampled sum over the process group and observed descendants, not
// ru_maxrss (which misses concurrent children). This is a benchmark supervisor,
// not a sandbox for a hostile process that deliberately escapes its group.
type proc struct {
	pid, ppid, group int
	rss              int64
}

func processTree(root int, seen map[int]bool) (int64, []int) {
	entries, _ := os.ReadDir("/proc")
	all := make([]proc, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		s := string(b)
		i := strings.LastIndex(s, ")")
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 22 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		group, _ := strconv.Atoi(f[2])
		rss, _ := strconv.ParseInt(f[21], 10, 64)
		all = append(all, proc{pid, ppid, group, rss * int64(os.Getpagesize())})
	}
	seen[root] = true
	for change := true; change; {
		change = false
		for _, p := range all {
			if !seen[p.pid] && (p.group == root || seen[p.ppid]) {
				seen[p.pid] = true
				change = true
			}
		}
	}
	var rss int64
	var pids []int
	for _, p := range all {
		if seen[p.pid] {
			rss += p.rss
			pids = append(pids, p.pid)
		}
	}
	return rss, pids
}

func diskBytes(root string, stop int64) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if s, e := d.Info(); e == nil {
				total += s.Size()
			}
		}
		if total > stop {
			return filepath.SkipAll
		}
		return nil
	})
	return total
}

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
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = out
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 250 * time.Millisecond
	cmd.Env = append(os.Environ(), "SAL_USE_VCLPLUGIN=gen", "OMP_NUM_THREADS=1")
	if dir != "" {
		cmd.Dir = dir
	}
	if err := cmd.Start(); err != nil {
		r.Failure = "start"
		r.Stderr = err.Error()
		return r
	}
	seen := map[int]bool{}
	var pids []int
	kill := func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	defer kill()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	diskTick := time.NewTicker(100 * time.Millisecond)
	defer diskTick.Stop()
	sample := func() {
		var rss int64
		rss, pids = processTree(cmd.Process.Pid, seen)
		if len(pids) > 0 {
			r.RSSObserved = true
		}
		if rss > r.PeakRSS {
			r.PeakRSS = rss
		}
		if memory > 0 && rss > memory {
			r.Failure = "memory_limit"
			kill()
		}
	}
	sample()
	waiting := true
	for waiting {
		select {
		case err := <-done:
			if cmd.ProcessState != nil {
				r.Exit = cmd.ProcessState.ExitCode()
			}
			if err != nil && r.Failure == "" {
				r.Failure = "exit"
			}
			waiting = false
		case <-ctx.Done():
			if r.Failure == "" {
				r.Failure = "timeout"
			}
			kill()
		case <-ticker.C:
			sample()
			_, a := out.value()
			_, b := stderr.value()
			if a || b {
				r.Failure = "output_limit"
				kill()
			}
		case <-diskTick.C:
			if disk > 0 && dir != "" && diskBytes(dir, disk) > disk {
				r.Failure = "disk_limit"
				kill()
			}
		}
	}
	r.Stdout, _ = out.value()
	r.Stderr, _ = stderr.value()
	_, a := out.value()
	_, b := stderr.value()
	if a || b {
		r.Failure = "output_limit"
	}
	if disk > 0 && dir != "" && diskBytes(dir, disk) > disk {
		r.Failure = "disk_limit"
	}
	if ctx.Err() != nil && r.Failure == "" {
		r.Failure = "timeout"
	}
	r.WallMS = time.Since(start).Milliseconds()
	return r
}
