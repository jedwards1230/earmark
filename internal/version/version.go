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
	GitHubRepo       = "jedwards1230/lilbro-whisper"
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
	sb.WriteString(fmt.Sprintf("Version: %s\n", i.Version))
	sb.WriteString(fmt.Sprintf("Commit: %s\n", i.Commit))
	sb.WriteString(fmt.Sprintf("Build Time: %s\n", i.BuildTime))
	sb.WriteString(fmt.Sprintf("Go Version: %s\n", i.GoVersion))
	return sb.String()
}

func CheckForUpdates(ctx context.Context, useCache bool) (*CheckResult, error) {
	return CheckForUpdatesWithExpiry(ctx, useCache, DefaultExpiry)
}

func CheckForUpdatesWithDebug(ctx context.Context, useCache bool, debug bool) (*CheckResult, error) {
	return CheckForUpdatesWithExpiryAndDebug(ctx, useCache, DefaultExpiry, debug)
}

func CheckForUpdatesWithExpiry(ctx context.Context, useCache bool, cacheExpiry time.Duration) (*CheckResult, error) {
	return CheckForUpdatesWithExpiryAndDebug(ctx, useCache, cacheExpiry, false)
}

func CheckForUpdatesWithExpiryAndDebug(ctx context.Context, useCache bool, cacheExpiry time.Duration, debug bool) (*CheckResult, error) {
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

	result, err := performUpdateCheckWithDebug(ctx, debug)
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

func performUpdateCheck(ctx context.Context) (*CheckResult, error) {
	return performUpdateCheckWithDebug(ctx, false)
}

func performUpdateCheckWithDebug(ctx context.Context, debug bool) (*CheckResult, error) {
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

	return checkReleaseVersionWithDebug(ctx, result, debug)
}

func checkCommitVersion(ctx context.Context, result *CheckResult) (*CheckResult, error) {
	return checkCommitVersionWithDebug(ctx, result, false)
}

func checkCommitVersionWithDebug(ctx context.Context, result *CheckResult, debug bool) (*CheckResult, error) {
	currentCommit := result.CurrentCommit
	if currentCommit == "unknown" || currentCommit == "" {
		if debug {
			fmt.Printf("DEBUG: Commit is unknown/empty, skipping commit check\n")
		}
		return result, nil
	}

	client := &http.Client{Timeout: 10 * time.Second}
	url := fmt.Sprintf("%s/repos/%s/commits/main", GitHubAPI, GitHubRepo)

	if debug {
		fmt.Printf("DEBUG: Checking commits at URL: %s\n", url)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		if debug {
			fmt.Printf("DEBUG: Commit API request failed: %v\n", err)
		}
		return result, nil
	}
	defer resp.Body.Close()

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
	result.UseReleases = true

	client := &http.Client{Timeout: 10 * time.Second}
	url := fmt.Sprintf("%s/repos/%s/releases/latest", GitHubAPI, GitHubRepo)

	if debug {
		fmt.Printf("DEBUG: Checking releases at URL: %s\n", url)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		if debug {
			fmt.Printf("DEBUG: Release API request failed: %v, falling back to commit check\n", err)
		}
		return checkCommitVersionWithDebug(ctx, result, debug)
	}
	defer resp.Body.Close()

	if debug {
		fmt.Printf("DEBUG: Release API response status: %d\n", resp.StatusCode)
	}

	if resp.StatusCode == http.StatusNotFound {
		if debug {
			fmt.Printf("DEBUG: No releases found (404) - repository may be private, falling back to commit check\n")
		}
		return checkCommitVersionWithDebug(ctx, result, debug)
	}

	if resp.StatusCode != http.StatusOK {
		if debug {
			fmt.Printf("DEBUG: Release API returned status %d, falling back to commit check\n", resp.StatusCode)
		}
		return checkCommitVersionWithDebug(ctx, result, debug)
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
			fmt.Printf("DEBUG: Release is draft or prerelease, falling back to commit check\n")
		}
		return checkCommitVersionWithDebug(ctx, result, debug)
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

	cacheDir := filepath.Join(home, ".cache", "lil-whisper")
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
