package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal reports whether f is a real terminal. A mode check alone is not
// enough: /dev/null is a character device too, so ask for its termios instead.
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGETA, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
