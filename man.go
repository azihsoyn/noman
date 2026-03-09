package main

import (
	"fmt"
	"os"
	"strings"
)

const (
	bold  = "\033[1m"
	uline = "\033[4m"
	reset = "\033[0m"
)

func showMan(args []string) {
	history := loadHistory()

	if len(args) == 0 {
		showManOverview(history)
		return
	}

	command := args[0]
	showManCommand(history, command)
}

func showManOverview(h *History) {
	// Collect entries per command, preserving insertion order
	grouped := make(map[string][]HistoryEntry)
	var order []string

	for _, e := range h.Entries {
		if _, ok := grouped[e.Command]; !ok {
			order = append(order, e.Command)
		}
		grouped[e.Command] = append(grouped[e.Command], e)
	}

	if len(order) == 0 {
		fmt.Fprintf(os.Stderr, "No history yet. Start using noman to build your personal man pages.\n")
		return
	}

	fmt.Fprintf(os.Stdout, "\n%sNOMAN(1)%s                        %sPersonal Command Reference%s                        %sNOMAN(1)%s\n\n", bold, reset, bold, reset, bold, reset)
	fmt.Fprintf(os.Stdout, "%sNAME%s\n", bold, reset)
	fmt.Fprintf(os.Stdout, "       noman - your personal command history\n\n")

	for _, cmd := range order {
		entries := grouped[cmd]

		// Deduplicate by prompt, keep most recent
		seen := make(map[string]bool)
		var unique []HistoryEntry
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			if !seen[e.Prompt] {
				seen[e.Prompt] = true
				unique = append([]HistoryEntry{e}, unique...)
			}
		}

		fmt.Fprintf(os.Stdout, "%s%s%s (%d entries)\n", bold, strings.ToUpper(cmd), reset, len(unique))
		for _, e := range unique {
			fmt.Fprintf(os.Stdout, "       %s$ %s %s%s\n", bold, cmd, strings.Join(e.Args, " "), reset)
			fmt.Fprintf(os.Stdout, "           %s\n", e.Prompt)
			if e.UseCount > 1 {
				fmt.Fprintf(os.Stdout, "           (used %d times)\n", e.UseCount)
			}
			fmt.Println()
		}
	}
}

func showManCommand(h *History, command string) {
	var entries []HistoryEntry
	for _, e := range h.Entries {
		if e.Command == command {
			entries = append(entries, e)
		}
	}

	if len(entries) == 0 {
		fmt.Fprintf(os.Stderr, "No history for '%s'. Try: noman %s \"<prompt>\"\n", command, command)
		return
	}

	header := fmt.Sprintf("NOMAN-%s(1)", strings.ToUpper(command))
	title := fmt.Sprintf("Personal %s Reference", strings.ToUpper(command))
	fmt.Fprintf(os.Stdout, "\n%s%s%s%s%s%s%s%s\n\n",
		bold, header, reset,
		centerPad(header, title),
		bold, title, reset,
		centerPad(title, header)+bold+header+reset)

	fmt.Fprintf(os.Stdout, "%sNAME%s\n", bold, reset)
	fmt.Fprintf(os.Stdout, "       %s - learned from %d past uses\n\n", command, len(entries))

	fmt.Fprintf(os.Stdout, "%sUSAGE%s\n", bold, reset)

	// Deduplicate by prompt, show most recent args
	seen := make(map[string]bool)
	var unique []HistoryEntry
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if !seen[e.Prompt] {
			seen[e.Prompt] = true
			unique = append([]HistoryEntry{e}, unique...)
		}
	}

	for _, e := range unique {
		fmt.Fprintf(os.Stdout, "       %s$ %s %s%s\n", bold, command, strings.Join(e.Args, " "), reset)
		fmt.Fprintf(os.Stdout, "           %s\n", e.Prompt)
		if e.UseCount > 1 {
			fmt.Fprintf(os.Stdout, "           (used %d times)\n", e.UseCount)
		}
		fmt.Println()
	}

	fmt.Fprintf(os.Stdout, "%sHISTORY%s\n", bold, reset)
	fmt.Fprintf(os.Stdout, "       %d unique prompts, %d total uses\n\n", len(unique), len(entries))
}

func centerPad(left, center string) string {
	// Simple padding between header elements
	total := 78
	used := len(left) + len(center)
	pad := (total - used) / 2
	if pad < 2 {
		pad = 2
	}
	return strings.Repeat(" ", pad)
}
