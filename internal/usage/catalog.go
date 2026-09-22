package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// CatalogModel is one entry of MiMo's own model catalogue. The ratio is the quota cost per
// token that the desktop UI shows as "0.4x"/"1.0x"/"10.0x".
type CatalogModel struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Type  string  `json:"modelType"`
	Ratio float64 `json:"displayRatio"`
}

// CatalogPath is where the desktop app caches the models available to the signed-in account.
// Reading it, rather than hardcoding ids, is what keeps the multipliers correct across MiMo
// updates.
func CatalogPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Xiaomi MiMo", "model-catalog.json"), nil
}

// LoadTextModels returns the chat-capable models from the catalogue, cheapest ratio first.
// It returns an error when the file is missing or unreadable so callers can fall back
// deliberately instead of silently trusting stale numbers.
func LoadTextModels() ([]CatalogModel, error) {
	path, err := CatalogPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 MiMo 模型目录: %w", err)
	}
	var payload struct {
		Account string         `json:"account"`
		Models  []CatalogModel `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("解析 MiMo 模型目录: %w", err)
	}
	var text []CatalogModel
	for _, m := range payload.Models {
		if m.Type == "TEXT" && m.ID != "" {
			text = append(text, m)
		}
	}
	if len(text) == 0 {
		return nil, fmt.Errorf("%s 里没有 TEXT 模型", path)
	}
	sort.Slice(text, func(i, j int) bool {
		if text[i].Ratio != text[j].Ratio {
			return text[i].Ratio < text[j].Ratio
		}
		return text[i].ID < text[j].ID
	})
	return text, nil
}

// PreferredModel picks the default target for clients that ask for an unknown model. It
// deliberately avoids the highest-ratio entry: a 10x model as the default would burn the
// quota far faster than the user expects.
func PreferredModel(models []CatalogModel) string {
	if len(models) == 0 {
		return ""
	}
	// The middle option when there are three (flash / pro / ultraspeed), else the cheapest.
	if len(models) >= 3 {
		return models[len(models)/2].ID
	}
	return models[0].ID
}
