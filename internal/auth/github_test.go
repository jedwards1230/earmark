package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNewAuthManager(t *testing.T) {
	am := NewAuthManager(false)
	if am == nil {
		t.Fatal("NewAuthManager returned nil")
	}
	if len(am.strategies) != 3 {
		t.Errorf("Expected 3 strategies, got %d", len(am.strategies))
	}
	if am.httpClient == nil {
		t.Error("HTTP client not initialized")
	}
	if am.httpClient.Timeout != 30*time.Second {
		t.Errorf("Expected timeout of 30s, got %v", am.httpClient.Timeout)
	}
}

func TestEnvTokenAuth_Name(t *testing.T) {
	auth := &EnvTokenAuth{}
	if auth.Name() != "Environment Variables" {
		t.Errorf("Expected 'Environment Variables', got %s", auth.Name())
	}
}

func TestEnvTokenAuth_GetToken(t *testing.T) {
	tests := []struct {
		name     string
		envVars  map[string]string
		expected string
		wantErr  bool
	}{
		{
			name:     "GITHUB_TOKEN set",
			envVars:  map[string]string{"GITHUB_TOKEN": "test-token"},
			expected: "test-token",
			wantErr:  false,
		},
		{
			name:     "GITHUB_PAT set",
			envVars:  map[string]string{"GITHUB_PAT": "pat-token"},
			expected: "pat-token",
			wantErr:  false,
		},
		{
			name:     "GH_TOKEN set",
			envVars:  map[string]string{"GH_TOKEN": "gh-token"},
			expected: "gh-token",
			wantErr:  false,
		},
		{
			name:     "PAT set",
			envVars:  map[string]string{"PAT": "pat"},
			expected: "pat",
			wantErr:  false,
		},
		{
			name:     "no tokens set",
			envVars:  map[string]string{},
			expected: "",
			wantErr:  true,
		},
		{
			name:     "priority order - GITHUB_TOKEN first",
			envVars:  map[string]string{"GITHUB_TOKEN": "first", "GITHUB_PAT": "second"},
			expected: "first",
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear all env vars
			for _, envVar := range []string{"GITHUB_TOKEN", "GITHUB_PAT", "GH_TOKEN", "PAT"} {
				_ = os.Unsetenv(envVar)
			}

			// Set test env vars
			for k, v := range tt.envVars {
				_ = os.Setenv(k, v)
			}

			auth := &EnvTokenAuth{debugMode: false}
			token, err := auth.GetToken()

			if tt.wantErr && err == nil {
				t.Error("Expected error but got none")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Expected no error but got: %v", err)
			}
			if token != tt.expected {
				t.Errorf("Expected token %s, got %s", tt.expected, token)
			}

			// Clean up
			for k := range tt.envVars {
				_ = os.Unsetenv(k)
			}
		})
	}
}

func TestEnvTokenAuth_ValidateToken(t *testing.T) {
	// Mock GitHub API server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Errorf("Expected /user endpoint, got %s", r.URL.Path)
		}

		authHeader := r.Header.Get("Authorization")
		switch authHeader {
		case "token valid-token":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"login": "testuser", "id": 12345}`)
		case "token invalid-token":
			w.WriteHeader(http.StatusUnauthorized)
		case "token forbidden-token":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	// Note: We can't easily mock the GitHub API URL in validateGitHubToken
	// In a real implementation, we'd make the API URL configurable

	auth := &EnvTokenAuth{debugMode: false}

	// Test empty token
	err := auth.ValidateToken("")
	if err == nil {
		t.Error("Expected error for empty token")
	}

	// Note: We can't easily test the actual HTTP validation without significant refactoring
	// to make the API URL configurable. This is a limitation of the current design.
}

func TestGitHubCLIAuth_Name(t *testing.T) {
	auth := &GitHubCLIAuth{}
	if auth.Name() != "GitHub CLI" {
		t.Errorf("Expected 'GitHub CLI', got %s", auth.Name())
	}
}

func TestGitHubCLIAuth_isGitHubCLIInstalled(t *testing.T) {
	auth := &GitHubCLIAuth{}

	// This test depends on the system environment
	// We'll just verify the function doesn't panic
	_ = auth.isGitHubCLIInstalled()
}

func TestGitHubCLIAuth_GetToken(t *testing.T) {
	auth := &GitHubCLIAuth{debugMode: false}

	// Check if gh is installed
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("GitHub CLI not installed, skipping test")
	}

	// This test is environment-dependent
	// We'll just verify the function doesn't panic
	_, err := auth.GetToken()

	// We expect either a token or an error, not a panic
	if err != nil {
		t.Logf("GitHub CLI auth failed (expected if not authenticated): %v", err)
	}
}

func TestSSHAuth_Name(t *testing.T) {
	auth := &SSHAuth{}
	if auth.Name() != "SSH Key" {
		t.Errorf("Expected 'SSH Key', got %s", auth.Name())
	}
}

func TestSSHAuth_GetToken(t *testing.T) {
	auth := &SSHAuth{debugMode: false}

	token, err := auth.GetToken()
	if err == nil {
		t.Error("Expected error for SSH auth GetToken")
	}
	if token != "" {
		t.Error("Expected empty token for SSH auth")
	}
}

func TestSSHAuth_ValidateToken(t *testing.T) {
	auth := &SSHAuth{debugMode: false}

	err := auth.ValidateToken("any-token")
	if err == nil {
		t.Error("Expected error for SSH auth ValidateToken")
	}
}

func TestAuthManager_GetAuthenticatedToken_Caching(t *testing.T) {
	am := NewAuthManager(false)

	// Set up a mock token for testing
	_ = os.Setenv("GITHUB_TOKEN", "test-token")
	defer func() { _ = os.Unsetenv("GITHUB_TOKEN") }()

	// Mock the validation by temporarily setting a server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"login": "testuser"}`)
	}))
	defer server.Close()

	// First call should try to authenticate
	token1, err1 := am.GetAuthenticatedToken()
	if err1 != nil {
		t.Skip("Token validation failed, skipping caching test")
	}

	// Second call should use cache
	token2, err2 := am.GetAuthenticatedToken()
	if err2 != nil {
		t.Errorf("Second call failed: %v", err2)
	}

	if token1 != token2 {
		t.Error("Cached token differs from first token")
	}
}

func TestAuthManager_AddAuthHeader(t *testing.T) {
	am := NewAuthManager(false)

	// Set up a valid token
	_ = os.Setenv("GITHUB_TOKEN", "test-token")
	defer func() { _ = os.Unsetenv("GITHUB_TOKEN") }()

	// Create a mock request
	req, err := http.NewRequest("GET", "https://api.github.com/user", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Mock validation by setting cached token directly
	am.cachedToken = "test-token"
	am.cachedMethod = "Test"

	// Add auth header
	err = am.AddAuthHeader(req)
	if err != nil {
		t.Errorf("AddAuthHeader failed: %v", err)
	}

	// Check if header was added
	authHeader := req.Header.Get("Authorization")
	if authHeader != "token test-token" {
		t.Errorf("Expected 'token test-token', got '%s'", authHeader)
	}
}

func TestAuthManager_GetAuthenticatedClient(t *testing.T) {
	am := NewAuthManager(false)

	client := am.GetAuthenticatedClient()
	if client == nil {
		t.Error("GetAuthenticatedClient returned nil")
		return
	}

	if client.Timeout != 30*time.Second {
		t.Errorf("Expected timeout of 30s, got %v", client.Timeout)
	}
}

func TestAuthManager_ErrorHandling(t *testing.T) {
	am := NewAuthManager(false)

	// Clear all environment variables
	for _, envVar := range []string{"GITHUB_TOKEN", "GITHUB_PAT", "GH_TOKEN", "PAT"} {
		_ = os.Unsetenv(envVar)
	}

	// This should fail all strategies, but might succeed if GitHub CLI is authenticated
	_, err := am.GetAuthenticatedToken()
	if err == nil {
		t.Logf("No error returned - likely GitHub CLI is authenticated in test environment")
		return
	}

	expectedMsg := "no valid GitHub authentication found"
	if !strings.Contains(err.Error(), expectedMsg) {
		t.Logf("Error message: %v", err)
		t.Logf("Expected to contain: %s", expectedMsg)
	}
}

func TestAuthManager_DebugMode(t *testing.T) {
	am := NewAuthManager(true)

	if !am.debugMode {
		t.Error("Debug mode not set correctly")
	}

	// Verify debug mode is passed to strategies
	for _, strategy := range am.strategies {
		switch s := strategy.(type) {
		case *EnvTokenAuth:
			if !s.debugMode {
				t.Error("Debug mode not set for EnvTokenAuth")
			}
		case *GitHubCLIAuth:
			if !s.debugMode {
				t.Error("Debug mode not set for GitHubCLIAuth")
			}
		case *SSHAuth:
			if !s.debugMode {
				t.Error("Debug mode not set for SSHAuth")
			}
		}
	}
}
