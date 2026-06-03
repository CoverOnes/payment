package domain

// validTransitions defines the complete state machine for transaction lifecycle.
// Table-driven: each entry is (from, to) => allowed.
// This is the single source of truth for all valid transitions.
//
// PENDING -> HELD     : hold/escrow (funds held from payer)
// HELD    -> RELEASED : release to payee
// HELD    -> REFUNDED : refund back to payer
// PENDING -> FAILED   : failure before hold
// HELD    -> FAILED   : failure after hold (e.g. provider error)
//
// Terminal states: RELEASED, REFUNDED, FAILED — no transitions out.
var validTransitions = map[Status]map[Status]bool{
	StatusPending: {
		StatusHeld:   true,
		StatusFailed: true,
	},
	StatusHeld: {
		StatusReleased: true,
		StatusRefunded: true,
		StatusFailed:   true,
	},
	// RELEASED, REFUNDED, FAILED are terminal — no outgoing transitions.
}

// CanTransition returns true if transitioning from → to is valid.
func CanTransition(from, to Status) bool {
	targets, ok := validTransitions[from]
	if !ok {
		return false
	}

	return targets[to]
}

// Transition validates and returns the new status.
// Returns ErrInvalidTransition if the transition is not allowed.
func Transition(current, next Status) (Status, error) {
	if !CanTransition(current, next) {
		return current, ErrInvalidTransition
	}

	return next, nil
}
