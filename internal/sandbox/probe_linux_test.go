package sandbox

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// probeMain is what a confined child runs: one operation, and one line that says
// what the kernel answered. The tests read the line, so a probe that could not
// tell "refused" from "broken" would let a broken test pass.
func probeMain(args []string) int {
	if len(args) == 0 {
		fmt.Println("error: no check named")
		return 2
	}
	fmt.Println(runProbe(args[0], args[1:]))
	return 0
}

var errnoNames = map[syscall.Errno]string{
	syscall.EACCES: "EACCES", syscall.EPERM: "EPERM", syscall.ENOSYS: "ENOSYS",
	syscall.ENOENT: "ENOENT", syscall.EXDEV: "EXDEV", syscall.EISCONN: "EISCONN",
	syscall.EAFNOSUPPORT: "EAFNOSUPPORT", syscall.ECONNREFUSED: "ECONNREFUSED",
}

// verdict is "ok", or the name of the errno the operation failed with.
func verdict(err error) string {
	if err == nil {
		return "ok"
	}
	var e syscall.Errno
	if errors.As(err, &e) {
		if n, ok := errnoNames[e]; ok {
			return n
		}
		return "errno=" + strconv.Itoa(int(e))
	}
	return "error: " + err.Error()
}

func rawErr(e syscall.Errno) error {
	if e == 0 {
		return nil
	}
	return e
}

func runProbe(check string, a []string) string {
	arg := func(i int) string {
		if i < len(a) {
			return a[i]
		}
		return ""
	}
	port := func(i int) int { n, _ := strconv.Atoi(arg(i)); return n }
	switch check {
	case "read":
		_, err := os.ReadFile(arg(0))
		return verdict(err)
	case "write":
		return verdict(os.WriteFile(arg(0), []byte("x"), 0o600))
	case "list":
		_, err := os.ReadDir(arg(0))
		return verdict(err)
	case "fsops":
		return fsops(arg(0))
	case "unix":
		fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err != nil {
			return "socket:" + verdict(err)
		}
		defer syscall.Close(fd)
		return "connect:" + verdict(syscall.Connect(fd, &syscall.SockaddrUnix{Name: arg(0)}))
	case "abstract":
		fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err != nil {
			return "socket:" + verdict(err)
		}
		defer syscall.Close(fd)
		return "connect:" + verdict(syscall.Connect(fd, &syscall.SockaddrUnix{Name: "@" + arg(0)}))
	case "socketpair":
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err == nil {
			syscall.Close(fds[0])
			syscall.Close(fds[1])
		}
		return verdict(err)
	case "socketpair-dgram":
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
		if err == nil {
			syscall.Close(fds[0])
			syscall.Close(fds[1])
		}
		return verdict(err)
	case "tcp":
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
		if err != nil {
			return "socket:" + verdict(err)
		}
		defer syscall.Close(fd)
		return "connect:" + verdict(syscall.Connect(fd, &syscall.SockaddrInet4{Port: port(0), Addr: [4]byte{127, 0, 0, 1}}))
	case "udp":
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
		if err == nil {
			syscall.Close(fd)
		}
		return verdict(err)
	case "udp6":
		fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_DGRAM, 0)
		if err == nil {
			syscall.Close(fd)
		}
		return verdict(err)
	case "netlink":
		fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
		if err == nil {
			syscall.Close(fd)
		}
		return verdict(err)
	case "serve": // bind, listen, accept one connection, answer it
		ln, err := net.Listen("tcp", "127.0.0.1:"+arg(0))
		if err != nil {
			return "listen:" + verdict(err)
		}
		defer ln.Close()
		fmt.Println("listening")
		if tl, ok := ln.(*net.TCPListener); ok {
			_ = tl.SetDeadline(time.Now().Add(10 * time.Second))
		}
		c, err := ln.Accept()
		if err != nil {
			return "accept:" + verdict(err)
		}
		defer c.Close()
		_, _ = io.WriteString(c, "pong")
		return "served"
	case "kill":
		return verdict(syscall.Kill(port(0), 0))
	case "shmget":
		_, _, e := syscall.Syscall(syscall.SYS_SHMGET, 0, 4096, 0o1600)
		return verdict(rawErr(e))
	case "mq_open":
		name := []byte("/muster-probe\x00")
		_, _, e := syscall.Syscall6(syscall.SYS_MQ_OPEN, uintptr(unsafe.Pointer(&name[0])), syscall.O_RDWR|syscall.O_CREAT, 0o600, 0, 0, 0)
		return verdict(rawErr(e))
	case "keyctl":
		_, _, e := syscall.Syscall6(syscall.SYS_KEYCTL, 0, 0, 0, 0, 0, 0)
		return verdict(rawErr(e))
	case "uring":
		_, _, e := syscall.Syscall(425, 4, 0, 0)
		return verdict(rawErr(e))
	case "ptrace":
		_, _, e := syscall.Syscall6(syscall.SYS_PTRACE, 16 /* PTRACE_ATTACH */, uintptr(port(0)), 0, 0, 0, 0)
		return verdict(rawErr(e))
	case "status":
		b, err := os.ReadFile("/proc/self/status")
		if err != nil {
			return verdict(err)
		}
		var out []string
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "NoNewPrivs:") || strings.HasPrefix(l, "Seccomp:") {
				out = append(out, strings.Join(strings.Fields(l), " "))
			}
		}
		return strings.Join(out, "; ")
	case "pty":
		f, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
		if err != nil {
			return verdict(err)
		}
		defer f.Close()
		var unlock int32
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
			return verdict(e)
		}
		const tiocgptpeer = 0x5441
		sfd, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), tiocgptpeer, uintptr(syscall.O_RDWR|syscall.O_NOCTTY))
		if e != 0 {
			return verdict(e)
		}
		syscall.Close(int(sfd))
		return "ok"
	case "env":
		return os.Getenv(arg(0))
	}
	return "error: unknown check " + check
}

// fsops does inside dir what a build does: create, make a directory, rename
// across directories (the ABI 2 right), truncate, remove. Each failure names its
// step.
func fsops(dir string) string {
	step := func(name string, err error) string {
		if err != nil {
			return name + ":" + verdict(err)
		}
		return ""
	}
	file := filepath.Join(dir, "f")
	sub := filepath.Join(dir, "d")
	moved := filepath.Join(sub, "g")
	for _, s := range []string{
		step("create", os.WriteFile(file, []byte("data"), 0o600)),
		step("mkdir", os.Mkdir(sub, 0o700)),
		step("rename", os.Rename(file, moved)),
		step("truncate", os.Truncate(moved, 0)),
		step("symlink", os.Symlink("g", filepath.Join(sub, "l"))),
		step("remove", os.Remove(moved)),
		step("remove-link", os.Remove(filepath.Join(sub, "l"))),
		step("rmdir", os.Remove(sub)),
	} {
		if s != "" {
			return s
		}
	}
	return "ok"
}
