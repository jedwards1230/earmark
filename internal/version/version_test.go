package version

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetInfo(t *testing.T) {
	originalVersion := Version
	originalCommit := Commit
	originalBuildTime := BuildTime
	originalGoVersion := GoVersion

	defer func() {
		Version = originalVersion
		Commit = originalCommit
		BuildTime = originalBuildTime
		GoVersion = originalGoVersion
	}()

	Version = "1.0.0"
	Commit = "abc123"
	BuildTime = "2024-01-01T00:00:00Z"
	GoVersion = "go1.21.0"

	info := GetInfo()
	assert.Equal(t, "1.0.0", info.Version)
	assert.Equal(t, "abc123", info.Commit)
	assert.Equal(t, "2024-01-01T00:00:00Z", info.BuildTime)
	assert.Equal(t, "go1.21.0", info.GoVersion)
}

func TestInfoString(t *testing.T) {
	info := Info{
		Version:   "1.0.0",
		Commit:    "abc123",
		BuildTime: "2024-01-01T00:00:00Z",
		GoVersion: "go1.21.0",
	}

	expected := `Version: 1.0.0
Commit: abc123
Build Time: 2024-01-01T00:00:00Z
Go Version: go1.21.0
`
	assert.Equal(t, expected, info.String())
}

func TestIsNewerVersion(t *testing.T) {
	tests := []struct {
		name     string
		current  string
		latest   string
		expected bool
	}{
		{"same version", "1.0.0", "1.0.0", false},
		{"patch update", "1.0.0", "1.0.1", true},
		{"minor update", "1.0.0", "1.1.0", true},
		{"major update", "1.0.0", "2.0.0", true},
		{"current newer", "1.1.0", "1.0.0", false},
		{"with v prefix", "v1.0.0", "v1.0.1", true},
		{"mixed prefix", "1.0.0", "v1.0.1", true},
		{"dev version", "dev", "1.0.0", true},
		{"latest dev", "1.0.0", "dev", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsNewerVersion(tt.current, tt.latest)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		version  string
		expected []int
	}{
		{"1.0.0", []int{1, 0, 0}},
		{"v1.2.3", []int{1, 2, 3}},
		{"2.10.5", []int{2, 10, 5}},
		{"invalid", []int{0}},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			result := parseVersion(tt.version)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCheckCommitVersion(t *testing.T) {
	originalCommit := Commit
	defer func() { Commit = originalCommit }()

	Commit = "abc1234"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.Path, "/repos/jedwards1230/lilbro-whisper/commits/main")

		commit := GitHubCommit{
			SHA: "def5678",
			Commit: struct {
				Author struct {
					Date time.Time `json:"date"`
				} `json:"author"`
				Message string `json:"message"`
			}{
				Message: "Latest commit",
			},
		}

		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(commit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	originalGitHubAPI := GitHubAPI
	defer func() { GitHubAPI = originalGitHubAPI }()

	GitHubAPI = server.URL

	result := &CheckResult{
		CurrentCommit: Commit,
		CheckedAt:     time.Now(),
	}

	updatedResult, err := checkCommitVersion(context.Background(), result)
	require.NoError(t, err)

	assert.True(t, updatedResult.HasUpdate)
	assert.Equal(t, "def5678", updatedResult.LatestCommit)
	assert.Contains(t, updatedResult.UpdateMessage, "Newer commit available")
}

func TestCheckCommitVersionNoUpdate(t *testing.T) {
	originalCommit := Commit
	defer func() { Commit = originalCommit }()

	Commit = "abc1234"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		commit := GitHubCommit{
			SHA: "abc1234",
		}

		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(commit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	originalGitHubAPI := GitHubAPI
	defer func() { GitHubAPI = originalGitHubAPI }()

	GitHubAPI = server.URL

	result := &CheckResult{
		CurrentCommit: Commit,
		CheckedAt:     time.Now(),
	}

	updatedResult, err := checkCommitVersion(context.Background(), result)
	require.NoError(t, err)

	assert.False(t, updatedResult.HasUpdate)
	assert.Equal(t, "abc1234", updatedResult.LatestCommit)
}

func TestCheckReleaseVersion(t *testing.T) {
	originalVersion := Version
	defer func() { Version = originalVersion }()

	Version = "1.0.0"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.Path, "/repos/jedwards1230/lilbro-whisper/releases/latest")

		release := GitHubRelease{
			TagName:     "v1.1.0",
			Name:        "Release 1.1.0",
			PublishedAt: time.Now(),
			Prerelease:  false,
			Draft:       false,
		}

		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(release)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	originalGitHubAPI := GitHubAPI
	defer func() { GitHubAPI = originalGitHubAPI }()

	GitHubAPI = server.URL

	result := &CheckResult{
		CurrentVersion: Version,
		CheckedAt:      time.Now(),
	}

	updatedResult, err := checkReleaseVersion(context.Background(), result)
	require.NoError(t, err)

	assert.True(t, updatedResult.HasUpdate)
	assert.True(t, updatedResult.UseReleases)
	assert.Equal(t, "v1.1.0", updatedResult.LatestVersion)
	assert.Contains(t, updatedResult.UpdateMessage, "Version v1.1.0 available")
}

func TestCacheOperations(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	cache := &VersionCache{
		LastCheck: time.Now(),
		Result: CheckResult{
			HasUpdate:      true,
			CurrentVersion: "1.0.0",
			LatestVersion:  "1.1.0",
			CheckedAt:      time.Now(),
		},
	}

	saveCache(cache)

	loaded := loadCache()
	require.NotNil(t, loaded)
	assert.Equal(t, cache.Result.HasUpdate, loaded.Result.HasUpdate)
	assert.Equal(t, cache.Result.CurrentVersion, loaded.Result.CurrentVersion)
	assert.Equal(t, cache.Result.LatestVersion, loaded.Result.LatestVersion)
}

func TestCheckForUpdatesWithCache(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	cache := &VersionCache{
		LastCheck: time.Now().Add(-1 * time.Hour),
		Result: CheckResult{
			HasUpdate:      true,
			CurrentVersion: "1.0.0",
			LatestVersion:  "1.1.0",
			CheckedAt:      time.Now().Add(-1 * time.Hour),
		},
	}
	saveCache(cache)

	result, err := CheckForUpdates(context.Background(), true)
	require.NoError(t, err)

	assert.Equal(t, cache.Result.HasUpdate, result.HasUpdate)
	assert.Equal(t, cache.Result.CurrentVersion, result.CurrentVersion)
	assert.Equal(t, cache.Result.LatestVersion, result.LatestVersion)
}

func TestGetCacheDir(t *testing.T) {
	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", originalHome)

	cacheDir, err := getCacheDir()
	require.NoError(t, err)

	expectedDir := filepath.Join(tmpDir, ".cache", "lil-whisper")
	assert.Equal(t, expectedDir, cacheDir)

	info, err := os.Stat(cacheDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}
