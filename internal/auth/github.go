package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

type GitHubAuth interface {
	GetToken() (string, error)
	ValidateToken(token string) error
	Name() string
}

type AuthManager struct {
	strategies    []GitHubAuth
	cachedToken   string
	cachedMethod  string
	debugMode     bool
	httpClient    *http.Client
}

func NewAuthManager(debugMode bool) *AuthManager {
	return &AuthManager{
		strategies: []GitHubAuth{
			&EnvTokenAuth{debugMode: debugMode},
			&GitHubCLIAuth{debugMode: debugMode},
			&SSHAuth{debugMode: debugMode},
		},
		debugMode: debugMode,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (am *AuthManager) GetAuthenticatedToken() (string, error) {
	if am.cachedToken != "" {
		if am.debugMode {
			fmt.Printf("DEBUG: Using cached token from %s\n", am.cachedMethod)
		}
		return am.cachedToken, nil
	}

	var lastErr error
	for _, strategy := range am.strategies {
		if am.debugMode {
			fmt.Printf("DEBUG: Trying authentication method: %s\n", strategy.Name())
		}

		token, err := strategy.GetToken()
		if err != nil {
			if am.debugMode {
				fmt.Printf("DEBUG: %s failed: %v\n", strategy.Name(), err)
			}
			lastErr = err
			continue
		}

		if token == "" {
			if am.debugMode {
				fmt.Printf("DEBUG: %s returned empty token\n", strategy.Name())
			}
			continue
		}

		if am.debugMode {
			fmt.Printf("DEBUG: %s returned token, validating...\n", strategy.Name())
		}

		if err := strategy.ValidateToken(token); err != nil {
			if am.debugMode {
				fmt.Printf("DEBUG: %s token validation failed: %v\n", strategy.Name(), err)
			}
			lastErr = err
			continue
		}

		if am.debugMode {
			fmt.Printf("DEBUG: %s authentication successful\n", strategy.Name())
		}

		am.cachedToken = token
		am.cachedMethod = strategy.Name()
		return token, nil
	}

	return "", fmt.Errorf("no valid GitHub authentication found - tried all methods: %w", lastErr)
}

func (am *AuthManager) GetAuthenticatedClient() *http.Client {
	return am.httpClient
}

func (am *AuthManager) AddAuthHeader(req *http.Request) error {
	token, err := am.GetAuthenticatedToken()
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "token "+token)
	return nil
}

type EnvTokenAuth struct {
	debugMode bool
}

func (e *EnvTokenAuth) Name() string {
	return "Environment Variables"
}

func (e *EnvTokenAuth) GetToken() (string, error) {
	envVars := []string{"GITHUB_TOKEN", "GITHUB_PAT", "GH_TOKEN", "PAT"}
	
	for _, envVar := range envVars {
		if token := os.Getenv(envVar); token != "" {
			if e.debugMode {
				fmt.Printf("DEBUG: Found token in environment variable %s\n", envVar)
			}
			return token, nil
		}
	}

	return "", fmt.Errorf("no GitHub token found in environment variables: %s", strings.Join(envVars, ", "))
}

func (e *EnvTokenAuth) ValidateToken(token string) error {
	return validateGitHubToken(token, e.debugMode)
}

type GitHubCLIAuth struct {
	debugMode bool
}

func (g *GitHubCLIAuth) Name() string {
	return "GitHub CLI"
}

func (g *GitHubCLIAuth) GetToken() (string, error) {
	if !g.isGitHubCLIInstalled() {
		return "", fmt.Errorf("GitHub CLI (gh) is not installed")
	}

	if !g.isGitHubCLIAuthenticated() {
		return "", fmt.Errorf("GitHub CLI is not authenticated - run 'gh auth login'")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get GitHub CLI token: %w", err)
	}

	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", fmt.Errorf("GitHub CLI returned empty token")
	}

	if g.debugMode {
		fmt.Printf("DEBUG: GitHub CLI returned token\n")
	}

	return token, nil
}

func (g *GitHubCLIAuth) ValidateToken(token string) error {
	return validateGitHubToken(token, g.debugMode)
}

func (g *GitHubCLIAuth) isGitHubCLIInstalled() bool {
	_, err := exec.LookPath("gh")
	return err == nil
}

func (g *GitHubCLIAuth) isGitHubCLIAuthenticated() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "auth", "status")
	err := cmd.Run()
	return err == nil
}

type SSHAuth struct {
	debugMode bool
}

func (s *SSHAuth) Name() string {
	return "SSH Key"
}

func (s *SSHAuth) GetToken() (string, error) {
	return "", fmt.Errorf("SSH authentication is not supported for GitHub API/release downloads - use for git operations only")
}

func (s *SSHAuth) ValidateToken(token string) error {
	return fmt.Errorf("SSH authentication does not use tokens")
}

func (s *SSHAuth) IsSSHConfigured() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ssh", "-T", "git@github.com")
	err := cmd.Run()
	
	if exitError, ok := err.(*exec.ExitError); ok {
		return exitError.ExitCode() == 1
	}
	
	return err == nil
}

func validateGitHubToken(token string, debugMode bool) error {
	if token == "" {
		return fmt.Errorf("token is empty")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	
	req, err := http.NewRequest("GET", "https://api.github.com/user", nil)
	if err != nil {
		return fmt.Errorf("failed to create validation request: %w", err)
	}

	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to validate token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		var user map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&user); err == nil {
			if debugMode {
				if login, ok := user["login"].(string); ok {
					fmt.Printf("DEBUG: Token validated for user: %s\n", login)
				}
			}
		}
		return nil
	}

	switch resp.StatusCode {
	case 401:
		return fmt.Errorf("token is invalid or expired")
	case 403:
		return fmt.Errorf("token lacks required permissions")
	default:
		return fmt.Errorf("token validation failed with status %d", resp.StatusCode)
	}
}