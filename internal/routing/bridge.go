package routing

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	defaultBridgeMaxTokens = 4000
	defaultBridgeTurns     = 3 // keep bridge for 3 turns after switch
	slidingWindowTurns     = 3 // include last 3 turns in bridge
)

// BridgeConfig controls context bridge behavior.
type BridgeConfig struct {
	MaxTokens int // max bridge size in estimated tokens
	KeepTurns int // how many turns to keep the bridge after injection
}

// DefaultBridgeConfig returns sensible defaults.
func DefaultBridgeConfig() BridgeConfig {
	return BridgeConfig{
		MaxTokens: defaultBridgeMaxTokens,
		KeepTurns: defaultBridgeTurns,
	}
}

// BridgeState tracks the lifecycle of an injected bridge per session.
type BridgeState struct {
	Active       bool
	TurnsLeft    int
	PreviousModel string
}

// BuildContextBridge generates a structured handoff note from conversation history.
// This is Tier 1: heuristic extraction + sliding window. Zero LLM calls.
//
// The bridge has two parts:
//  1. Structured state — files, decisions, current task (extracted via regex)
//  2. Sliding window — last 2-3 full turns (uncompressed recent context)
//
// Returns empty string if history is nil or too short.
func BuildContextBridge(history *History, maxTokens int) string {
	if history == nil || len(history.Turns) < 2 {
		return ""
	}

	var bridge strings.Builder
	bridge.WriteString(fmt.Sprintf("## Context from previous assistant (%s)\n\n", history.LastModel))

	// Part 1: Structured state
	state := extractStructuredState(history)
	if state != "" {
		bridge.WriteString(state)
		bridge.WriteString("\n")
	}

	// Part 2: Sliding window (last N turns)
	recentTurns := lastNTurns(history, slidingWindowTurns)
	if len(recentTurns) > 0 {
		bridge.WriteString("### Recent context\n\n")
		tokensPerTurn := (maxTokens - estimateTokens(state)) / max(len(recentTurns), 1)
		if tokensPerTurn < 200 {
			tokensPerTurn = 200
		}

		for _, turn := range recentTurns {
			content := DecompressTurn(turn)
			truncated := truncateToTokens(content, tokensPerTurn)
			bridge.WriteString(fmt.Sprintf("**%s:**\n%s\n\n", turn.Role, truncated))
		}
	}

	result := bridge.String()
	return truncateToTokens(result, maxTokens)
}

// extractStructuredState scans conversation for concrete artifacts:
// file paths, decisions, and the current task.
func extractStructuredState(history *History) string {
	var state strings.Builder

	files := extractFileReferences(history)
	decisions := extractDecisions(history)

	if len(files) > 0 {
		state.WriteString("**Files:** ")
		state.WriteString(strings.Join(files, ", "))
		state.WriteString("\n")
	}

	if len(decisions) > 0 {
		state.WriteString("**Decisions:**\n")
		for _, d := range decisions {
			state.WriteString("- ")
			state.WriteString(d)
			state.WriteString("\n")
		}
	}

	return state.String()
}

// File path patterns: /path/to/file, path/to/file.ext
var filePathRegex = regexp.MustCompile(`(?:^|\s)((?:[a-zA-Z]:)?(?:/[\w.-]+)+\.[\w]+)`)

// extractFileReferences finds file paths mentioned in assistant messages.
func extractFileReferences(history *History) []string {
	seen := map[string]bool{}
	var files []string

	for _, turn := range history.Turns {
		if turn.Role != "assistant" {
			continue
		}
		content := DecompressTurn(turn)
		matches := filePathRegex.FindAllStringSubmatch(content, -1)
		for _, m := range matches {
			path := strings.TrimSpace(m[1])
			if !seen[path] && len(path) > 3 {
				seen[path] = true
				files = append(files, path)
			}
		}
	}

	// Limit to 20 most recent files
	if len(files) > 20 {
		files = files[len(files)-20:]
	}
	return files
}

// Decision patterns: "I'll use X", "Let's go with X", "I chose X", "decided to X"
var decisionRegex = regexp.MustCompile(`(?i)(?:I'll use|let's go with|I chose|chose to use|decided to|going with|picked|selected|using)\s+(.{5,80})`)

// extractDecisions finds decision statements from assistant messages.
func extractDecisions(history *History) []string {
	var decisions []string

	for _, turn := range history.Turns {
		if turn.Role != "assistant" {
			continue
		}
		content := DecompressTurn(turn)
		matches := decisionRegex.FindAllStringSubmatch(content, 5) // max 5 per turn
		for _, m := range matches {
			d := strings.TrimSpace(m[0])
			// Clean up trailing punctuation
			d = strings.TrimRight(d, ".,;:")
			if len(d) > 10 {
				decisions = append(decisions, d)
			}
		}
	}

	// Limit to 10 most recent decisions
	if len(decisions) > 10 {
		decisions = decisions[len(decisions)-10:]
	}
	return decisions
}

func lastNTurns(history *History, n int) []Turn {
	if len(history.Turns) <= n {
		return history.Turns
	}
	return history.Turns[len(history.Turns)-n:]
}

func truncateToTokens(s string, maxTokens int) string {
	maxChars := maxTokens * 4 // ~4 chars per token
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n...(truncated)"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func estimateTokens(s string) int {
	return (len(s) + 3) / 4
}
