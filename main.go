package main

import (
	"bufio"
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
       noman which "<prompt>"

Options:
  --no-cache       Skip cache and always call AI
  --confirm, -c    Show generated args and ask Y/n before executing
  --shell, -s      Execute via shell (enables glob, pipes, etc.)
  --debug          Show generated args without executing
  --help, -h       Show this help

Subcommands:
  which "<prompt>"   AI picks the best command for you
  man [command]      Show past usage from history (like a personal man page)
  noman "<prompt>"   Ask how to use noman itself

Examples:
  cat data.json | noman jq "filter items where title contains XYZ"
  noman curl "fetch HTML from example.com"
  cat log.txt | noman grep "extract lines that look like errors"
  noman --no-cache jq "regenerate without cache"
  noman which "find all TODO comments in current directory"
  cat access.log | noman which "count requests per status code"

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
	shell   bool
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
		case "--shell", "-s":
			opts.shell = true
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

	// Handle subcommands
	if rest[0] == "man" {
		showMan(rest[1:])
		os.Exit(0)
	}

	if rest[0] == "noman" {
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: noman noman \"<prompt>\"")
			os.Exit(1)
		}
		cfg := loadConfig()
		question := strings.Join(rest[1:], " ")
		handleNoman(cfg, question)
		os.Exit(0)
	}

	if rest[0] == "which" {
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: noman which \"<prompt>\"")
			os.Exit(1)
		}
		opts.prompt = strings.Join(rest[1:], " ")
		// command remains "" to signal auto-command mode
		return opts
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

	// Load config
	cfg := loadConfig()

	// Auto-command mode: AI picks the command
	if command == "" {
		history := loadHistory()

		// Check cache first
		if !opts.noCache {
			if cachedCmd, cachedArgs, ok := history.FindByPrompt(prompt, stdinHash(stdinData)); ok {
				fmt.Fprintf(os.Stderr, "[noman] (cached) %s %s\n", cachedCmd, strings.Join(cachedArgs, " "))
				if opts.debug {
					return
				}
				if opts.confirm {
					switch askConfirm() {
					case confirmYes:
					case confirmNo:
						fmt.Fprintf(os.Stderr, "[noman] aborted\n")
						return
					case confirmRetry:
						goto whichGenerate
					}
				}
				if err := execute(cachedCmd, cachedArgs, stdinData, opts.shell); err != nil {
					os.Exit(1)
				}
				_ = history.save()
				return
			}
		}
	whichGenerate:

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		spin := NewSpinner("Figuring out the right command...")
		spin.Start()
		cmdResult, err := generateCommandAndArgs(ctx, cfg, prompt, stdinData)
		spin.Stop()
		restoreTerminal()
		if err != nil {
			if strings.Contains(err.Error(), "cancelled") || strings.Contains(err.Error(), "signal: killed") {
				fmt.Fprintf(os.Stderr, "[noman] cancelled\n")
				os.Exit(130)
			}
			fatal("failed to generate command: %v", err)
		}
		command = cmdResult.command

		// Save to history
		if cmdResult.cacheable {
			history.Add(command, prompt, cmdResult.args, stdinData)
			_ = history.save()
		}

		for {
			fmt.Fprintf(os.Stderr, "[noman] %s %s\n", command, strings.Join(cmdResult.args, " "))

			if opts.debug {
				return
			}

			if opts.confirm {
				switch askConfirm() {
				case confirmYes:
					// proceed
				case confirmNo:
					fmt.Fprintf(os.Stderr, "[noman] aborted\n")
					return
				case confirmRetry:
					fmt.Fprintf(os.Stderr, "[noman] regenerating...\n")
					spin := NewSpinner("Figuring out the right command...")
					spin.Start()
					cmdResult, err = generateCommandAndArgs(ctx, cfg, prompt, stdinData)
					spin.Stop()
					restoreTerminal()
					if err != nil {
						fatal("failed to generate command: %v", err)
					}
					command = cmdResult.command
					continue
				}
			}

			if err := execute(command, cmdResult.args, stdinData, opts.shell); err != nil {
				os.Exit(1)
			}
			return
		}
	}

	// Check if command exists (supports paths like ./tool and /usr/local/bin/tool)
	if strings.Contains(command, "/") {
		if _, err := os.Stat(command); err != nil {
			fatal("command not found: %s", command)
		}
	} else {
		if _, err := exec.LookPath(command); err != nil {
			fatal("command not found: %s", command)
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
			if opts.confirm {
				switch askConfirm() {
				case confirmYes:
				case confirmNo:
					fmt.Fprintf(os.Stderr, "[noman] aborted\n")
					return
				case confirmRetry:
					goto generate
				}
			}
			if err := execute(command, args, stdinData, opts.shell); err != nil {
				os.Exit(1)
			}
			_ = history.save()
			return
		}
	}

generate:
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
	restoreTerminal()
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

	for {
		// Print the generated command to stderr for visibility
		fmt.Fprintf(os.Stderr, "[noman] %s %s\n", command, strings.Join(result.args, " "))

		if opts.debug {
			return
		}

		if opts.confirm {
			switch askConfirm() {
			case confirmYes:
				// proceed to execute
			case confirmNo:
				fmt.Fprintf(os.Stderr, "[noman] aborted\n")
				return
			case confirmRetry:
				fmt.Fprintf(os.Stderr, "[noman] regenerating...\n")
				spin := NewSpinner(fmt.Sprintf("Generating args for %s...", command))
				spin.Start()
				result, err = generateArgs(ctx, cfg, command, prompt, helpText, stdinData, examples)
				spin.Stop()
				restoreTerminal()
				if err != nil {
					fatal("failed to generate args: %v", err)
				}
				continue
			}
		}

		// Execute the command
		if err := execute(command, result.args, stdinData, opts.shell); err != nil {
			os.Exit(1)
		}
		return
	}
}

func handleNoman(cfg Config, question string) {
	systemPrompt := fmt.Sprintf(`You are the help assistant for "noman", a CLI tool that converts natural language into command-line arguments using AI.

Here is noman's full usage information:
%s

Answer the user's question about how to use noman. Be concise and practical. Show example commands when helpful. Reply in the same language as the user's question.`, usage)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spin := NewSpinner("Thinking...")
	spin.Start()
	answer, err := askAI(ctx, cfg, systemPrompt, question)
	spin.Stop()
	restoreTerminal()
	if err != nil {
		if strings.Contains(err.Error(), "cancelled") || strings.Contains(err.Error(), "signal: killed") {
			fmt.Fprintf(os.Stderr, "[noman] cancelled\n")
			os.Exit(130)
		}
		fatal("failed: %v", err)
	}
	fmt.Println(answer)
}

func askAI(ctx context.Context, cfg Config, systemPrompt, userPrompt string) (string, error) {
	switch cfg.Backend {
	case "cli":
		return askAICLI(ctx, cfg, systemPrompt, userPrompt)
	case "api":
		return askAIAPI(ctx, cfg, systemPrompt, userPrompt)
	default:
		return "", fmt.Errorf("unknown backend: %s", cfg.Backend)
	}
}

func askAICLI(ctx context.Context, cfg Config, systemPrompt, userPrompt string) (string, error) {
	claudePath := cfg.ClaudePath
	if claudePath == "" {
		var err error
		claudePath, err = exec.LookPath("claude")
		if err != nil {
			return "", fmt.Errorf("claude command not found")
		}
	}

	fullPrompt := systemPrompt + "\n\nUser question: " + userPrompt
	cmd := exec.CommandContext(ctx, claudePath, "-p", fullPrompt)
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
			return "", fmt.Errorf("cancelled")
		}
		return "", fmt.Errorf("claude command failed: %v\n%s", err, stderr.String())
	}

	return strings.TrimSpace(stdout.String()), nil
}

func askAIAPI(ctx context.Context, cfg Config, systemPrompt, userPrompt string) (string, error) {
	apiKey := cfg.APIKey
	if apiKey == "" {
		return "", fmt.Errorf("set api_key in config, or ANTHROPIC_API_KEY / NOMAN_API_KEY environment variable")
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	reqBody := anthropicRequest{
		Model:     cfg.Model,
		MaxTokens: 1024,
		System:    systemPrompt,
		Messages:  []message{{Role: "user", Content: userPrompt}},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("API request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var apiResp anthropicResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return "", fmt.Errorf("failed to parse response: %v", err)
	}

	if len(apiResp.Content) == 0 {
		return "", fmt.Errorf("empty response from API")
	}

	return strings.TrimSpace(apiResp.Content[0].Text), nil
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
			return aiResult{cacheable: strings.TrimPrefix(text, "CACHEABLE:") == "yes"}
		}
		directive := strings.TrimSpace(text[:idx])
		text = strings.TrimSpace(text[idx+1:])
		cacheable = directive == "CACHEABLE:yes"
	}

	// Filter out commentary lines
	var cleanLines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if looksLikeCommentary(line) {
			break
		}
		cleanLines = append(cleanLines, line)
	}

	return aiResult{args: parseArgs(strings.Join(cleanLines, "\n")), cacheable: cacheable}
}

type commandResult struct {
	command   string
	args      []string
	cacheable bool
}

// parseCommandResponse extracts command, args, and CACHEABLE from AI output.
// Expected format:
//
//	CACHEABLE:yes
//	COMMAND:grep
//	-r
//	TODO
//	.
func parseCommandResponse(text string) (commandResult, error) {
	text = strings.TrimSpace(text)
	cacheable := true
	command := ""

	lines := strings.Split(text, "\n")
	var argLines []string
	foundCommand := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "CACHEABLE:") {
			cacheable = strings.TrimPrefix(line, "CACHEABLE:") == "yes"
			continue
		}
		if strings.HasPrefix(line, "COMMAND:") {
			command = strings.TrimSpace(strings.TrimPrefix(line, "COMMAND:"))
			foundCommand = true
			continue
		}
		if !foundCommand {
			continue // skip anything before COMMAND: directive
		}
		if line == "" {
			continue
		}
		// Stop at lines that look like commentary (contain multiple spaces + common English words)
		if looksLikeCommentary(line) {
			break
		}
		argLines = append(argLines, line)
	}

	if command == "" {
		return commandResult{}, fmt.Errorf("AI did not return a COMMAND: directive")
	}

	argsText := strings.Join(argLines, "\n")
	return commandResult{
		command:   command,
		args:      parseArgs(argsText),
		cacheable: cacheable,
	}, nil
}

func generateCommandAndArgs(ctx context.Context, cfg Config, prompt string, stdinData []byte) (commandResult, error) {
	systemPrompt := buildAutoCommandPrompt(stdinData)
	switch cfg.Backend {
	case "cli":
		return generateCommandCLI(ctx, cfg, systemPrompt, prompt)
	case "api":
		return generateCommandAPI(ctx, cfg, systemPrompt, prompt)
	default:
		return commandResult{}, fmt.Errorf("unknown backend: %s", cfg.Backend)
	}
}

func generateCommandCLI(ctx context.Context, cfg Config, systemPrompt, prompt string) (commandResult, error) {
	claudePath := cfg.ClaudePath
	if claudePath == "" {
		var err error
		claudePath, err = exec.LookPath("claude")
		if err != nil {
			return commandResult{}, fmt.Errorf("claude command not found")
		}
	}

	fullPrompt := systemPrompt + "\n\nUser request: " + prompt
	cmd := exec.CommandContext(ctx, claudePath, "-p", fullPrompt)
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
			return commandResult{}, fmt.Errorf("cancelled")
		}
		return commandResult{}, fmt.Errorf("claude command failed: %v\n%s", err, stderr.String())
	}

	return parseCommandResponse(stdout.String())
}

func generateCommandAPI(ctx context.Context, cfg Config, systemPrompt, prompt string) (commandResult, error) {
	apiKey := cfg.APIKey
	if apiKey == "" {
		return commandResult{}, fmt.Errorf("set api_key in config, or ANTHROPIC_API_KEY / NOMAN_API_KEY environment variable")
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	reqBody := anthropicRequest{
		Model:     cfg.Model,
		MaxTokens: 1024,
		System:    systemPrompt,
		Messages:  []message{{Role: "user", Content: prompt}},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return commandResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return commandResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return commandResult{}, fmt.Errorf("API request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return commandResult{}, err
	}

	if resp.StatusCode != 200 {
		return commandResult{}, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var apiResp anthropicResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return commandResult{}, fmt.Errorf("failed to parse response: %v", err)
	}

	if len(apiResp.Content) == 0 {
		return commandResult{}, fmt.Errorf("empty response from API")
	}

	return parseCommandResponse(apiResp.Content[0].Text)
}

func buildAutoCommandPrompt(stdinData []byte) string {
	var sb strings.Builder
	sb.WriteString(`You are a command-line argument generator. The user describes what they want to do, and you determine the best Unix command and its arguments.

OUTPUT FORMAT (strictly follow this, no exceptions):
Line 1: CACHEABLE:yes or CACHEABLE:no
Line 2: COMMAND:<command_name>
Line 3+: one argument per line

EXAMPLE OUTPUT:
CACHEABLE:yes
COMMAND:grep
-r
TODO
.

RULES:
- Output ONLY the lines described above. ABSOLUTELY NO explanation, no commentary, no markdown, no thinking.
- If an argument contains spaces, wrap it in single quotes.
- Choose a SINGLE command. Do NOT suggest pipes or multiple commands.
- If the ideal solution requires pipes, pick the most important command and output only its args.
- The command will be run non-interactively. NEVER use options that require interactive input.
- Only use commands commonly available on macOS/Linux.
- Do NOT second-guess yourself or correct yourself. Output your best answer once.
`)

	if len(stdinData) > 0 {
		sample := truncate(string(stdinData), 2000)
		sb.WriteString(fmt.Sprintf("\nSample of stdin data:\n```\n%s\n```\n", sample))
	}

	return sb.String()
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
- Output ONLY the arguments for a SINGLE invocation of the command, nothing else. No explanation, no markdown.
- Do NOT include the command name itself.
- Do NOT chain multiple commands. Only output args for one command invocation.
- If the task seems to require multiple commands (e.g. "git add and commit"), focus on the most important one (e.g. just the commit with -am flag).
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

func execute(command string, args []string, stdinData []byte, shell bool) error {
	var cmd *exec.Cmd
	if shell {
		// Build a single command string for sh -c
		// Reconstruct: command arg1 arg2 ...
		parts := make([]string, 0, len(args)+1)
		parts = append(parts, command)
		parts = append(parts, args...)
		cmd = exec.Command("sh", "-c", strings.Join(parts, " "))
	} else {
		cmd = exec.Command(command, args...)
	}
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

// looksLikeCommentary detects lines that are explanatory text rather than args.
// e.g. "Wait, `du` doesn't have --sort." or "This needs a pipe..."
func looksLikeCommentary(line string) bool {
	// Lines that start with typical argument characters are never commentary
	trimmed := strings.TrimSpace(line)
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '-', '.', '/', '\'', '"', '[', '{', '(', '*', '~', '$', '|', '<', '>':
		return false
	}

	// Check for known commentary markers
	lower := strings.ToLower(line)
	for _, marker := range []string{"wait", "note:", "let me", "this ", "however", "instead", "actually", "sorry", "correction", "i ", "the "} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	// Long lines without argument-like characters are likely commentary
	words := strings.Fields(line)
	if len(words) >= 8 {
		return true
	}
	return false
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n... (truncated)"
}

type confirmResult int

const (
	confirmYes confirmResult = iota
	confirmNo
	confirmRetry
)

func restoreTerminal() {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer tty.Close()
	cmd := exec.Command("stty", "sane", "echo", "icanon", "erase", "^?")
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	cmd.Run()
}

func askConfirm() confirmResult {
	restoreTerminal()

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return confirmNo
	}
	defer tty.Close()

	fmt.Fprintf(tty, "[noman] execute? [Y/n/r(retry)] ")
	scanner := bufio.NewScanner(tty)
	if !scanner.Scan() {
		return confirmNo
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	switch answer {
	case "", "y", "yes":
		return confirmYes
	case "r", "retry":
		return confirmRetry
	default:
		return confirmNo
	}
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
