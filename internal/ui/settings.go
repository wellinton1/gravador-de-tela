//go:build windows

package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings persist the window's choices across runs, like vokoscreen's
// settings tab, in a JSON file under %APPDATA%. Everything here has a sane
// zero value, so a missing or corrupt file simply means defaults.

type Settings struct {
	Dir        string `json:"dir"`
	WithAudio  bool   `json:"withAudio"`
	FPS        int    `json:"fpsIdx"`
	Quality    int    `json:"qualityIdx"`
	Countdown  int    `json:"countdownIdx"`
	LimitMin   int    `json:"limitIdx"`
	ShowClick  bool   `json:"showClick"`
	Halo       bool   `json:"halo"`
	DrawCursor bool   `json:"drawCursor"`
	Display    int    `json:"display"`
}

// DefaultSettings is what a first run looks like: H.264-friendly quality,
// sound on, no countdown, no time limit.
func DefaultSettings() Settings {
	return Settings{WithAudio: true, FPS: 1, Quality: 1}
}

func settingsPath() string {
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		return filepath.Join(appdata, "GravadorDeTela", "config.json")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".gravador-de-tela.json")
	}
	return "gravador-de-tela.json"
}

// LoadSettings reads the file, returning defaults when it is absent or bad.
func LoadSettings() Settings {
	def := DefaultSettings()
	raw, err := os.ReadFile(settingsPath())
	if err != nil {
		return def
	}
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return def
	}
	if s.FPS < 0 || s.FPS > 2 {
		s.FPS = def.FPS
	}
	if s.Quality < 0 || s.Quality > 2 {
		s.Quality = def.Quality
	}
	if s.Countdown < 0 || s.Countdown > 3 {
		s.Countdown = 0
	}
	if s.LimitMin < 0 || s.LimitMin > 5 {
		s.LimitMin = 0
	}
	return s
}

// Save writes the file, creating its folder; a failure is swallowed because
// settings must never break a recording.
func (s Settings) Save() {
	path := settingsPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(path, raw, 0o644)
}
