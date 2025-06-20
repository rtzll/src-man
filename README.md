# src-man

A fast, concurrent Git repository manager that auto-updates local repos and shows progress in real time.

## Motivation

I found myself with a growing collection of Git repos spread across different directories. I wanted a simple tool to keep them all up-to-date without having to manually run `git pull` for each one.

## Features

- **Concurrent Operations**: Pulls from multiple repositories simultaneously using configurable worker pools
- **Smart Discovery**: Automatically finds Git repositories in common directory structures:
  - `~/src/*` - Direct repositories
  - `~/src/*-src/*` - Source collections
  - `~/src/_*/*` - Special collections
- **Live Progress Display**: Real-time updates showing pull status and progress
- **Efficient Checking**: Only pulls repositories that have remote changes
- **Error Handling**: Graceful handling of timeouts, network issues, and Git errors

## Installation

```bash
go install github.com/rtzll/src-man@latest
```

## Demo

![CLI demo](assets/demo.gif)

### Example Output
```
$ src-man
✔ Up-to-date: 101/117
↑ Updated: sourcegraph/cody (1 new commits)
↑ Updated: github/github-mcp-server (1 new commits)
↑ Updated: golang/go (1 new commits)
↑ Updated: sst/opencode (1 new commits)
↑ Updated: temporalio/temporal (5 new commits)
↑ Updated: jj-vcs/jj (3 new commits)
↑ Updated: microsoft/typescript-go (5 new commits)
↑ Updated: gitbutlerapp/gitbutler (5 new commits)
↑ Updated: zed-industries/zed (9 new commits)
↑ Updated: tigerbeetle/tigerbeetle (2 new commits)
↑ Updated: better-auth/better-auth (1 new commits)
↑ Updated: ziglang/zig (37 new commits)
↑ Updated: LeaVerou/lea.verou.me (3 new commits)
↑ Updated: calcom/cal.com (3 new commits)
↑ Updated: the-pudding/website (2 new commits)

× Error: tylerneylon/pacpac → network error: cannot access remote
```

## Usage

```bash
src-man [flags]
```

### Commands

- `src-man`: Run the repository manager
- `src-man version`: Show version information

### Flags

- `-j, --jobs N`: Set maximum number of concurrent jobs (default: 2 × CPU cores)
- `-p, --path PATH`: Path to source directory (default: ~/src)
- `-v, --verbose`: Enable verbose output (logs to stderr)
- `-t, --timeout DURATION`: Timeout for git operations (default: 30s)

### Environment Variables

- `SRC_MAN_JOBS`: Set default number of concurrent jobs
- `SRC_MAN_PATH`: Set default source directory path
- `SRC_MAN_TIMEOUT`: Set default timeout for git operations

## How It Works

1. Scans `~/src` directory for Git repositories
2. For each repository:
   - Compares local HEAD with remote HEAD
   - Skips repositories that are already up-to-date
   - Pulls repositories with new commits using `git pull --ff-only --quiet`
3. Displays live progress with:
   - Count of up-to-date repositories
   - Currently pulling repositories
   - Successfully updated repositories with commit counts
   - Any errors encountered

## Repository Detection

The tool searches for repositories in these patterns:
- `~/src/*/` - Individual repositories
- `~/src/*-src/*/` - Organized by source
- `~/src/_*/` - Special collections

## Timeout and Safety

- Git operations timeout after 30 seconds (configurable with `--timeout` flag or `SRC_MAN_TIMEOUT` environment variable)
- Uses `--ff-only` flag to prevent merge commits
- Uses `--quiet` flag to minimize output noise
- Graceful handling of authentication failures and network issues
- Distinguishes between timeout errors and network connectivity issues
- Provides clear error messages with appropriate symbols (✗) in the UI
