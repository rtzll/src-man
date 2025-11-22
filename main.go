// Package main implements a source code repository manager using git.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gosuri/uilive"
	"github.com/spf13/cobra"
)

const (
	gitPullOptions = "--ff-only --quiet"
	defaultTimeout = 30 * time.Second

	// Environment variables
	envJobsKey    = "SRC_MAN_JOBS"
	envSourcePath = "SRC_MAN_PATH"
	envTimeout    = "SRC_MAN_TIMEOUT"

	// UI symbols
	upToDateSymbol = "✔"
	pullingSymbol  = "↻"
	errorSymbol    = "✗"
	updatedSymbol  = "↑"

	// Version information
	version = "0.3.0"
)

var (
	pullArgs       = strings.Split(gitPullOptions, " ")
	defaultMaxJobs = runtime.NumCPU()

	// Command line flags
	maxJobs    int
	sourcePath string
	verbose    bool
	timeout    time.Duration
)

// EventType represents the type of event in the repository update system.
type EventType int

// Event types for the repository update system.
const (
	UpToDate EventType = iota // Repository is already up-to-date
	Pulling                   // Repository is being pulled
	Updated                   // Repository was updated
	Error                     // An error occurred
)

// event represents a state change in the repository update system.
type event struct {
	eventType   EventType // Type of event (UpToDate, Pulling, Updated, Error)
	repoName    string    // Repository name in format "org/name"
	commitCount int       // Number of new commits (for Updated events)
	msg         string    // Error message (for Error events)
}

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "src-man",
	Short: "A tool to manage and update git repositories",
	Long: `src-man is a tool that finds and updates git repositories in your source directory.
It concurrently pulls updates for multiple repositories and provides a clean UI
to track progress.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSrcMan()
	},
	SilenceErrors: true,
	SilenceUsage:  true,
}

// versionCmd represents the version command
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("src-man version %s\n", version)
	},
}

func init() {
	// Get default source path
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("cannot find HOME: %v", err)
	}
	defaultSourcePath := filepath.Join(home, "src")

	// Check environment variables for overrides
	if envPath := os.Getenv(envSourcePath); envPath != "" {
		defaultSourcePath = envPath
	}

	// Add persistent flags to the root command
	rootCmd.PersistentFlags().IntVarP(&maxJobs, "jobs", "j", getEnvInt(envJobsKey, defaultMaxJobs), "Maximum number of concurrent jobs")
	rootCmd.PersistentFlags().StringVarP(&sourcePath, "path", "p", defaultSourcePath, "Path to source directory")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")
	rootCmd.PersistentFlags().DurationVarP(&timeout, "timeout", "t", getEnvDuration(envTimeout, defaultTimeout), "Timeout for git operations")

	// Add subcommands
	rootCmd.AddCommand(versionCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runSrcMan implements the main functionality of the src-man tool.
func runSrcMan() error {
	// Configure logging to use stderr when in verbose mode
	if verbose {
		log.SetOutput(os.Stderr)
	}

	// Validate flags
	if maxJobs < 1 {
		return fmt.Errorf("jobs must be at least 1, got %d", maxJobs)
	}

	// Validate source path
	if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
		return fmt.Errorf("source path does not exist: %s", sourcePath)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	repos, err := findRepos(sourcePath)
	if err != nil {
		return fmt.Errorf("finding repos: %w", err)
	}

	events := make(chan event)
	var wg sync.WaitGroup

	// Start renderer and track it so we can wait for it
	var renderWg sync.WaitGroup
	renderWg.Go(func() {
		renderLoop(ctx, events, len(repos))
	})

	// Dispatch workers using worker pool pattern
	jobs := make(chan string, len(repos))

	// Start worker goroutines
	for range maxJobs {
		wg.Go(func() {
			for repo := range jobs {
				processRepo(ctx, repo, events)
			}
		})
	}

	// Send all repos to the worker pool
	for _, repo := range repos {
		jobs <- repo
	}
	close(jobs)

	// Wait for all pulls to finish, then close events
	wg.Wait()
	close(events)

	// Wait for renderer to finish printing errors
	renderWg.Wait()
	return nil
}

// findRepos does the three globs and strips "/.git"
func findRepos(base string) ([]string, error) {
	globPatterns := []string{
		filepath.Join(base, "*", ".git"),
		filepath.Join(base, "*-src", "*", ".git"),
		filepath.Join(base, "_*", "*", ".git"),
	}

	if verbose {
		log.Printf("Searching for repositories in patterns: %v", globPatterns)
	}

	var out []string
	for _, pattern := range globPatterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("glob pattern %s failed: %w", pattern, err)
		}
		for _, g := range matches {
			out = append(out, filepath.Dir(g))
		}
	}

	if verbose {
		log.Printf("Found %d repositories", len(out))
	}

	return out, nil
}

// getEnvInt retrieves an integer from environment variable with a fallback value
func getEnvInt(key string, fallback int) int {
	if val, ok := os.LookupEnv(key); ok {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return fallback
}

// getEnvDuration retrieves a duration from environment variable with a fallback value
func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if val, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
	}
	return fallback
}

func processRepo(ctx context.Context, path string, events chan<- event) {
	// Create a single context with timeout for all git operations in this function
	gitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	remoteURL := getRemoteURL(gitCtx, path)
	repoName := getRepositoryName(remoteURL, path)

	if verbose {
		log.Printf("Processing repository: %s (%s)", repoName, path)
	}

	localSHA, err := runGitCommand(gitCtx, path, "rev-parse", "HEAD")
	if err != nil {
		if verbose {
			log.Printf("Error getting local HEAD for %s: %v", repoName, err)
		}
		// Report local HEAD errors to the user
		events <- event{
			eventType: Error,
			repoName:  repoName,
			msg:       "local error: " + summarizeGitError(err),
		}
		return
	}

	remoteHeadOutput, err := runGitCommand(gitCtx, path, "ls-remote", "origin", "HEAD")
	if err != nil {
		if verbose {
			log.Printf("Error getting remote HEAD for %s: %v", repoName, err)
		}
		msg := summarizeGitError(err)
		if diag := diagnoseRemoteAccess(remoteURL); diag != "" {
			msg = diag
		}
		// Report remote errors to the user with the git-provided context
		events <- event{
			eventType: Error,
			repoName:  repoName,
			msg:       msg,
		}
		return
	}

	// Parse remote SHA
	parts := strings.Fields(remoteHeadOutput)
	if len(parts) == 0 {
		if verbose {
			log.Printf("Invalid ls-remote output for %s: %s", repoName, remoteHeadOutput)
		}
		// Report invalid remote output as an error
		events <- event{
			eventType: Error,
			repoName:  repoName,
			msg:       "invalid remote response",
		}
		return
	}
	remoteSHA := parts[0]

	if localSHA == remoteSHA {
		if verbose {
			log.Printf("Repository %s is already up-to-date", repoName)
		}
		events <- event{eventType: UpToDate, repoName: repoName}
		return
	}

	// Start pulling
	events <- event{eventType: Pulling, repoName: repoName}
	if verbose {
		log.Printf("Pulling updates for %s", repoName)
	}

	cmd := gitCommand(gitCtx, path, append([]string{"pull"}, pullArgs...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		errMsg := extractErrorMessage(string(out))
		if verbose {
			log.Printf("Pull failed for %s: %s", repoName, errMsg)
		}
		events <- event{
			eventType: Error,
			repoName:  repoName,
			msg:       errMsg,
		}
		return
	}

	newSHA, err := runGitCommand(gitCtx, path, "rev-parse", "HEAD")
	if err != nil {
		if verbose {
			log.Printf("Error getting new HEAD after pull for %s: %v", repoName, err)
		}
		events <- event{eventType: Error, repoName: repoName, msg: "post-pull rev-parse failed"}
		return
	}

	commitCountStr, err := runGitCommand(gitCtx, path, "rev-list", "--count", fmt.Sprintf("%s..%s", localSHA, newSHA))
	if err != nil {
		if verbose {
			log.Printf("Error counting commits for %s: %v", repoName, err)
		}
		events <- event{eventType: Error, repoName: repoName, msg: "commit count failed"}
		return
	}

	// Parse commit count
	commitCount, err := strconv.Atoi(commitCountStr)
	if err != nil {
		if verbose {
			log.Printf("Error parsing commit count for %s: %v", repoName, err)
		}
		events <- event{eventType: Error, repoName: repoName, msg: "failed to parse commit count"}
		return
	}

	if verbose {
		log.Printf("Successfully pulled %d new commits for %s", commitCount, repoName)
	}

	events <- event{eventType: Updated, repoName: repoName, commitCount: commitCount}
}

func getRepositoryName(remoteURL, path string) string {
	remoteURL = strings.TrimSuffix(remoteURL, ".git")

	// Handle SSH URLs (git@github.com:org/repo)
	if trimmed, ok := strings.CutPrefix(remoteURL, "git@"); ok {
		parts := strings.Split(trimmed, ":")
		if len(parts) == 2 {
			return parts[1]
		}
	}

	// Handle HTTP/HTTPS URLs
	parsedURL, err := url.Parse(remoteURL)
	if err == nil && parsedURL.Host != "" {
		// Remove leading slash and return the path
		path := strings.TrimPrefix(parsedURL.Path, "/")
		if path != "" {
			return path
		}
	}

	// fallback to original logic for any other format
	if slash := strings.Index(remoteURL, "/"); slash != -1 {
		return remoteURL[slash+1:]
	}
	if remoteURL != "" {
		return remoteURL
	}
	return filepath.Base(path)
}

func getRemoteURL(ctx context.Context, path string) string {
	remoteURL, err := runGitCommand(ctx, path, "config", "--get", "remote.origin.url")
	if err != nil {
		if verbose {
			log.Printf("Error getting remote URL for %s: %v", path, err)
		}
		return ""
	}
	return remoteURL
}

// extractErrorMessage returns the first non-blank line, stripping any leading "hint:"
func extractErrorMessage(raw string) string {
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// strip leading "hint:" if present
		if trimmed, ok := strings.CutPrefix(line, "hint:"); ok {
			line = strings.TrimSpace(trimmed)
		}
		return line
	}
	return "pull failed"
}

// gitCommand returns an exec.Cmd prepared to run git with the configured
// environment. GIT_TERMINAL_PROMPT=0 disables interactive auth prompts so we
// can fail fast and report which repository needs credentials. We also clear
// credential helpers so macOS Keychain or other helpers cannot pop dialogs.
func gitCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	baseArgs := []string{"-c", "credential.helper=", "-C", path}
	cmd := exec.CommandContext(ctx, "git", append(baseArgs, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/true")
	return cmd
}

func runGitCommand(ctx context.Context, path string, args ...string) (string, error) {
	cmd := gitCommand(ctx, path, args...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		errMsg := ""
		if errors.As(err, &exitErr) {
			errMsg = string(exitErr.Stderr)
		}
		return "", fmt.Errorf("git %s failed: %s: %w", strings.Join(args, " "), errMsg, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// summarizeGitError extracts the most useful part of a git error, including
// stderr when available and handling timeouts distinctly.
func summarizeGitError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout: remote operation took too long"
	}

	if stderr := gitStderr(err); stderr != "" {
		if msg := extractErrorMessage(stderr); msg != "" {
			return msg
		}
	}

	if msg := extractErrorMessage(err.Error()); msg != "" {
		return msg
	}

	return "git command failed"
}

func gitStderr(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return string(exitErr.Stderr)
	}
	return ""
}

// diagnoseRemoteAccess performs a lightweight HTTP HEAD to identify common
// remote availability issues without using authenticated API calls.
func diagnoseRemoteAccess(remoteURL string) string {
	httpURL := toHTTPURL(remoteURL)
	if httpURL == "" {
		return ""
	}

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodHead, httpURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "src-man")

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnavailableForLegalReasons: // 451
		return "remote unavailable (DMCA takedown)"
	case http.StatusNotFound: // 404
		return "remote not found or made private"
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return fmt.Sprintf("remote moved/renamed (HTTP %d)", resp.StatusCode)
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication required for remote"
	case http.StatusTooManyRequests:
		return "remote rate limited (HTTP 429)"
	case http.StatusOK:
		return "remote reachable; git authentication failed"
	default:
		if resp.StatusCode >= 500 {
			return fmt.Sprintf("remote server error (HTTP %d)", resp.StatusCode)
		}
	}

	return ""
}

// toHTTPURL converts common git remote formats into an HTTPS URL for probing.
func toHTTPURL(remoteURL string) string {
	if remoteURL == "" {
		return ""
	}

	remoteURL = strings.TrimSuffix(remoteURL, ".git")

	if trimmed, ok := strings.CutPrefix(remoteURL, "git@"); ok {
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) == 2 {
			host := parts[0]
			path := strings.TrimPrefix(parts[1], "/")
			return fmt.Sprintf("https://%s/%s", host, path)
		}
	}

	parsed, err := url.Parse(remoteURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	parsed.Scheme = "https"
	return parsed.String()
}

func renderLoop(ctx context.Context, events <-chan event, totalRepos int) {
	writer := uilive.New()
	// Write to stdout even if verbose mode is redirecting logs to stderr
	writer.Out = os.Stdout
	// Set refresh interval to reduce UI flicker
	writer.RefreshInterval = time.Millisecond * 100
	writer.Start()
	defer writer.Stop()

	var (
		upToDateCount int
		inProgress    []string
		updatedLines  []string
		errorLines    []string
		allDone       bool
	)

	// Helper to remove a repo from inProgress list
	removeFromInProgress := func(repoName string) {
		for i, repo := range inProgress {
			if repo == repoName {
				inProgress = slices.Delete(inProgress, i, i+1)
				break
			}
		}
	}

	// Update the display
	redraw := func() {
		fmt.Fprintf(writer, "%s Up-to-date: %d/%d\n", upToDateSymbol, upToDateCount, totalRepos)

		// Only show the Pulling line if we're not completely done
		if !allDone {
			if len(inProgress) > 0 {
				fmt.Fprintf(writer, "%s Pulling(%d): %s\n", pullingSymbol, len(inProgress), strings.Join(inProgress, ", "))
			} else {
				fmt.Fprintf(writer, "%s Pulling(0)\n", pullingSymbol)
			}
		}

		for _, l := range updatedLines {
			fmt.Fprintln(writer, l)
		}

		if err := writer.Flush(); err != nil {
			log.Printf("error flushing output: %v", err)
			return
		}
	}

	// Process events until channel closes or context is canceled
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				// Channel closed, all operations are complete
				allDone = true
				redraw() // Update display one last time to remove Pulling line
				goto done
			}

			// Process event
			switch ev.eventType {
			case UpToDate:
				upToDateCount++

			case Pulling:
				inProgress = append(inProgress, ev.repoName)

			case Updated:
				removeFromInProgress(ev.repoName)
				updatedLines = append(updatedLines,
					fmt.Sprintf("%s Updated: %s (%d new commits)", updatedSymbol, ev.repoName, ev.commitCount))

			case Error:
				removeFromInProgress(ev.repoName)
				errorLines = append(errorLines,
					fmt.Sprintf("%s Error: %s → %s", errorSymbol, ev.repoName, ev.msg))
			}
			redraw()

		case <-ctx.Done():
			// Context was canceled (e.g., by Ctrl+C)
			log.Println("\nRender loop canceled")
			goto done
		}
	}

	// Print any errors once before exiting
done:
	if len(errorLines) > 0 {
		fmt.Println()
		for _, l := range errorLines {
			fmt.Println(l)
		}
	}
}
