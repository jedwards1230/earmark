# GitHub Actions Setup Guide

This document outlines the prerequisites and setup required for the GitHub Actions workflows in this repository.

## 🔧 **Built-in Requirements (No Setup Needed)**

The following are automatically provided by GitHub and require no additional setup:

### Secrets
- **`GITHUB_TOKEN`**: Automatically provided by GitHub Actions
  - Used for: Creating releases, pushing tags, accessing repository
  - Permissions: Configured in workflow files

### GitHub Actions Marketplace Actions
All actions used are from the official GitHub Actions marketplace and require no setup:

- **actions/checkout@v4**: Repository checkout
- **actions/setup-go@v5**: Go language setup  
- **actions/cache@v4**: Build cache management
- **actions/upload-artifact@v4**: Build artifact uploads
- **softprops/action-gh-release@v2**: GitHub release creation

## 🛡️ **Repository Permissions**

Ensure your repository has the following settings:

### Workflow Permissions
In **Settings → Actions → General → Workflow permissions**:
- ✅ **"Read and write permissions"** (required for release workflow)
- ✅ **"Allow GitHub Actions to create and approve pull requests"**

### Repository Settings
In **Settings → General**:
- ✅ **Issues**: Enabled (for bug reports)
- ✅ **Wiki**: Optional
- ✅ **Discussions**: Optional but recommended

## 📊 **Optional Third-Party Services**

### Code Coverage (Optional)
The CI workflow includes Codecov integration:

- **Service**: [Codecov.io](https://codecov.io)
- **Setup**: 
  1. Sign up at codecov.io with your GitHub account
  2. Add your repository to Codecov
  3. No additional secrets needed (uses GITHUB_TOKEN)
- **Benefits**: Coverage reporting and PR comments
- **Cost**: Free for public repositories

### Security Scanning (Included)
The CI workflow includes security scanning:

- **Gosec**: Static security analysis (no setup required)
- **govulncheck**: Vulnerability database scanning (no setup required)

## 🚀 **Workflow Triggers**

### CI Workflow (`ci.yml`)
- **Triggers**: Push to `main`/`develop`, Pull Requests to `main`
- **Purpose**: Testing, linting, building, security scanning
- **Requirements**: None (fully automated)

### Release Workflow (`release.yml`)
- **Trigger**: Manual dispatch only (`workflow_dispatch`)
- **Purpose**: Version bumping, release creation, binary building
- **Requirements**: Write permissions (see above)

## 🏗️ **Build Requirements**

### Platform Constraints
- **Target**: macOS only (darwin/amd64, darwin/arm64)
- **Reason**: Yap speech recognition requires macOS hardware
- **CI Runner**: Uses `ubuntu-latest` (cross-compilation)

### Go Version
- **Version**: Go 1.24+ (configured in workflows)
- **Modules**: All dependencies in go.mod/go.sum

## 🎯 **Manual Release Process**

To create a release:

1. **Navigate**: Go to Actions → Release workflow
2. **Trigger**: Click "Run workflow"
3. **Configure**:
   - **Branch**: Usually `main`
   - **Version Type**: `major`, `minor`, or `patch`
   - **Custom Version**: Optional override (e.g., `v1.0.0`)
   - **Draft**: Create as draft release
   - **Prerelease**: Mark as prerelease
4. **Execute**: Click "Run workflow"

The workflow will:
- ✅ Calculate new version number
- ✅ Generate changelog from commits
- ✅ Create and push git tag
- ✅ Build macOS binaries (amd64 + arm64)
- ✅ Create GitHub release with binaries
- ✅ Update README version badge (if present)

## 🔍 **Troubleshooting**

### Common Issues

#### Permission Denied Errors
**Symptoms**: Workflow fails with permission errors
**Solution**: Check repository workflow permissions (see above)

#### Build Failures
**Symptoms**: Go build or test failures
**Solution**: 
- Ensure code compiles locally with `make build`
- Check Go version compatibility
- Verify all tests pass with `go test ./...`

#### Release Creation Fails
**Symptoms**: Release workflow succeeds but no release created
**Solution**:
- Check repository write permissions
- Verify GITHUB_TOKEN has sufficient scope
- Ensure tag doesn't already exist

#### Cross-Compilation Issues
**Symptoms**: Builds fail for darwin targets
**Solution**:
- This is expected - the application requires macOS runtime
- CI builds are for validation only
- Actual releases should be built/tested on macOS

### Getting Help

1. **Check workflow logs**: Actions tab → Failed workflow → Expand steps
2. **Review this documentation**: Ensure all requirements are met
3. **Test locally**: Verify builds work with `make build` and `make test`
4. **GitHub Issues**: Create issue with workflow logs and error details

## 📚 **Additional Resources**

- [GitHub Actions Documentation](https://docs.github.com/en/actions)
- [Go Setup Action](https://github.com/actions/setup-go)
- [Checkout Action](https://github.com/actions/checkout)
- [Release Action](https://github.com/softprops/action-gh-release)

---

**Note**: All workflows are designed to work with minimal setup. The only requirement is proper repository permissions for the release workflow.