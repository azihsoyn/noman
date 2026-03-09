package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const maxFewShotExamples = 5

type HistoryEntry struct {
	Command   string    `json:"command"`
	Prompt    string    `json:"prompt"`
	Args      []string  `json:"args"`
	StdinHash string    `json:"stdin_hash,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UseCount  int       `json:"use_count"`
}

type History struct {
	Entries []HistoryEntry `json:"entries"`
	path    string
}

func loadHistory() *History {
	dir := configDir()
	path := filepath.Join(dir, "history.json")

	h := &History{path: path}

	data, err := os.ReadFile(path)
	if err != nil {
		return h
	}

	_ = json.Unmarshal(data, h)
	return h
}

func (h *History) save() error {
	dir := filepath.Dir(h.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Trim old entries if over limit
	max := loadConfig().MaxHistory
	if len(h.Entries) > max {
		h.Entries = h.Entries[len(h.Entries)-max:]
	}

	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(h.path, data, 0644)
}

func stdinHash(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	hash := sha256.Sum256(data)
	return fmt.Sprintf("%x", hash[:8])
}

// FindExact returns cached args if the same command+prompt+stdin was used before.
func (h *History) FindExact(command, prompt string, sHash string) ([]string, bool) {
	for i := len(h.Entries) - 1; i >= 0; i-- {
		e := &h.Entries[i]
		if e.Command == command && e.Prompt == prompt && e.StdinHash == sHash {
			e.UseCount++
			return e.Args, true
		}
	}
	return nil, false
}

// FindByPrompt returns cached command+args matching prompt+stdin (for "which" mode).
func (h *History) FindByPrompt(prompt string, sHash string) (string, []string, bool) {
	for i := len(h.Entries) - 1; i >= 0; i-- {
		e := &h.Entries[i]
		if e.Prompt == prompt && e.StdinHash == sHash {
			e.UseCount++
			return e.Command, e.Args, true
		}
	}
	return "", nil, false
}

// FewShotExamples returns recent history entries for the same command
// to use as few-shot examples in the AI prompt.
func (h *History) FewShotExamples(command string) []HistoryEntry {
	var examples []HistoryEntry
	seen := make(map[string]bool)

	// Walk backwards to get most recent first, deduplicate by prompt
	for i := len(h.Entries) - 1; i >= 0; i-- {
		e := h.Entries[i]
		if e.Command == command && !seen[e.Prompt] {
			examples = append(examples, e)
			seen[e.Prompt] = true
			if len(examples) >= maxFewShotExamples {
				break
			}
		}
	}

	// Reverse to chronological order
	for i, j := 0, len(examples)-1; i < j; i, j = i+1, j-1 {
		examples[i], examples[j] = examples[j], examples[i]
	}
	return examples
}

// Add records a new prompt→args mapping.
func (h *History) Add(command, prompt string, args []string, stdinData []byte) {
	// Update existing entry if same command+prompt
	for i := range h.Entries {
		if h.Entries[i].Command == command && h.Entries[i].Prompt == prompt {
			h.Entries[i].Args = args
			h.Entries[i].UseCount++
			h.Entries[i].CreatedAt = time.Now()
			return
		}
	}

	h.Entries = append(h.Entries, HistoryEntry{
		Command:   command,
		Prompt:    prompt,
		Args:      args,
		StdinHash: stdinHash(stdinData),
		CreatedAt: time.Now(),
		UseCount:  1,
	})
}
