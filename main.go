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
  --no-cache       Skip cache and always call AI
  --confirm, -c    Show generated args and ask Y/n before executing
  --debug          Show generated args without executing
  --help, -h       Show this help

Subcommands:
  man [command]  Show past usage from history (like a personal man page)

Examples:
  cat data.json | noman jq "filter items where title contains XYZ"
  noman curl "fetch HTML from example.com"
  cat log.txt | noman grep "extract lines that look like errors"
  noman --no-cache jq "regenerate without cache"

Config file (~/.config/noman/config.toml or config.json):
  backend      = "cli"                    # "cli" or "api"
  claude_path  = ""                       # path to claude command
  api_key      = ""                       # API key for api backend
  model        = "claude-sonnet-4-20250514"
  base_url     = ""                       # API base URL
  max_history  = 500                      # max history entries

Environment variables (override config):
  NOMAN_BACKEND, NOMAN_CLAUDE_PATH, NOMAN_API_KEY, NOMAN_MODEL,
  NOMAN_BASE_URL, NOMAN_MAX_HISTORY, NOMAN_CONFIG_DIR
`

type options struct {
	noCache bool
	confirm bool
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
		case "--confirm", "-c":
			opts.confirm = true
		case "--debug":
			opts.debug = true
		case "--help", "-h":
			fmt.Fprint(os.Stderr, usage)
			os.Exit(0)
		default:
			rest = append(rest, a)
		}
	}

	if len(rest) < 1 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	// Handle "noman man [command]" subcommand
	if rest[0] == "man" {
		showMan(rest[1:])
		os.Exit(0)
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

	// Check if command exists
	if _, err := exec.LookPath(command); err != nil {
		fatal("command not found: %s", command)
	}

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
		if args, ok := history.FindExact(command, prompt, stdinHash(stdinData)); ok {
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

	// Load config
	cfg := loadConfig()

	// Get command help text
	helpText := getCommandHelp(command)

	// Get few-shot examples from history
	examples := history.FewShotExamples(command)

	// Generate args via AI (cancellable with Ctrl+C)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spin := NewSpinner(fmt.Sprintf("Generating args for %s...", command))
	spin.Start()
	result, err := generateArgs(ctx, cfg, command, prompt, helpText, stdinData, examples)
	spin.Stop()
	// Restore terminal state in case claude messed with it
	exec.Command("stty", "sane").Run()
	if err != nil {
		if strings.Contains(err.Error(), "cancelled") || strings.Contains(err.Error(), "signal: killed") {
			fmt.Fprintf(os.Stderr, "[noman] cancelled\n")
			os.Exit(130)
		}
		fatal("failed to generate args: %v", err)
	}

	// Save to history only if cacheable
	if result.cacheable {
		history.Add(command, prompt, result.args, stdinData)
		if err := history.save(); err != nil {
			fmt.Fprintf(os.Stderr, "[noman] warning: failed to save history: %v\n", err)
		}
	}

	// Print the generated command to stderr for visibility
	fmt.Fprintf(os.Stderr, "[noman] %s %s\n", command, strings.Join(result.args, " "))

	if opts.debug {
		return
	}

	if opts.confirm {
		if !askConfirm() {
			fmt.Fprintf(os.Stderr, "[noman] aborted\n")
			return
		}
	}

	// Execute the command
	if err := execute(command, result.args, stdinData); err != nil {
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

type aiResult struct {
	args      []string
	cacheable bool
}

// parseAIResponse extracts the CACHEABLE directive and args from AI output.
func parseAIResponse(text string) aiResult {
	text = strings.TrimSpace(text)
	cacheable := true

	if strings.HasPrefix(text, "CACHEABLE:") {
		idx := strings.IndexByte(text, '\n')
		if idx == -1 {
			// Only the directive, no args
			return aiResult{cacheable: strings.TrimPrefix(text, "CACHEABLE:") == "yes"}
		}
		directive := strings.TrimSpace(text[:idx])
		text = strings.TrimSpace(text[idx+1:])
		cacheable = directive == "CACHEABLE:yes"
	}

	return aiResult{args: parseArgs(text), cacheable: cacheable}
}

func generateArgs(ctx context.Context, cfg Config, command, prompt, helpText string, stdinData []byte, examples []HistoryEntry) (aiResult, error) {
	switch cfg.Backend {
	case "cli":
		return generateArgsCLI(ctx, cfg, command, prompt, helpText, stdinData, examples)
	case "api":
		return generateArgsAPI(ctx, cfg, command, prompt, helpText, stdinData, examples)
	default:
		return aiResult{}, fmt.Errorf("unknown backend: %s (use 'cli' or 'api')", cfg.Backend)
	}
}

func generateArgsCLI(ctx context.Context, cfg Config, command, prompt, helpText string, stdinData []byte, examples []HistoryEntry) (aiResult, error) {
	claudePath := cfg.ClaudePath
	if claudePath == "" {
		var err error
		claudePath, err = exec.LookPath("claude")
		if err != nil {
			return aiResult{}, fmt.Errorf("claude command not found. Set claude_path in config or NOMAN_CLAUDE_PATH, or use backend = \"api\"")
		}
	}

	systemPrompt := buildSystemPrompt(command, helpText, stdinData, examples)
	fullPrompt := systemPrompt + "\n\nUser request: " + prompt

	cmd := exec.CommandContext(ctx, claudePath, "-p", fullPrompt)
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
	cmd.Stdin = bytes.NewReader(nil)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return aiResult{}, fmt.Errorf("cancelled")
		}
		return aiResult{}, fmt.Errorf("claude command failed: %v\n%s", err, stderr.String())
	}

	text := strings.TrimSpace(stdout.String())
	return parseAIResponse(text), nil
}

func generateArgsAPI(ctx context.Context, cfg Config, command, prompt, helpText string, stdinData []byte, examples []HistoryEntry) (aiResult, error) {
	apiKey := cfg.APIKey
	if apiKey == "" {
		return aiResult{}, fmt.Errorf("set api_key in config, or ANTHROPIC_API_KEY / NOMAN_API_KEY environment variable")
	}

	model := cfg.Model

	baseURL := cfg.BaseURL
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
		return aiResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return aiResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return aiResult{}, fmt.Errorf("API request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return aiResult{}, err
	}

	if resp.StatusCode != 200 {
		return aiResult{}, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var apiResp anthropicResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return aiResult{}, fmt.Errorf("failed to parse response: %v", err)
	}

	if len(apiResp.Content) == 0 {
		return aiResult{}, fmt.Errorf("empty response from API")
	}

	text := strings.TrimSpace(apiResp.Content[0].Text)
	return parseAIResponse(text), nil
}

func buildSystemPrompt(command, helpText string, stdinData []byte, examples []HistoryEntry) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`You are a command-line argument generator. Your task is to convert a natural language description into command-line arguments for the "%s" command.

RULES:
- Output ONLY the arguments, nothing else. No explanation, no markdown.
- Do NOT include the command name itself.
- If stdin data is provided, consider its structure when generating arguments.
- Output one argument per line. If an argument contains spaces, wrap it in single quotes.
- The command will be run non-interactively. NEVER use options that open an editor or require interactive input. Use inline alternatives instead (e.g. "git commit -m 'message'" instead of "git commit").
- On the FIRST line, output either CACHEABLE:yes or CACHEABLE:no
  - CACHEABLE:yes if the same prompt would always produce the same args (e.g. filtering, searching, converting)
  - CACHEABLE:no if the args depend on runtime context like current time, git state, or generated content (e.g. commit messages, timestamps, dynamic values)
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

// parseArgs splits a string into arguments using shell-like tokenization.
// Supports single quotes, double quotes, and space/newline delimiters.
func parseArgs(text string) []string {
	var args []string
	var current strings.Builder
	inSingle := false
	inDouble := false

	for i := 0; i < len(text); i++ {
		ch := text[i]
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		case (ch == ' ' || ch == '\t' || ch == '\n') && !inSingle && !inDouble:
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(ch)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
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

func askConfirm() bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	defer tty.Close()

	fmt.Fprintf(os.Stderr, "[noman] execute? [Y/n] ")
	buf := make([]byte, 64)
	n, err := tty.Read(buf)
	if err != nil {
		return false
	}
	answer := strings.TrimSpace(strings.ToLower(string(buf[:n])))
	return answer == "" || answer == "y" || answer == "yes"
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
