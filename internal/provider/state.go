package provider

import "fmt"

// State represents the current lifecycle state of a provider connection.
type State string

const (
	StateIdle        State = "idle"
	StateActive      State = "active"
	StateRateLimited State = "rate_limited"
	StateCooldown    State = "cooldown"
	StateAuthExpired State = "auth_expired"
	StateRefreshing  State = "refreshing"
	StateErrored     State = "errored"
	StateDisabled    State = "disabled"
)

// AllStates lists every valid state for validation purposes.
var AllStates = []State{
	StateIdle,
	StateActive,
	StateRateLimited,
	StateCooldown,
	StateAuthExpired,
	StateRefreshing,
	StateErrored,
	StateDisabled,
}

// validTransitions encodes every legal state transition.
// Key = source state, value = set of reachable target states.
var validTransitions = map[State]map[State]bool{
	StateIdle: {
		StateActive:   true,
		StateDisabled: true,
	},
	StateActive: {
		StateRateLimited: true,
		StateAuthExpired: true,
		StateErrored:     true,
		StateIdle:        true,
		StateDisabled:    true,
	},
	StateRateLimited: {
		StateCooldown: true,
		StateDisabled: true,
	},
	StateCooldown: {
		StateIdle:     true,
		StateDisabled: true,
	},
	StateAuthExpired: {
		StateRefreshing: true,
		StateIdle:       true, // auto_detect connections can recover when fresh creds available
		StateDisabled:   true,
	},
	StateRefreshing: {
		StateActive:   true,
		StateErrored:  true,
		StateDisabled: true,
	},
	StateErrored: {
		StateIdle:     true,
		StateDisabled: true,
	},
	StateDisabled: {
		StateIdle: true,
	},
}

// ValidTransitions returns a copy of the full transition table.
func ValidTransitions() map[State]map[State]bool {
	out := make(map[State]map[State]bool, len(validTransitions))
	for from, targets := range validTransitions {
		inner := make(map[State]bool, len(targets))
		for to, ok := range targets {
			inner[to] = ok
		}
		out[from] = inner
	}
	return out
}

// CanTransition reports whether moving from one state to another is permitted.
func CanTransition(from, to State) bool {
	targets, ok := validTransitions[from]
	if !ok {
		return false
	}
	return targets[to]
}

// ErrInvalidTransition is returned when a state transition is not allowed.
type ErrInvalidTransition struct {
	From State
	To   State
}

func (e *ErrInvalidTransition) Error() string {
	return fmt.Sprintf("invalid state transition: %s -> %s", e.From, e.To)
}

// statePriority returns a sort key used during provider selection.
// Lower values are preferred. Idle connections are preferred over active ones
// (active means in-flight), and both are preferred over degraded states.
func statePriority(s State) int {
	switch s {
	case StateIdle:
		return 0
	case StateActive:
		return 1
	case StateCooldown:
		return 5
	case StateRefreshing:
		return 6
	case StateErrored:
		return 7
	case StateRateLimited:
		return 8
	case StateAuthExpired:
		return 9
	case StateDisabled:
		return 10
	default:
		return 99
	}
}
