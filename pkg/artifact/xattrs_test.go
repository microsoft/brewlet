package artifact

import "testing"

func TestFirstRejectedXattr(t *testing.T) {
	for _, c := range []struct {
		list string
		want string
	}{
		{"", ""},
		{"com.apple.provenance\x00", ""},
		{"com.apple.quarantine\x00com.apple.provenance\x00", ""},
		{"com.apple.provenance\x00user.test\x00", "user.test"},
		{"com.apple.FinderInfo\x00", "com.apple.FinderInfo"},
		{"com.apple.ResourceFork\x00", "com.apple.ResourceFork"},
		{"com.apple.macl\x00", "com.apple.macl"},
		{"security.capability\x00", "security.capability"},
	} {
		if got := firstRejectedXattr([]byte(c.list), darwinHostLocalXattrs); got != c.want {
			t.Errorf("firstRejectedXattr(%q) = %q, want %q", c.list, got, c.want)
		}
	}
	if got := firstRejectedXattr([]byte("com.apple.provenance\x00"), nil); got != "com.apple.provenance" {
		t.Errorf("nil allowlist accepted %q", got)
	}
}
