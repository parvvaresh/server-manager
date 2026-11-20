package main

import (
	"bytes"
	"syscall"
	"unsafe"
)

// ptyUnlock grants and unlocks a new /dev/ptmx and returns the path of its
// other end.
func ptyUnlock(fd int) (string, error) {
	if err := ioctl(uintptr(fd), syscall.TIOCPTYGRANT, 0); err != nil {
		return "", err
	}
	if err := ioctl(uintptr(fd), syscall.TIOCPTYUNLK, 0); err != nil {
		return "", err
	}
	buf := make([]byte, 128)
	if err := ioctl(uintptr(fd), syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&buf[0]))); err != nil {
		return "", err
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf), nil
}
