//go:build !darwin

package whatsappdb

import "os"

func fileMaterialized(os.FileInfo) bool { return true }

func mediaReadWouldBlock(error) bool { return false }
