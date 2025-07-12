# GitHub Authentication Package

This package provides robust GitHub authentication for the lil-whisper update system, supporting multiple authentication methods with automatic fallback.

## Features

- **Multiple Authentication Methods**: Supports environment variables, GitHub CLI, and SSH keys
- **Automatic Fallback**: Tries authentication methods in priority order
- **Token Validation**: Validates tokens against GitHub API before use
- **Caching**: Caches successful authentication for performance
- **Debug Mode**: Detailed logging for troubleshooting

## Authentication Methods (Priority Order)

### 1. Environment Variables (Highest Priority)
The package checks for GitHub tokens in these environment variables:
- `GITHUB_TOKEN` (most common)
- `GITHUB_PAT` (GitHub Personal Access Token)
- `GH_TOKEN` (GitHub CLI style)
- `PAT` (generic Personal Access Token)

**Setup:**
```bash
export GITHUB_TOKEN=ghp_your_token_here
# or
export GITHUB_PAT=ghp_your_token_here
```

### 2. GitHub CLI Authentication
If GitHub CLI is installed and authenticated, the package will use its credentials.

**Setup:**
```bash
# Install GitHub CLI
brew install gh

# Authenticate
gh auth login

# The package will automatically use these credentials
```

### 3. SSH Keys (Lowest Priority)
SSH authentication is supported for git operations only. This method cannot be used for HTTP downloads of release assets.

**Setup:**
```bash
# SSH keys are configured at the system level
# This method is automatically detected if SSH keys are configured
```

## Usage

### Basic Usage
```go
package main

import (
    "github.com/jedwards1230/lil-whisper/internal/auth"
)

func main() {
    // Create auth manager
    authManager := auth.NewAuthManager(false) // false = disable debug mode
    
    // Get authenticated token
    token, err := authManager.GetAuthenticatedToken()
    if err != nil {
        log.Fatal("Authentication failed:", err)
    }
    
    // Use token for API calls
    fmt.Println("Got token:", token)
}
```

### With HTTP Client
```go
// Get authenticated HTTP client
client := authManager.GetAuthenticatedClient()

// Create request
req, err := http.NewRequest("GET", "https://api.github.com/user", nil)
if err != nil {
    log.Fatal(err)
}

// Add authentication header
err = authManager.AddAuthHeader(req)
if err != nil {
    log.Fatal("Failed to add auth header:", err)
}

// Make request
resp, err := client.Do(req)
```

### Debug Mode
```go
// Enable debug mode for detailed logging
authManager := auth.NewAuthManager(true) // true = enable debug mode

// This will output detailed authentication attempts
token, err := authManager.GetAuthenticatedToken()
```

## Debug Output Example
```
DEBUG: Trying authentication method: Environment Variables
DEBUG: Environment Variables failed: no GitHub token found in environment variables: GITHUB_TOKEN, GITHUB_PAT, GH_TOKEN, PAT
DEBUG: Trying authentication method: GitHub CLI
DEBUG: GitHub CLI returned token
DEBUG: GitHub CLI returned token, validating...
DEBUG: Token validated for user: username
DEBUG: GitHub CLI authentication successful
```

## Error Handling

The package provides detailed error messages for common authentication failures:

### No Authentication Available
```
no valid GitHub authentication found - tried all methods: <last error>
```

### Invalid Token
```
token is invalid or expired
```

### Insufficient Permissions
```
token lacks required permissions
```

### GitHub CLI Not Authenticated
```
GitHub CLI is not authenticated - run 'gh auth login'
```

## Token Requirements

For private repository access, tokens need the following scopes:
- `repo` - Full repository access
- `read:org` - Read organization membership (if repo is in an organization)

## Security Considerations

1. **Token Storage**: Tokens are cached in memory only and are not persisted to disk
2. **Token Validation**: All tokens are validated against GitHub API before use
3. **Scope Checking**: The package validates that tokens have the required permissions
4. **Timeout**: All API calls have reasonable timeouts to prevent hanging

## Testing

The package includes comprehensive tests covering:
- All authentication methods
- Error scenarios
- Token validation
- Caching behavior
- Debug mode functionality

Run tests with:
```bash
go test ./internal/auth/ -v
```

## Configuration

The package respects the following environment variables:
- `GITHUB_TOKEN` - GitHub Personal Access Token
- `GITHUB_PAT` - Alternative name for GitHub token
- `GH_TOKEN` - GitHub CLI style token name
- `PAT` - Generic Personal Access Token name

## Integration

This package is designed to work seamlessly with the lil-whisper update system:
- Automatically used by the `lil-whisper update` command
- Supports both public and private repositories
- Provides detailed feedback for authentication issues
- Gracefully handles authentication failures

## Troubleshooting

### Common Issues

1. **"no valid GitHub authentication found"**
   - Ensure you have set up at least one authentication method
   - Try running with debug mode to see which methods are being tried

2. **"token is invalid or expired"**
   - Check that your GitHub token is valid and hasn't expired
   - Verify the token has the required scopes

3. **"GitHub CLI is not authenticated"**
   - Run `gh auth login` to authenticate with GitHub CLI
   - Verify authentication with `gh auth status`

4. **Private repository access denied**
   - Ensure your token has `repo` scope
   - Verify you have access to the repository

### Debug Mode
Enable debug mode to see detailed authentication attempts:
```bash
./lil-whisper update --debug
```

This will show which authentication methods are being tried and why they succeed or fail.