package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const usage = `Usage: noman [options] <command> "<prompt>"

Options:
  --no-cache    Skip cache and always call AI
  --debug       Show generated args without executing
  --help, -h    Show this help

Examples:
  cat data.json | noman jq "filter items where title contains XYZ"
  noman curl "fetch HTML from example.com"
  cat log.txt | noman grep "extract lines that look like errors"
  noman --no-cache jq "regenerate without cache"

Environment variables:
  NOMAN_BACKEND      "cli" (default, uses claude command) or "api"
  NOMAN_CLAUDE_PATH  Path to claude command (default: auto-detect from PATH)
  NOMAN_API_KEY      API key for api backend (default: ANTHROPIC_API_KEY)
  NOMAN_MODEL        Model name (default: claude-sonnet-4-20250514)
  NOMAN_BASE_URL     API base URL (default: https://api.anthropic.com)
  NOMAN_CONFIG_DIR   Config/history directory (default: ~/.config/noman)
`

type options struct {
	noCache bool
	debug   bool
	command string
	prompt  string
}

func parseOptions() options {
	var opts options
	args := os.Args[1:]

	// Extract flags
	var rest []string
	for _, a := range args {
		switch a {
		case "--no-cache":
			opts.noCache = true
		case "--debug":
			opts.debug = true
		case "--help", "-h":
			fmt.Fprint(os.Stderr, usage)
			os.Exit(0)
		default:
			rest = append(rest, a)
		}
	}

	if len(rest) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	opts.command = rest[0]
	opts.prompt = strings.Join(rest[1:], " ")
	return opts
}

func main() {
	opts := parseOptions()
	command := opts.command
	prompt := opts.prompt

	// Read stdin if piped
	var stdinData []byte
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		var err error
		stdinData, err = io.ReadAll(os.Stdin)
		if err != nil {
			fatal("failed to read stdin: %v", err)
		}
	}

	// Load history
	history := loadHistory()

	// Check exact cache first (unless disabled)
	if !opts.noCache {
		if args, ok := history.FindExact(command, prompt); ok {
			fmt.Fprintf(os.Stderr, "[noman] (cached) %s %s\n", command, strings.Join(args, " "))
			if opts.debug {
				return
			}
			if err := execute(command, args, stdinData); err != nil {
				os.Exit(1)
			}
			_ = history.save()
			return
		}
	}

	// Get command help text
	helpText := getCommandHelp(command)

	// Get few-shot examples from history
	examples := history.FewShotExamples(command)

	// Generate args via AI
	spin := NewSpinner(fmt.Sprintf("Generating args for %s...", command))
	spin.Start()
	args, err := generateArgs(command, prompt, helpText, stdinData, examples)
	spin.Stop()
	if err != nil {
		fatal("failed to generate args: %v", err)
	}

	// Save to history
	history.Add(command, prompt, args, stdinData)
	if err := history.save(); err != nil {
		fmt.Fprintf(os.Stderr, "[noman] warning: failed to save history: %v\n", err)
	}

	// Print the generated command to stderr for visibility
	fmt.Fprintf(os.Stderr, "[noman] %s %s\n", command, strings.Join(args, " "))

	if opts.debug {
		return
	}

	// Execute the command
	if err := execute(command, args, stdinData); err != nil {
		os.Exit(1)
	}
}

func getCommandHelp(command string) string {
	// Try --help first, then -h, then man
	for _, flag := range []string{"--help", "-h"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		cmd := exec.CommandContext(ctx, command, flag)
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil && len(out) > 0 {
			return truncate(string(out), 4000)
		}
		// Some commands print help to stderr and exit non-zero, still useful
		if len(out) > 100 {
			return truncate(string(out), 4000)
		}
	}

	// Try man page
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "man", command)
	cmd.Env = append(os.Environ(), "MANPAGER=cat", "COLUMNS=120")
	out, err := cmd.Output()
	if err == nil && len(out) > 0 {
		return truncate(string(out), 4000)
	}

	return ""
}

func generateArgs(command, prompt, helpText string, stdinData []byte, examples []HistoryEntry) ([]string, error) {
	backend := os.Getenv("NOMAN_BACKEND")
	if backend == "" {
		backend = "cli"
	}

	switch backend {
	case "cli":
		return generateArgsCLI(command, prompt, helpText, stdinData, examples)
	case "api":
		return generateArgsAPI(command, prompt, helpText, stdinData, examples)
	default:
		return nil, fmt.Errorf("unknown backend: %s (use 'cli' or 'api')", backend)
	}
}

func generateArgsCLI(command, prompt, helpText string, stdinData []byte, examples []HistoryEntry) ([]string, error) {
	claudePath := os.Getenv("NOMAN_CLAUDE_PATH")
	if claudePath == "" {
		var err error
		claudePath, err = exec.LookPath("claude")
		if err != nil {
			return nil, fmt.Errorf("claude command not found. Set NOMAN_CLAUDE_PATH or install Claude Code, or use NOMAN_BACKEND=api")
		}
	}

	systemPrompt := buildSystemPrompt(command, helpText, stdinData, examples)
	fullPrompt := systemPrompt + "\n\nUser request: " + prompt

	cmd := exec.Command(claudePath, "-p", fullPrompt)
	// Allow running inside Claude Code sessions
	env := os.Environ()
	filteredEnv := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, "CLAUDECODE=") {
			filteredEnv = append(filteredEnv, e)
		}
	}
	cmd.Env = filteredEnv
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("claude command failed: %v\n%s", err, stderr.String())
	}

	text := strings.TrimSpace(stdout.String())
	return parseArgs(text), nil
}

func generateArgsAPI(command, prompt, helpText string, stdinData []byte, examples []HistoryEntry) ([]string, error) {
	apiKey := os.Getenv("NOMAN_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("set ANTHROPIC_API_KEY or NOMAN_API_KEY environment variable")
	}

	model := os.Getenv("NOMAN_MODEL")
	if model == "" {
		model = "claude-sonnet-4-20250514"
	}

	baseURL := os.Getenv("NOMAN_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	systemPrompt := buildSystemPrompt(command, helpText, stdinData, examples)

	reqBody := anthropicRequest{
		Model:     model,
		MaxTokens: 1024,
		System:    systemPrompt,
		Messages: []message{
			{Role: "user", Content: prompt},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var apiResp anthropicResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %v", err)
	}

	if len(apiResp.Content) == 0 {
		return nil, fmt.Errorf("empty response from API")
	}

	text := strings.TrimSpace(apiResp.Content[0].Text)
	return parseArgs(text), nil
}

func buildSystemPrompt(command, helpText string, stdinData []byte, examples []HistoryEntry) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`You are a command-line argument generator. Your task is to convert a natural language description into command-line arguments for the "%s" command.

RULES:
- Output ONLY the arguments, nothing else. No explanation, no markdown.
- Do NOT include the command name itself.
- If stdin data is provided, consider its structure when generating arguments.
- Output one argument per line. If an argument contains spaces, wrap it in single quotes.
`, command))

	if helpText != "" {
		sb.WriteString(fmt.Sprintf("\nCommand help:\n```\n%s\n```\n", helpText))
	}

	if len(stdinData) > 0 {
		sample := truncate(string(stdinData), 2000)
		sb.WriteString(fmt.Sprintf("\nSample of stdin data:\n```\n%s\n```\n", sample))
	}

	if len(examples) > 0 {
		sb.WriteString("\nPrevious successful conversions for reference:\n")
		for _, e := range examples {
			sb.WriteString(fmt.Sprintf("- \"%s\" → %s\n", e.Prompt, strings.Join(e.Args, " ")))
		}
	}

	return sb.String()
}

func parseArgs(text string) []string {
	lines := strings.Split(text, "\n")
	var args []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Handle quoted arguments
		if (strings.HasPrefix(line, "'") && strings.HasSuffix(line, "'")) ||
			(strings.HasPrefix(line, "\"") && strings.HasSuffix(line, "\"")) {
			line = line[1 : len(line)-1]
		}
		args = append(args, line)
	}
	return args
}

func execute(command string, args []string, stdinData []byte) error {
	cmd := exec.Command(command, args...)
	if len(stdinData) > 0 {
		cmd.Stdin = bytes.NewReader(stdinData)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "[noman] command failed: %v\n", err)
		return err
	}
	return nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n... (truncated)"
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[noman] "+format+"\n", args...)
	os.Exit(1)
}

// API types

type anthropicRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
