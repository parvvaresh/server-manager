package main

import (
	"strconv"
	"syscall"
	"unsafe"
)

// ptyUnlock unlocks a new /dev/ptmx and returns the path of its other end.
func ptyUnlock(fd int) (string, error) {
	var unlock int32
	if err := ioctl(uintptr(fd), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		return "", err
	}
	var n uint32
	if err := ioctl(uintptr(fd), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		return "", err
	}
	return "/dev/pts/" + strconv.FormatUint(uint64(n), 10), nil
}
