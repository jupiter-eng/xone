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

// ProcStat 从 /proc 读到的一个进程的资源占用（只在 Linux 上有）
type ProcStat struct {
	// CPU 用户态加内核态累计用掉的 CPU 时间。两次读数相减就是这段时间里用了多少
	CPU time.Duration
	// RSS 当前常驻内存，字节（VmRSS）
	RSS int64
	// PeakRSS 进程启动以来常驻内存的峰值，字节（VmHWM）
	PeakRSS int64
	// Threads 线程数
	Threads int
}

// userHZ /proc/<pid>/stat 里 CPU 时间的单位。内核对用户态暴露的
// USER_HZ 在所有主流架构上固定是 100，与内核自己的 CONFIG_HZ 无关
const userHZ = 100

// ReadProc 读 /proc/<pid>/stat 和 /proc/<pid>/status
func ReadProc(pid int) (ProcStat, error) {
	var s ProcStat
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return s, err
	}
	// 第 2 个字段是括号括起来的命令名，里面可能有空格，所以从最后一个 ')' 之后数：
	// 那之后的第 1 个字段是第 3 个字段（state），utime / stime 是第 14、15 个
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
			s.RSS = n * 1024 // 单位是 kB
		case "VmHWM":
			s.PeakRSS = n * 1024
		case "Threads":
			s.Threads = int(n)
		}
	}
	return s, sc.Err()
}

// ResetPeakRSS 把 /proc/<pid>/status 里的 VmHWM 重置成当前的 RSS（写 5 到 clear_refs，
// Linux 4.0 起支持）。压测分档时每档开始前调一次，档末读到的 PeakRSS 就是这一档自己的峰值，
// 而不是进程启动以来的峰值
func ResetPeakRSS(pid int) error {
	return os.WriteFile(fmt.Sprintf("/proc/%d/clear_refs", pid), []byte("5"), 0)
}
