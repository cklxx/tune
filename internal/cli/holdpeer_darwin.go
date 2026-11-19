//go:build darwin

package cli

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func psComm(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func psPPID(pid int) int {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "ppid=").Output()
	if err != nil {
		return 0
	}
	ppid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return ppid
}

func psCwd(pid int) string {
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return strings.TrimPrefix(line, "n")
		}
	}
	return ""
}

func procInfo(pid int) string {
	if pid == 0 {
		return ""
	}
	return fmt.Sprintf("%d ppid=%d cwd=%q exe=%q", pid, psPPID(pid), psCwd(pid), psComm(pid))
}

func holdPeerInfo(nc net.Conn) string {
	uc, ok := nc.(*net.UnixConn)
	if !ok {
		return ""
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return ""
	}
	var pid int
	ctlErr := rc.Control(func(fd uintptr) {
		v, err := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if err == nil {
			pid = v
		}
	})
	if ctlErr != nil || pid == 0 {
		return ""
	}
	ppid := psPPID(pid)
	gppid := psPPID(ppid)
	return fmt.Sprintf("self={%s} parent={%s} grand={%s}", procInfo(pid), procInfo(ppid), procInfo(gppid))
}
