//go:build !windows

package mediafile

import (
	"os"
	"syscall"
)

const readFlags = os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW
