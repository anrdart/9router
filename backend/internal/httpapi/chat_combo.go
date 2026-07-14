package httpapi

import (
	"context"
	"strings"
	"sync"

	"9router/backend/internal/database"
)

type comboRotation struct {
	index           int
	consecutiveUses int
}

var comboRotations = struct {
	sync.Mutex
	byName map[string]comboRotation
}{byName: map[string]comboRotation{}}

func resolveNativeCombo(ctx context.Context, db *database.DB, req nativeChatRequest, settings map[string]any) ([]resolvedNativeChat, bool) {
	if db == nil || req.Model == "" || strings.Contains(req.Model, "/") || !nativeOpenAIChatBody(req.Body) {
		return nil, false
	}
	if _, ok := req.Body["stream"].(bool); !ok || comboNeedsCapabilityRouting(req.Body) {
		return nil, false
	}
	combo, err := db.GetComboByName(ctx, req.Model)
	if err != nil || len(combo.Models) == 0 || (combo.Kind != "" && combo.Kind != llmKind) {
		return nil, false
	}
	strategy := nativeComboStrategy(settings, combo.Name)
	if strategy == "fusion" {
		return nil, false
	}
	models, commitRotation := previewComboModels(combo.Name, combo.Models, strategy, nativeComboStickyLimit(settings))
	resolutions := make([]resolvedNativeChat, 0, len(models))
	for _, model := range models {
		sub := nativeChatRequest{Model: model, Stream: req.Stream, Body: req.Body}
		base, subSettings, ok := resolveNativeChatBase(ctx, db, sub)
		if !ok {
			return nil, false
		}
		resolution, ok := selectNativeChatAccount(ctx, db, base, subSettings, nil)
		if !ok {
			return nil, false
		}
		resolutions = append(resolutions, resolution)
	}
	if commitRotation != nil {
		commitRotation()
	}
	return resolutions, len(resolutions) > 0
}

func nativeComboStrategy(settings map[string]any, name string) string {
	if strategies, ok := settings["comboStrategies"].(map[string]any); ok {
		if cfg, ok := strategies[name].(map[string]any); ok {
			if strategy, _ := cfg["fallbackStrategy"].(string); strategy != "" {
				return strategy
			}
		}
	}
	if strategy, _ := settings["comboStrategy"].(string); strategy != "" {
		return strategy
	}
	return "fallback"
}

func nativeComboStickyLimit(settings map[string]any) int {
	if n := numberSetting(settings["comboStickyRoundRobinLimit"]); n > 0 {
		return n
	}
	return 1
}

func previewComboModels(name string, models []string, strategy string, stickyLimit int) ([]string, func()) {
	out := append([]string(nil), models...)
	if strategy != "round-robin" || len(out) < 2 {
		return out, nil
	}
	if stickyLimit < 1 {
		stickyLimit = 1
	}
	comboRotations.Lock()
	state := comboRotations.byName[name]
	start := state.index % len(out)
	rotated := append(append([]string(nil), out[start:]...), out[:start]...)
	comboRotations.Unlock()
	return rotated, func() {
		comboRotations.Lock()
		state := comboRotations.byName[name]
		state.consecutiveUses++
		if state.consecutiveUses >= stickyLimit {
			state.index = (state.index + 1) % len(out)
			state.consecutiveUses = 0
		}
		comboRotations.byName[name] = state
		comboRotations.Unlock()
	}
}

func comboNeedsCapabilityRouting(body map[string]any) bool {
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if _, plain := message["content"].(string); !plain {
			return true
		}
	}
	return false
}
