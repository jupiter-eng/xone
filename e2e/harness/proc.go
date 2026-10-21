package harness

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ProcStat is one process's resource use as read from /proc (Linux only)
type ProcStat struct {
	// CPU is the user and kernel CPU time used so far. Two readings subtracted give what was used
	// in between
	CPU time.Duration
	// RSS is the current resident memory in bytes (VmRSS)
	RSS int64
	// PeakRSS is the highest resident memory since the process started, in bytes (VmHWM)
	PeakRSS int64
	// Threads is the number of threads
	Threads int
}

// userHZ is the unit of the CPU times in /proc/<pid>/stat. The USER_HZ the kernel exposes to user
// space is 100 on every mainstream architecture, whatever the kernel's own CONFIG_HZ
const userHZ = 100

// ReadProc reads /proc/<pid>/stat and /proc/<pid>/status
func ReadProc(pid int) (ProcStat, error) {
	var s ProcStat
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return s, err
	}
	// The 2nd field is the command name in brackets and may hold spaces, so the count starts after
	// the last ')': the 1st field after it is the 3rd field (state), and utime / stime are the
	// 14th and 15th
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return s, fmt.Errorf("unexpected /proc/%d/stat: %q", pid, stat)
	}
	f := strings.Fields(string(stat[i+1:]))
	if len(f) < 13 {
		return s, fmt.Errorf("unexpected /proc/%d/stat: %q", pid, stat)
	}
	utime, err1 := strconv.ParseInt(f[11], 10, 64)
	stime, err2 := strconv.ParseInt(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return s, fmt.Errorf("unexpected /proc/%d/stat: %q", pid, stat)
	}
	s.CPU = time.Duration(utime+stime) * time.Second / userHZ

	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return s, err
	}
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "VmRSS":
			s.RSS = n * 1024 // the unit is kB
		case "VmHWM":
			s.PeakRSS = n * 1024
		case "Threads":
			s.Threads = int(n)
		}
	}
	return s, sc.Err()
}

// ResetPeakRSS resets VmHWM in /proc/<pid>/status to the current RSS (by writing 5 to clear_refs,
// supported since Linux 4.0). A load test calls it before each step, so that the PeakRSS read at
// the end of a step is that step's own peak rather than the peak since the process started
func ResetPeakRSS(pid int) error {
	return os.WriteFile(fmt.Sprintf("/proc/%d/clear_refs", pid), []byte("5"), 0)
}
