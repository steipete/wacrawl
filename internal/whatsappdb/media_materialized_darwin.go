//go:build darwin

package whatsappdb

import (
	"errors"
	"os"
	"syscall"
)

// sfDataless mirrors SF_DATALESS from <sys/stat.h>: the file's bytes live with
// a dataless-file provider (for WhatsApp media, content that has not been
// downloaded yet) and reading it would block until macOS materializes it.
const sfDataless = 0x40000000

func fileMaterialized(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return stat.Flags&sfDataless == 0
}

func mediaReadWouldBlock(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK)
}
