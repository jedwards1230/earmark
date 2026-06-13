package version

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
	GoVersion = "unknown"
)

const (
	DefaultGitHubAPI = "https://api.github.com"
	GitHubRepo       = "jedwards1230/earmark"
	CacheFile        = ".version_cache"
	DefaultExpiry    = 24 * time.Hour
)

var (
	GitHubAPI = DefaultGitHubAPI
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
}

type GitHubCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author struct {
			Date time.Time `json:"date"`
		} `json:"author"`
		Message string `json:"message"`
	} `json:"commit"`
}

type GitHubRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	PublishedAt time.Time `json:"published_at"`
	Prerelease  bool      `json:"prerelease"`
	Draft       bool      `json:"draft"`
}

type CheckResult struct {
	HasUpdate      bool      `json:"has_update"`
	CurrentVersion string    `json:"current_version"`
	CurrentCommit  string    `json:"current_commit"`
	LatestCommit   string    `json:"latest_commit,omitempty"`
	LatestVersion  string    `json:"latest_version,omitempty"`
	UpdateMessage  string    `json:"update_message,omitempty"`
	CheckedAt      time.Time `json:"checked_at"`
	UseReleases    bool      `json:"use_releases"`
}

type VersionCache struct {
	LastCheck time.Time   `json:"last_check"`
	Result    CheckResult `json:"result"`
}

func GetInfo() Info {
	commit := Commit
	if commit == "unknown" {
		commit = getRuntimeCommit()
	}

	buildTime := BuildTime
	if buildTime == "unknown" {
		buildTime = getRuntimeBuildTime()
	}

	goVersion := GoVersion
	if goVersion == "unknown" {
		goVersion = getRuntimeGoVersion()
	}

	return Info{
		Version:   Version,
		Commit:    commit,
		BuildTime: buildTime,
		GoVersion: goVersion,
	}
}

func getRuntimeCommit() string {
	// Get the current commit hash
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	commit := strings.TrimSpace(string(output))

	// Check if working tree is dirty (has uncommitted changes)
	cmd = exec.Command("git", "status", "--porcelain")
	statusOutput, err := cmd.Output()
	if err != nil {
		return commit
	}

	// If there are uncommitted changes, add -dirty suffix
	if len(strings.TrimSpace(string(statusOutput))) > 0 {
		return commit + "-dirty"
	}

	return commit
}

func getRuntimeBuildTime() string {
	// Try to get the executable's modification time
	executable, err := os.Executable()
	if err != nil {
		return time.Now().UTC().Format(time.RFC3339)
	}

	stat, err := os.Stat(executable)
	if err != nil {
		return time.Now().UTC().Format(time.RFC3339)
	}

	return stat.ModTime().UTC().Format(time.RFC3339)
}

func getRuntimeGoVersion() string {
	return runtime.Version()
}

func (i Info) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Version: %s\n", i.Version)
	fmt.Fprintf(&sb, "Commit: %s\n", i.Commit)
	fmt.Fprintf(&sb, "Build Time: %s\n", i.BuildTime)
	fmt.Fprintf(&sb, "Go Version: %s\n", i.GoVersion)
	return sb.String()
}

func CheckForUpdates(ctx context.Context, useCache bool) (*CheckResult, error) {
	return CheckForUpdatesWithExpiry(ctx, useCache, DefaultExpiry)
}

func CheckForUpdatesWithDebug(ctx context.Context, useCache bool, debug bool) (*CheckResult, error) {
	return CheckForUpdatesWithExpiryAndDebug(ctx, useCache, DefaultExpiry, debug)
}

// CheckForUpdatesWithAuthenticatedClient performs an update check with an authenticated HTTP client
func CheckForUpdatesWithAuthenticatedClient(ctx context.Context, useCache bool, debug bool, client *http.Client) (*CheckResult, error) {
	return CheckForUpdatesWithExpiryAndAuthenticatedClient(ctx, useCache, DefaultExpiry, debug, client)
}

func CheckForUpdatesWithExpiry(ctx context.Context, useCache bool, cacheExpiry time.Duration) (*CheckResult, error) {
	return CheckForUpdatesWithExpiryAndDebug(ctx, useCache, cacheExpiry, false)
}

func CheckForUpdatesWithExpiryAndDebug(ctx context.Context, useCache bool, cacheExpiry time.Duration, debug bool) (*CheckResult, error) {
	return CheckForUpdatesWithExpiryAndAuthenticatedClient(ctx, useCache, cacheExpiry, debug, nil)
}

// CheckForUpdatesWithExpiryAndAuthenticatedClient performs an update check with optional authenticated client
func CheckForUpdatesWithExpiryAndAuthenticatedClient(ctx context.Context, useCache bool, cacheExpiry time.Duration, debug bool, client *http.Client) (*CheckResult, error) {
	if debug {
		fmt.Printf("DEBUG: Starting update check, useCache=%v, cacheExpiry=%v\n", useCache, cacheExpiry)
	}

	if useCache {
		if cached := loadCache(); cached != nil && time.Since(cached.LastCheck) < cacheExpiry {
			// Check if current state matches cached state
			currentCommit := Commit
			if currentCommit == "unknown" {
				currentCommit = getRuntimeCommit()
			}

			if cached.Result.CurrentVersion == Version && cached.Result.CurrentCommit == currentCommit {
				if debug {
					fmt.Printf("DEBUG: Using cached result from %v\n", cached.LastCheck)
				}
				return &cached.Result, nil
			} else {
				if debug {
					fmt.Printf("DEBUG: Cache invalidated - current state changed (version: %s->%s, commit: %s->%s)\n",
						cached.Result.CurrentVersion, Version, cached.Result.CurrentCommit, currentCommit)
				}
			}
		}
		if debug {
			fmt.Printf("DEBUG: Cache expired or not found, performing fresh check\n")
		}
	}

	result, err := performUpdateCheckWithAuthenticatedClient(ctx, debug, client)
	if err != nil {
		return nil, err
	}

	if useCache {
		saveCache(&VersionCache{
			LastCheck: time.Now(),
			Result:    *result,
		})
		if debug {
			fmt.Printf("DEBUG: Cached update result\n")
		}
	}

	return result, nil
}

// performUpdateCheckWithAuthenticatedClient performs update check with optional authenticated client
func performUpdateCheckWithAuthenticatedClient(ctx context.Context, debug bool, client *http.Client) (*CheckResult, error) {
	currentCommit := Commit
	if currentCommit == "unknown" {
		currentCommit = getRuntimeCommit()
	}

	result := &CheckResult{
		CurrentVersion: Version,
		CurrentCommit:  currentCommit,
		CheckedAt:      time.Now(),
	}

	if debug {
		fmt.Printf("DEBUG: Current version: %s, Current commit: %s\n", Version, currentCommit)
		fmt.Printf("DEBUG: GitHub repo: %s\n", GitHubRepo)
	}

	return checkReleaseVersionWithAuthenticatedClient(ctx, result, debug, client)
}

func checkCommitVersion(ctx context.Context, result *CheckResult) (*CheckResult, error) {
	return checkCommitVersionWithDebug(ctx, result, false)
}

func checkCommitVersionWithDebug(ctx context.Context, result *CheckResult, debug bool) (*CheckResult, error) {
	return checkCommitVersionWithAuthenticatedClient(ctx, result, debug, nil)
}

// checkCommitVersionWithAuthenticatedClient checks for commits with optional authenticated client
func checkCommitVersionWithAuthenticatedClient(ctx context.Context, result *CheckResult, debug bool, client *http.Client) (*CheckResult, error) {
	currentCommit := result.CurrentCommit
	if currentCommit == "unknown" || currentCommit == "" {
		if debug {
			fmt.Printf("DEBUG: Commit is unknown/empty, skipping commit check\n")
		}
		return result, nil
	}

	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	url := fmt.Sprintf("%s/repos/%s/commits/main", GitHubAPI, GitHubRepo)

	if debug {
		fmt.Printf("DEBUG: Checking commits at URL: %s\n", url)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	// Add GitHub token authentication if available for private repositories (backward compatibility)
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "token "+token)
		if debug {
			fmt.Printf("DEBUG: Using GitHub token for private repository access\n")
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		if debug {
			fmt.Printf("DEBUG: Commit API request failed: %v\n", err)
		}
		return result, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if debug {
		fmt.Printf("DEBUG: Commit API response status: %d\n", resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK {
		if debug {
			fmt.Printf("DEBUG: Commit API returned status %d - repository may be private, assuming no updates\n", resp.StatusCode)
		}
		return result, nil
	}

	var commit GitHubCommit
	if err := json.NewDecoder(resp.Body).Decode(&commit); err != nil {
		return nil, fmt.Errorf("decoding commit response: %w", err)
	}

	result.LatestCommit = commit.SHA
	currentCommitShort := currentCommit
	if len(currentCommitShort) > 7 {
		currentCommitShort = currentCommitShort[:7]
	}

	latestCommitShort := commit.SHA
	if len(latestCommitShort) > 7 {
		latestCommitShort = latestCommitShort[:7]
	}

	if currentCommitShort != latestCommitShort {
		result.HasUpdate = true
		result.UpdateMessage = fmt.Sprintf("Newer commit available: %s", latestCommitShort)
	}

	return result, nil
}

func checkReleaseVersion(ctx context.Context, result *CheckResult) (*CheckResult, error) {
	return checkReleaseVersionWithDebug(ctx, result, false)
}

func checkReleaseVersionWithDebug(ctx context.Context, result *CheckResult, debug bool) (*CheckResult, error) {
	return checkReleaseVersionWithAuthenticatedClient(ctx, result, debug, nil)
}

// checkReleaseVersionWithAuthenticatedClient checks for releases with optional authenticated client
func checkReleaseVersionWithAuthenticatedClient(ctx context.Context, result *CheckResult, debug bool, client *http.Client) (*CheckResult, error) {
	result.UseReleases = true

	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	url := fmt.Sprintf("%s/repos/%s/releases/latest", GitHubAPI, GitHubRepo)

	if debug {
		fmt.Printf("DEBUG: Checking releases at URL: %s\n", url)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	// Add GitHub token authentication if available for private repositories (backward compatibility)
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "token "+token)
		if debug {
			fmt.Printf("DEBUG: Using GitHub token for private repository access\n")
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		if debug {
			fmt.Printf("DEBUG: Release API request failed: %v, falling back to commit check\n", err)
		}
		return checkCommitVersionWithAuthenticatedClient(ctx, result, debug, client)
	}
	defer func() { _ = resp.Body.Close() }()

	if debug {
		fmt.Printf("DEBUG: Release API response status: %d\n", resp.StatusCode)
	}

	if resp.StatusCode == http.StatusNotFound {
		if debug {
			fmt.Printf("DEBUG: No releases found (404) - repository may be private, trying git-based check\n")
		}
		return checkGitReleasesWithAuthenticatedClient(ctx, result, debug, client)
	}

	if resp.StatusCode != http.StatusOK {
		if debug {
			fmt.Printf("DEBUG: Release API returned status %d, trying git-based check\n", resp.StatusCode)
		}
		return checkGitReleasesWithAuthenticatedClient(ctx, result, debug, client)
	}

	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decoding release response: %w", err)
	}

	if debug {
		fmt.Printf("DEBUG: Found release: %s (draft=%v, prerelease=%v)\n", release.TagName, release.Draft, release.Prerelease)
	}

	if release.Draft || release.Prerelease {
		if debug {
			fmt.Printf("DEBUG: Release is draft or prerelease, trying git-based check\n")
		}
		return checkGitCommitsWithDebug(ctx, result, debug)
	}

	result.LatestVersion = release.TagName
	isNewer := IsNewerVersion(Version, result.LatestVersion)

	if debug {
		fmt.Printf("DEBUG: Version comparison - current: %s, latest: %s, isNewer: %v\n", Version, result.LatestVersion, isNewer)
	}

	if isNewer {
		result.HasUpdate = true
		result.UpdateMessage = fmt.Sprintf("Version %s available", result.LatestVersion)
		if debug {
			fmt.Printf("DEBUG: Update available: %s\n", result.UpdateMessage)
		}
	} else {
		if debug {
			fmt.Printf("DEBUG: No update needed\n")
		}
	}

	return result, nil
}

func getCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	cacheDir := filepath.Join(home, ".cache", "earmark")
	if err := os.MkdirAll(cacheDir, 0750); err != nil {
		return "", err
	}

	return cacheDir, nil
}

func loadCache() *VersionCache {
	cacheDir, err := getCacheDir()
	if err != nil {
		return nil
	}

	// Include repository name in cache filename to prevent contamination
	repoSafe := strings.ReplaceAll(GitHubRepo, "/", "_")
	cacheFileName := fmt.Sprintf("%s_%s", repoSafe, CacheFile)
	cachePath := filepath.Join(cacheDir, cacheFileName)

	// #nosec G304 - cachePath is constructed from known safe components
	data, err := os.ReadFile(filepath.Clean(cachePath))
	if err != nil {
		return nil
	}

	var cache VersionCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil
	}

	return &cache
}

func saveCache(cache *VersionCache) {
	cacheDir, err := getCacheDir()
	if err != nil {
		return
	}

	// Include repository name in cache filename to prevent contamination
	repoSafe := strings.ReplaceAll(GitHubRepo, "/", "_")
	cacheFileName := fmt.Sprintf("%s_%s", repoSafe, CacheFile)
	cachePath := filepath.Join(cacheDir, cacheFileName)

	data, err := json.Marshal(cache)
	if err != nil {
		return
	}

	if err := os.WriteFile(cachePath, data, 0600); err != nil {
		// Ignore cache write errors, not critical
		_ = err
	}
}

func IsNewerVersion(current, latest string) bool {
	if latest == "dev" {
		return false
	}
	if current == "dev" {
		return true
	}

	if !strings.HasPrefix(current, "v") {
		current = "v" + current
	}
	if !strings.HasPrefix(latest, "v") {
		latest = "v" + latest
	}

	currentParts := parseVersion(current)
	latestParts := parseVersion(latest)

	if len(currentParts) != 3 || len(latestParts) != 3 {
		return current != latest
	}

	for i := 0; i < 3; i++ {
		if latestParts[i] > currentParts[i] {
			return true
		}
		if latestParts[i] < currentParts[i] {
			return false
		}
	}

	return false
}

func parseVersion(version string) []int {
	version = strings.TrimPrefix(version, "v")
	parts := strings.Split(version, ".")
	result := make([]int, len(parts))

	for i, part := range parts {
		if num, err := strconv.Atoi(part); err == nil {
			result[i] = num
		}
	}

	return result
}

// checkGitReleasesWithAuthenticatedClient checks for releases using git commands (works with private repos)
func checkGitReleasesWithAuthenticatedClient(ctx context.Context, result *CheckResult, debug bool, client *http.Client) (*CheckResult, error) {
	result.UseReleases = true

	if debug {
		fmt.Printf("DEBUG: Checking releases using git ls-remote for private repository access\n")
	}

	// Try to get tags from remote repository using git
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--tags", "--sort=-version:refname", "origin")
	output, err := cmd.Output()
	if err != nil {
		if debug {
			fmt.Printf("DEBUG: Git ls-remote failed: %v, falling back to commit check\n", err)
		}
		return checkGitCommitsWithDebug(ctx, result, debug)
	}

	if debug && os.Getenv("LOG_VERBOSE") == "1" {
		fmt.Printf("DEBUG: Git ls-remote tags output: %s\n", string(output))
	}

	// Parse the output to find the latest semantic version tag
	latestVersion := parseGitTags(string(output), debug)
	if latestVersion == "" {
		if debug {
			fmt.Printf("DEBUG: No valid semantic version tags found, falling back to commit check\n")
		}
		return checkGitCommitsWithDebug(ctx, result, debug)
	}

	result.LatestVersion = latestVersion
	if IsNewerVersion(Version, result.LatestVersion) {
		result.HasUpdate = true
		result.UpdateMessage = fmt.Sprintf("Version %s available", result.LatestVersion)
		if debug {
			fmt.Printf("DEBUG: Update available via git: %s\n", result.UpdateMessage)
		}
	} else {
		if debug {
			fmt.Printf("DEBUG: No update needed via git - current: %s, latest: %s\n", Version, result.LatestVersion)
		}
	}

	return result, nil
}

// checkGitCommitsWithDebug checks for commits using git commands (works with private repos)
func checkGitCommitsWithDebug(ctx context.Context, result *CheckResult, debug bool) (*CheckResult, error) {
	currentCommit := result.CurrentCommit
	if currentCommit == "unknown" || currentCommit == "" {
		if debug {
			fmt.Printf("DEBUG: Current commit unknown, skipping git commit check\n")
		}
		return result, nil
	}

	if debug {
		fmt.Printf("DEBUG: Checking commits using git ls-remote for private repository access\n")
	}

	// Get the latest commit from the remote main branch
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "origin", "main")
	output, err := cmd.Output()
	if err != nil {
		if debug {
			fmt.Printf("DEBUG: Git ls-remote for commits failed: %v\n", err)
		}
		return result, nil
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 {
		if debug {
			fmt.Printf("DEBUG: No commit information from git ls-remote\n")
		}
		return result, nil
	}

	// Parse the commit hash (first part before whitespace)
	parts := strings.Fields(lines[0])
	if len(parts) == 0 {
		if debug {
			fmt.Printf("DEBUG: Could not parse commit from git output\n")
		}
		return result, nil
	}

	latestCommit := parts[0]
	result.LatestCommit = latestCommit

	// Compare commit hashes (use first 7 characters)
	currentCommitShort := currentCommit
	if len(currentCommitShort) > 7 {
		currentCommitShort = currentCommitShort[:7]
	}

	latestCommitShort := latestCommit
	if len(latestCommitShort) > 7 {
		latestCommitShort = latestCommitShort[:7]
	}

	if debug {
		fmt.Printf("DEBUG: Comparing commits - current: %s, latest: %s\n", currentCommitShort, latestCommitShort)
	}

	if currentCommitShort != latestCommitShort {
		result.HasUpdate = true
		result.UpdateMessage = fmt.Sprintf("Newer commit available: %s", latestCommitShort)
		if debug {
			fmt.Printf("DEBUG: Update available via git commits: %s\n", result.UpdateMessage)
		}
	}

	return result, nil
}

// parseGitTags extracts the latest semantic version from git ls-remote tags output
func parseGitTags(output string, debug bool) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	var latestVersion string

	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		// Extract tag name from refs/tags/tagname
		tagRef := parts[1]
		if !strings.HasPrefix(tagRef, "refs/tags/") {
			continue
		}

		tag := strings.TrimPrefix(tagRef, "refs/tags/")

		// Skip tags ending with ^{} (annotated tag objects)
		if strings.HasSuffix(tag, "^{}") {
			continue
		}

		// Check if it's a semantic version tag (starts with v or is just numbers.numbers.numbers)
		if isSemanticVersion(tag) {
			if debug {
				fmt.Printf("DEBUG: Found semantic version tag: %s\n", tag)
			}
			// Since git ls-remote --sort=-version:refname gives us tags in descending order,
			// the first valid semantic version we find is the latest
			if latestVersion == "" {
				latestVersion = tag
			}
		}
	}

	return latestVersion
}

// isSemanticVersion checks if a string looks like a semantic version
func isSemanticVersion(tag string) bool {
	// Remove 'v' prefix if present
	version := strings.TrimPrefix(tag, "v")

	// Check if it matches basic semantic version pattern (x.y.z)
	parts := strings.Split(version, ".")
	if len(parts) < 2 || len(parts) > 4 {
		return false
	}

	// Check if all parts are numbers
	for _, part := range parts {
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}

	return true
}
