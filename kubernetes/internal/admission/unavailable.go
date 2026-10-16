package admission

import "fmt"

// InventoryUnavailable is an outage, not a determination that a requested ID
// is unknown or incompatible. Pod transport is deliberately fail-open; trusted
// local authorization, identity and barriers remain the execution boundary.
type InventoryUnavailable struct{ Cause error }

func (e *InventoryUnavailable) Error() string {
	return fmt.Sprintf("Brewlet inventory unavailable: %v", e.Cause)
}
func (e *InventoryUnavailable) Unwrap() error { return e.Cause }
