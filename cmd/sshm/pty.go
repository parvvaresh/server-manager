package main

// Pseudo-terminals for the dashboard's terminal, without cgo or a third party
// package. Only the step that finds the other end differs between Linux and
// macOS; see pty_linux.go and pty_darwin.go.

import (
	"os"
	"syscall"
	"unsafe"
)

func ioctl(fd, req, arg uintptr) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	if e != 0 {
		return e
	}
	return nil
}

// openPTY returns a new pseudo-terminal: the master end, which we read and
// write, and the terminal end for the program that runs on it.
//
// The master is put in non-blocking mode before it becomes an *os.File, so
// it goes through Go's poller: Close then wakes a Read that is waiting on it.
func openPTY() (master, tty *os.File, err error) {
	fd, err := syscall.Open("/dev/ptmx", syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	name, err := ptyUnlock(fd)
	if err == nil {
		err = syscall.SetNonblock(fd, true)
	}
	if err != nil {
		syscall.Close(fd)
		return nil, nil, err
	}
	master = os.NewFile(uintptr(fd), "/dev/ptmx")
	tty, err = os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, tty, nil
}

// setWinsize tells the terminal its size; the program on it gets SIGWINCH.
func setWinsize(f *os.File, cols, rows int) error {
	ws := struct{ Row, Col, X, Y uint16 }{Row: uint16(rows), Col: uint16(cols)}
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ierr error
	err = rc.Control(func(fd uintptr) {
		ierr = ioctl(fd, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
	})
	if err != nil {
		return err
	}
	return ierr
}
