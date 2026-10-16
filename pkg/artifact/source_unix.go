//go:build linux || darwin

package artifact

import (
	"os"
	"syscall"
)

type fileIdentity struct{ Dev, Ino uint64 }

func sourceIdentity(fi os.FileInfo) fileIdentity {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return fileIdentity{uint64(st.Dev), uint64(st.Ino)}
	}
	return fileIdentity{}
}
