package version

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
	GitHubRepo       = "jedwards1230/lil-whisper"
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
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
		GoVersion: GoVersion,
	}
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

func CheckForUpdatesWithExpiry(ctx context.Context, useCache bool, cacheExpiry time.Duration) (*CheckResult, error) {
	if useCache {
		if cached := loadCache(); cached != nil && time.Since(cached.LastCheck) < cacheExpiry {
			return &cached.Result, nil
		}
	}

	result, err := performUpdateCheck(ctx)
	if err != nil {
		return nil, err
	}

	if useCache {
		saveCache(&VersionCache{
			LastCheck: time.Now(),
			Result:    *result,
		})
	}

	return result, nil
}

func performUpdateCheck(ctx context.Context) (*CheckResult, error) {
	result := &CheckResult{
		CurrentVersion: Version,
		CurrentCommit:  Commit,
		CheckedAt:      time.Now(),
	}

	if Version != "dev" && !strings.Contains(Version, "unknown") {
		return checkReleaseVersion(ctx, result)
	}

	return checkCommitVersion(ctx, result)
}

func checkCommitVersion(ctx context.Context, result *CheckResult) (*CheckResult, error) {
	if Commit == "unknown" || Commit == "" {
		return result, nil
	}

	client := &http.Client{Timeout: 10 * time.Second}
	url := fmt.Sprintf("%s/repos/%s/commits/main", GitHubAPI, GitHubRepo)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching latest commit: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var commit GitHubCommit
	if err := json.NewDecoder(resp.Body).Decode(&commit); err != nil {
		return nil, fmt.Errorf("decoding commit response: %w", err)
	}

	result.LatestCommit = commit.SHA
	currentCommitShort := Commit
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
	result.UseReleases = true

	client := &http.Client{Timeout: 10 * time.Second}
	url := fmt.Sprintf("%s/repos/%s/releases/latest", GitHubAPI, GitHubRepo)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return checkCommitVersion(ctx, result)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decoding release response: %w", err)
	}

	if release.Draft || release.Prerelease {
		return checkCommitVersion(ctx, result)
	}

	result.LatestVersion = release.TagName
	if result.LatestVersion != Version {
		result.HasUpdate = true
		result.UpdateMessage = fmt.Sprintf("Version %s available", result.LatestVersion)
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

	cachePath := filepath.Join(cacheDir, CacheFile)
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

	cachePath := filepath.Join(cacheDir, CacheFile)
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
	if current == "dev" || latest == "dev" {
		return false
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
