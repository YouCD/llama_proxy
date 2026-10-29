// procresolve.go 真实服务进程解析与进程树终止。
//
// 模型启动命令可能通过包装脚本把真正的推理服务以 setsid/nohup 派生为独立会话的
// 孙进程，并在启动完成后退出包装脚本自身（如 vLLM launcher：nohup setsid ... python
// 后台启动服务、launcher 自身打印 START OK 后退出）。此时 exec.Cmd 只覆盖短命的包装
// shell：其 PID 既不能作为存活判据（launcher 一退出就被 reap 判为"已退出"），也无法
// 用于终止真正的服务进程（杀包装 shell 碰不到 setsid 派生的服务），残留服务会继续
// 占用端口与显存，导致后续模型切换时新进程绑定端口失败、调度卡死。
//
// 本文件以 readiness_url 端口上实际处于 LISTEN 的进程为"真实服务进程"的唯一事实
// 来源：按端口解析持有者 PID（/proc/net/tcp{,6} → socket inode → /proc/<pid>/fd
// 反查）、判断存活、终止其进程树（会话/进程组 leader 杀整组，否则先收集后代再
// 逐一信号），并在停止后确认端口监听释放。
package scheduler

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"

	"llama_proxy/internal/config"
)

// readinessPort 从 readiness_url 解析服务监听端口；解析失败返回 0
// （该模型退化为仅跟踪包装进程的传统行为）。
func readinessPort(entry *config.ProcessConfig) int {
	if entry == nil || entry.ReadinessURL == "" {
		return 0
	}
	u, err := url.Parse(entry.ReadinessURL)
	if err != nil || u.Host == "" {
		return 0
	}
	_, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return 0
	}
	return port
}

// listenSocketInode 返回指定端口 LISTEN 状态套接字的 inode；无监听者时返回 error。
// 依据 /proc/net/tcp 与 /proc/net/tcp6：本地地址末段为十六进制端口，st 列 0A 为
// TCP_LISTEN，inode 为第 10 列。
func listenSocketInode(port int) (uint64, error) {
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for _, line := range lines[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 {
				continue
			}
			if fields[3] != "0A" {
				continue
			}
			local := fields[1]
			colon := strings.LastIndex(local, ":")
			if colon < 0 {
				continue
			}
			p, err := strconv.ParseInt(local[colon+1:], 16, 32)
			if err != nil || int(p) != port {
				continue
			}
			inode, err := strconv.ParseUint(fields[9], 10, 64)
			if err != nil {
				continue
			}
			return inode, nil
		}
	}
	return 0, fmt.Errorf("no listening socket found on port %d", port)
}

// resolveServerPID 返回在指定端口上处于 LISTEN 状态的套接字持有进程 PID。
// 先按端口找到 LISTEN 套接字 inode，再在 /proc/<pid>/fd 下反查 socket:[inode]
// 的所有者。未找到监听者时返回 error。
func resolveServerPID(port int) (int, error) {
	inode, err := listenSocketInode(port)
	if err != nil {
		return 0, err
	}
	target := fmt.Sprintf("socket:[%d]", inode)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdEntries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err != nil {
			continue
		}
		for _, fd := range fdEntries {
			link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
			if err != nil {
				continue
			}
			if link == target {
				return pid, nil
			}
		}
	}
	return 0, fmt.Errorf("no process holds a listening socket on port %d", port)
}

// pidAlive 报告指定 PID 是否存活（signal 0 探活，不发送实际信号）。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// procGroupID 返回指定 PID 的进程组 ID（/proc/<pid>/stat 第 5 列 pgrp）。
// comm 字段可能含空格或括号，从最后一个 ')' 之后开始切分。
func procGroupID(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	s := string(data)
	end := strings.LastIndex(s, ")")
	if end < 0 || end+2 >= len(s) {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 3 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	gid, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, err
	}
	return gid, nil
}

// statPPID 解析 /proc/<pid>/stat 的 ppid 列（第 4 列）；进程不存在时返回 0。
func statPPID(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(data)
	end := strings.LastIndex(s, ")")
	if end < 0 {
		return 0
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

// processChildren 返回 parent 的直接子进程 PID 列表（全量 /proc 扫描）。
func processChildren(parent int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if statPPID(pid) == parent {
			out = append(out, pid)
		}
	}
	return out
}

// collectTree 广度优先收集 root 及其全部后代 PID（先收集后发送，避免中途退出
// 导致漏杀）。
func collectTree(root int) []int {
	out := []int{root}
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, child := range processChildren(cur) {
			if !seen[child] {
				seen[child] = true
				out = append(out, child)
				queue = append(queue, child)
			}
		}
	}
	return out
}

// killTree 向目标进程树发送信号：目标为自身进程组 leader（pgid==pid，如 setsid
// 派生的服务进程）时直接杀整组（负 PID），一次覆盖 api server 与其全部 worker；
// 否则先收集后代再连同自身逐一信号。目标已退出时静默返回。
func killTree(pid, group int, sig syscall.Signal) error {
	if !pidAlive(pid) {
		return nil
	}
	if group == pid {
		return syscall.Kill(-pid, sig)
	}
	var firstErr error
	for _, p := range collectTree(pid) {
		if p == pid {
			continue
		}
		if err := syscall.Kill(p, sig); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := syscall.Kill(pid, sig); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// portListenerGone 报告指定端口上是否已无 LISTEN 套接字（port<=0 视为恒真）。
func portListenerGone(port int) bool {
	if port <= 0 {
		return true
	}
	_, err := listenSocketInode(port)
	return err != nil
}

// pidIn 报告 pid 是否出现在列表内。
func pidIn(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}

// anyAlive 报告列表中是否还有存活进程。
func anyAlive(pids []int) bool {
	for _, p := range pids {
		if pidAlive(p) {
			return true
		}
	}
	return false
}
