# noman

> No manual needed — AI converts natural language into command-line arguments and runs them.

Stop reading man pages. Just tell `noman` what you want to do.

## Demo

![demo](./demo.svg)

```bash
$ echo '[{"title":"ABC news","count":1},{"title":"XYZ report","count":2}]' | noman jq "filter items where title contains XYZ"
[noman] jq .[] | select(.title | test("XYZ"))
{
  "title": "XYZ report",
  "count": 2
}
```

## Install

### From source (requires Go 1.22+)

```bash
go install github.com/azihsoyn/noman@latest
```

### Build manually

```bash
git clone https://github.com/azihsoyn/noman.git
cd noman
go build -o noman .
cp noman /usr/local/bin/
```

## Usage

```
noman [options] <command> "<prompt>"
noman which "<prompt>"
```

### Options

| Option | Description |
|---|---|
| `--no-cache` | Skip cache and always call AI |
| `--confirm`, `-c` | Show generated args and ask Y/n/r(retry) before executing |
| `--shell`, `-s` | Execute via shell (enables glob `*`, pipes, etc.) |
| `--debug` | Show generated args without executing |
| `--help`, `-h` | Show help |

### Subcommands

| Subcommand | Description |
|---|---|
| `which "<prompt>"` | AI picks the best command for the task |
| `man` | Show all past usage grouped by command |
| `man <command>` | Show detailed history for a specific command |

```bash
# Show your personal man pages for all commands
noman man

# Show history for a specific command
noman man jq
```

### Examples

```bash
# jq: filter JSON
cat data.json | noman jq "filter items where title contains XYZ"

# curl: fetch a URL
noman curl "fetch HTML from example.com"

# grep: search logs
cat log.txt | noman grep "extract lines that look like errors"

# awk: text processing
cat access.log | noman awk "count requests per status code"

# find: search files (everyone forgets the syntax)
noman find "log files older than 7 days"

# tar: always confusing flags
noman tar "compress src directory into backup.tar.gz"

# ffmpeg: the ultimate "just google it" command
noman ffmpeg "cut first 30 seconds of input.mp4 and convert to gif"

# imagemagick: another options nightmare
noman convert "resize photo.png to 800px width, save as JPEG quality 85"

# rsync: too many flags to remember
noman rsync "copy src to dst, exclude .git, dry run"

# docker: cleanup
noman docker "remove all stopped containers and dangling images"

# git: complex log queries
noman git "show only merge commits from last week"

# don't know which command? let AI pick
noman which "find all TODO comments in current directory"
cat access.log | noman which "count requests per status code"
noman which "disk usage of current directory, sorted by size"
```

## How it works

1. Reads the target command's `--help` / `man` page
2. If stdin is piped, samples the data to understand its structure
3. Sends everything to AI to generate the right arguments
4. Executes the command with the generated arguments

### History & caching

- **Exact match cache**: Same command + prompt + stdin reuses cached args instantly (no AI call)
- **Smart cache**: AI decides whether results should be cached (e.g. `jq` filters are cached, `git commit` messages are not)
- **Few-shot learning**: Past conversions for the same command are included as examples to improve accuracy
- History is stored in `~/.config/noman/history.json`
- Use `noman man` to browse your history as a personal man page

## Configuration

Settings are loaded in order of priority:

1. **Environment variables** (highest priority)
2. **Config file** (`~/.config/noman/config.toml` or `config.json`)
3. **Default values**

### Config file

Create `~/.config/noman/config.toml`:

```toml
backend      = "cli"
claude_path  = "/path/to/claude"
model        = "claude-sonnet-4-20250514"
max_history  = 500
```

Or `~/.config/noman/config.json`:

```json
{
  "backend": "cli",
  "claude_path": "/path/to/claude",
  "model": "claude-sonnet-4-20250514",
  "max_history": 500
}
```

### All settings

| Config key / Env var | Description | Default |
|---|---|---|
| `backend` / `NOMAN_BACKEND` | `cli` or `api` | `cli` |
| `claude_path` / `NOMAN_CLAUDE_PATH` | Path to `claude` command | auto-detect |
| `api_key` / `NOMAN_API_KEY` | API key for `api` backend | `ANTHROPIC_API_KEY` |
| `model` / `NOMAN_MODEL` | Model name | `claude-sonnet-4-20250514` |
| `base_url` / `NOMAN_BASE_URL` | API base URL | `https://api.anthropic.com` |
| `max_history` / `NOMAN_MAX_HISTORY` | Max history entries | `500` |
| — / `NOMAN_CONFIG_DIR` | Config/history directory | `~/.config/noman` |

## Backends

### Claude Code CLI (default)

Uses the `claude` command with your existing login. No API key needed.

```bash
noman jq "sum all counts"
```

### Anthropic API

Set via config file:

```toml
backend = "api"
api_key = "sk-ant-..."
```

Or environment variables:

```bash
export NOMAN_BACKEND=api
export ANTHROPIC_API_KEY=sk-ant-...
noman jq "sum all counts"
```

## License

MIT
