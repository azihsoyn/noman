# noman

> No manual needed — AI converts natural language into command-line arguments and runs them.

Stop reading man pages. Just tell `noman` what you want to do.

## Demo

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
```

### Options

| Option | Description |
|---|---|
| `--no-cache` | Skip cache and always call AI |
| `--debug` | Show generated args without executing |
| `--help`, `-h` | Show help |

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
```

## How it works

1. Reads the target command's `--help` / `man` page
2. If stdin is piped, samples the data to understand its structure
3. Sends everything to AI to generate the right arguments
4. Executes the command with the generated arguments

### History & caching

- **Exact match cache**: Same command + prompt reuses cached args instantly (no AI call)
- **Few-shot learning**: Past conversions for the same command are included as examples to improve accuracy
- History is stored in `~/.config/noman/history.json`

## Backends

### Claude Code CLI (default)

Uses the `claude` command with your existing login. No API key needed.

```bash
noman jq "sum all counts"
```

### Anthropic API

```bash
export NOMAN_BACKEND=api
export ANTHROPIC_API_KEY=sk-ant-...
noman jq "sum all counts"
```

## Environment variables

| Variable | Description | Default |
|---|---|---|
| `NOMAN_BACKEND` | `cli` or `api` | `cli` |
| `NOMAN_CLAUDE_PATH` | Path to `claude` command | auto-detect |
| `NOMAN_API_KEY` | API key for `api` backend | `ANTHROPIC_API_KEY` |
| `NOMAN_MODEL` | Model name | `claude-sonnet-4-20250514` |
| `NOMAN_BASE_URL` | API base URL | `https://api.anthropic.com` |
| `NOMAN_CONFIG_DIR` | Config/history directory | `~/.config/noman` |

## License

MIT
