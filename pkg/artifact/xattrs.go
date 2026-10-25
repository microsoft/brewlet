package artifact

import "strings"

// darwinHostLocalXattrs are macOS bookkeeping attributes that the OS attaches
// to files (and may re-apply after removal). They carry no meaning inside a
// Linux image layer, and layer tar headers never include xattrs.
var darwinHostLocalXattrs = map[string]bool{
	"com.apple.provenance": true,
	"com.apple.quarantine": true,
}

// firstRejectedXattr parses a NUL-separated listxattr buffer and returns the
// first name not present in ignored, or "" when every name is ignored.
func firstRejectedXattr(list []byte, ignored map[string]bool) string {
	for _, name := range strings.Split(string(list), "\x00") {
		if name != "" && !ignored[name] {
			return name
		}
	}
	return ""
}
