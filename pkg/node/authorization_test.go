package node

import (
	"context"
	"strings"
	"testing"
)

func TestAuthorizationCannotAdoptForeignProfileOrReuseGeneration(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testAuthorization()
	if err := s.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	foreign := a
	foreign.ProfileUID = "foreign-profile"
	if err := s.WriteAuthorization(ctx, foreign); reason(err) != "ownership-fence-failed" {
		t.Fatal("foreign authorization adopted", err)
	}
	reused := a
	reused.Digest = "sha256:" + strings.Repeat("c", 64)
	if err := s.WriteAuthorization(ctx, reused); err == nil {
		t.Fatal("generation reused for new digest")
	}
}
