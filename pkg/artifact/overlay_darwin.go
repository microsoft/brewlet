//go:build darwin

package artifact

import "fmt"

func overlayWhiteout(string) error {
	return fmt.Errorf("overlay whiteout materialization requires Linux")
}
func overlayOpaque(string) error { return fmt.Errorf("overlay opaque materialization requires Linux") }
