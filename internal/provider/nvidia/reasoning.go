package nvidia

import (
	"strings"

	"glm52-nvidia/internal/models"
)

type reasoningKind uint8

const (
	reasoningEffort reasoningKind = iota
	reasoningToggle
	reasoningEffortAndToggle
	reasoningMiniMax
)

type reasoningProfile struct {
	kind         reasoningKind
	levels       []string
	defaultLevel string
}

// reasoningProfiles carries only the cases that diverge from the generic
// effort-based fallback. Today that is minimax-m3 alone: it sends a custom
// `thinking_mode` kwarg instead of the standard `reasoning_effort`.
//
// Every other reasoning-capable model — current and future — falls through
// to defaultReasoningProfile (built from defaultReasoningLevels + the
// registry's Capability.Reasoning flag).
var reasoningProfiles = map[string]reasoningProfile{
	"minimaxai/minimax-m3": {kind: reasoningMiniMax},
}

// defaultReasoningLevels is what unknown reasoning-capable models get. Listed
// low → high so mapEffort's "round up to next declared level" walks the user
// through low → medium → high → xhigh → max as they ask for more effort.
// "none" sits at the floor so callers can explicitly disable reasoning.
//
// ponytail: 6 tiers covers every NVIDIA model that exposes an effort scale
// today (low/medium/high/xhigh/max). Add a new tier here only when NVIDIA
// ships a level none of these express.
var defaultReasoningLevels = []string{"none", "low", "medium", "high", "xhigh", "max"}

// defaultReasoningProfile is the single profile every unknown reasoning-
// capable model gets. Built from defaultReasoningLevels so adding a tier
// only needs one edit.
var defaultReasoningProfile = reasoningProfile{
	kind:         reasoningEffort,
	levels:       defaultReasoningLevels,
	defaultLevel: "high",
}

var effortRanks = map[string]int{
	"none":    0,
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
}

func normalizeThinking(raw map[string]any) {
	model, _ := raw["model"].(string)
	profile, supported := reasoningProfiles[model]
	kw, hadKwargs := raw["chat_template_kwargs"].(map[string]any)
	if kw == nil {
		kw = map[string]any{}
	}

	effort, hasEffort := requestedEffort(raw, kw, profile.defaultLevel)
	enabled, hasEnabled := requestedThinkingEnabled(raw, kw)
	clearThinking, hasClearThinking := requestedClearThinking(raw, kw)

	// Budget → effort: Claude Code speaks Anthropic shape
	// {"thinking":{"type":"enabled","budget_tokens":N}} with no effort tier.
	// Without this every budget collapses to profile.defaultLevel, so a 1k
	// request overthinks at max effort. Explicit reasoning_effort still wins;
	// an explicit disabled beats any budget.
	budget, hasBudget := requestedBudget(raw)
	explicitlyOff := hasEnabled && !enabled
	if !hasEffort && hasBudget && !explicitlyOff {
		effort = effortFromBudget(budget)
		hasEffort = true
	}

	delete(raw, "thinking")
	delete(raw, "enable_thinking")
	delete(raw, "clear_thinking")
	delete(raw, "reasoning_effort")
	delete(raw, "thinking_budget")

	// Fallback: a model the registry reports as reasoning-capable but without
	// an explicit profile here gets defaultReasoningProfile rather than being
	// silently no-oped. The registry is the single source of truth — per-model
	// overrides live in reasoningProfiles above only when they diverge from the
	// generic profile (different kwarg name, different default level, etc.).
	if !supported {
		if info, err := models.Lookup(model); err == nil && info.Capability != nil && info.Capability.Reasoning {
			profile = defaultReasoningProfile
			supported = true
		}
	}

	if supported {
		switch profile.kind {
		case reasoningEffort:
			delete(kw, "enable_thinking")
			if hasEffort {
				kw["reasoning_effort"] = mapEffort(effort, profile)
			} else if hasEnabled {
				kw["reasoning_effort"] = mapEnabled(enabled, profile)
			}
		case reasoningToggle:
			delete(kw, "reasoning_effort")
			if hasEnabled {
				kw["enable_thinking"] = enabled
			} else if hasEffort {
				kw["enable_thinking"] = effortEnablesThinking(effort)
			}
		case reasoningEffortAndToggle:
			if hasEnabled {
				kw["enable_thinking"] = enabled
			} else if hasEffort {
				kw["enable_thinking"] = effortEnablesThinking(effort)
			}
			if hasEffort && (!hasEnabled || enabled) && effortEnablesThinking(effort) {
				kw["reasoning_effort"] = mapEffort(effort, profile)
			} else {
				delete(kw, "reasoning_effort")
			}
		case reasoningMiniMax:
			delete(kw, "enable_thinking")
			delete(kw, "reasoning_effort")
			if hasEffort {
				kw["thinking_mode"] = miniMaxThinkingMode(effort)
			} else if hasEnabled {
				kw["thinking_mode"] = "disabled"
				if enabled {
					kw["thinking_mode"] = "enabled"
				}
			}
		}
		if hasClearThinking && (profile.kind == reasoningToggle || profile.kind == reasoningEffortAndToggle) {
			kw["clear_thinking"] = clearThinking
		}
	}

	if len(kw) > 0 || hadKwargs {
		raw["chat_template_kwargs"] = kw
	} else {
		delete(raw, "chat_template_kwargs")
	}
}

func requestedEffort(raw, kw map[string]any, defaultLevel string) (string, bool) {
	for _, value := range []any{kw["reasoning_effort"], raw["reasoning_effort"]} {
		if effort, ok := value.(string); ok && strings.TrimSpace(effort) != "" {
			return normalizeEffort(effort, defaultLevel), true
		}
	}
	if thinking, ok := raw["thinking"].(map[string]any); ok {
		if effort, ok := thinking["reasoning_effort"].(string); ok && strings.TrimSpace(effort) != "" {
			return normalizeEffort(effort, defaultLevel), true
		}
	}
	return "", false
}

func requestedThinkingEnabled(raw, kw map[string]any) (bool, bool) {
	if enabled, ok := kw["enable_thinking"].(bool); ok {
		return enabled, true
	}
	if thinking, ok := raw["thinking"].(map[string]any); ok {
		if typ, ok := thinking["type"].(string); ok {
			switch strings.ToLower(strings.TrimSpace(typ)) {
			case "enabled", "enable", "on":
				return true, true
			case "disabled", "disable", "off":
				return false, true
			}
		}
	}
	enabled, ok := raw["enable_thinking"].(bool)
	return enabled, ok
}

// requestedBudget extracts the Anthropic-style thinking budget: Claude Code
// sends {"thinking":{"type":"enabled","budget_tokens":N}}; "thinking_budget"
// is the common top-level alias for the same knob.
func requestedBudget(raw map[string]any) (int, bool) {
	if t, ok := raw["thinking"].(map[string]any); ok {
		if n, ok := jsonInt(t["budget_tokens"]); ok {
			return n, true
		}
	}
	return jsonInt(raw["thinking_budget"])
}

// jsonInt narrows a decoded JSON number to int (map[string]any decoding
// yields float64 for every number).
func jsonInt(v any) (int, bool) {
	n, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int(n), true
}

// effortFromBudget derives an effort tier from a thinking token budget.
//
// ponytail: heuristic thresholds, not spec — NVIDIA exposes no budget knob,
// so this only stops a 1k-budget request from running at max effort (the main
// Claude Code latency bug). Retune the boundaries here if calibration drifts.
func effortFromBudget(t int) string {
	switch {
	case t <= 0:
		return "none"
	case t < 2048:
		return "low"
	case t < 8192:
		return "medium"
	case t < 16384:
		return "high"
	case t < 32768:
		return "xhigh"
	default:
		return "max"
	}
}

func requestedClearThinking(raw, kw map[string]any) (any, bool) {
	if value, ok := kw["clear_thinking"]; ok {
		return value, true
	}
	if thinking, ok := raw["thinking"].(map[string]any); ok {
		if value, ok := thinking["clear_thinking"]; ok {
			return value, true
		}
	}
	value, ok := raw["clear_thinking"]
	return value, ok
}

func normalizeEffort(effort, defaultLevel string) string {
	normalized := strings.ToLower(strings.TrimSpace(effort))
	if _, ok := effortRanks[normalized]; ok {
		return normalized
	}
	return defaultLevel
}

func mapEffort(effort string, profile reasoningProfile) string {
	requestedRank := effortRanks[effort]
	for _, level := range profile.levels {
		if effortRanks[level] >= requestedRank {
			return level
		}
	}
	return profile.levels[len(profile.levels)-1]
}

func mapEnabled(enabled bool, profile reasoningProfile) string {
	if !enabled {
		return mapEffort("none", profile)
	}
	return profile.defaultLevel
}

func effortEnablesThinking(effort string) bool {
	return effort != "none"
}

func miniMaxThinkingMode(effort string) string {
	switch effort {
	case "none":
		return "disabled"
	case "minimal", "low", "medium":
		return "adaptive"
	default:
		return "enabled"
	}
}
