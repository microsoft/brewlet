package provisioner

import (
	"fmt"
	"os"
	"syscall"
)

func trustedFile(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return trustedFileInfo(path, st)
}

func trustedFileInfo(path string, st os.FileInfo) error {
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("untrusted host file %s", path)
	}
	return nil
}
