// Package main implements a source code repository manager using git.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	uiSeparator    = "──────────────────────────────────"
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
	version = "0.1.1"
)

var (
	pullArgs       = strings.Split(gitPullOptions, " ")
	defaultMaxJobs = runtime.NumCPU() * 2

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

	// Header
	fmt.Println()
	fmt.Println(uiSeparator)
	fmt.Printf(" Found %d git repos\n", len(repos))
	fmt.Println(uiSeparator)

	events := make(chan event)
	var wg sync.WaitGroup

	// Start renderer and track it so we can wait for it
	var renderWg sync.WaitGroup
	renderWg.Add(1)
	go func() {
		defer renderWg.Done()
		renderLoop(ctx, events)
	}()

	// Dispatch workers using worker pool pattern
	jobs := make(chan string, len(repos))

	// Start worker goroutines
	for range maxJobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for repo := range jobs {
				processRepo(ctx, repo, events)
			}
		}()
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

	repoName := getRepositoryName(gitCtx, path)

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
			msg:       "local error: " + err.Error(),
		}
		return
	}

	remoteHeadOutput, err := runGitCommand(gitCtx, path, "ls-remote", "origin", "HEAD")
	if err != nil {
		if verbose {
			log.Printf("Error getting remote HEAD for %s: %v", repoName, err)
		}
		// Check if the error is a timeout
		errMsg := "network error: cannot access remote"
		if errors.Is(err, context.DeadlineExceeded) {
			errMsg = "timeout: remote operation took too long"
		}
		// Report network errors to the user
		events <- event{
			eventType: Error,
			repoName:  repoName,
			msg:       errMsg,
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

	cmd := exec.CommandContext(gitCtx, "git", append([]string{"-C", path, "pull"}, pullArgs...)...)
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

func getRepositoryName(ctx context.Context, path string) string {
	// Using the already timeout-limited context passed from processRepo
	remoteUrl, err := runGitCommand(ctx, path, "config", "--get", "remote.origin.url")
	if err != nil {
		if verbose {
			log.Printf("Error getting remote URL for %s: %v", path, err)
		}
		return filepath.Base(path)
	}
	remoteUrl = strings.TrimSuffix(remoteUrl, ".git")

	// Handle SSH URLs (git@github.com:org/repo)
	if strings.HasPrefix(remoteUrl, "git@") {
		parts := strings.Split(remoteUrl, ":")
		if len(parts) == 2 {
			return parts[1]
		}
	}

	// Handle HTTP/HTTPS URLs
	parsedURL, err := url.Parse(remoteUrl)
	if err == nil && parsedURL.Host != "" {
		// Remove leading slash and return the path
		path := strings.TrimPrefix(parsedURL.Path, "/")
		if path != "" {
			return path
		}
	}

	// fallback to original logic for any other format
	if slash := strings.Index(remoteUrl, "/"); slash != -1 {
		return remoteUrl[slash+1:]
	}
	return remoteUrl
}

// extractErrorMessage returns the first non-blank line, stripping any leading "hint:"
func extractErrorMessage(raw string) string {
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// strip leading "hint:" if present
		if strings.HasPrefix(line, "hint:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "hint:"))
		}
		return line
	}
	return "pull failed"
}

func runGitCommand(ctx context.Context, path string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", path}, args...)...)
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

func renderLoop(ctx context.Context, events <-chan event) {
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
		fmt.Fprintf(writer, "%s Up-to-date: %d\n", upToDateSymbol, upToDateCount)

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
