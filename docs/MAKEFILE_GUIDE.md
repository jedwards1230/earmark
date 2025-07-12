# Makefile Guide for Beginners

This guide explains how to use the Makefile in this project, especially if you've never used Make before.

## 🤔 **What is a Makefile?**

A Makefile is a special file that contains a set of rules and commands that automate common development tasks. Instead of remembering and typing long commands, you can use simple `make` commands.

Think of it as a **collection of shortcuts** for building, testing, and managing your project.

## 🚀 **Basic Usage**

### **Running Make Commands**

```bash
# Basic syntax
make <target>

# Examples
make build      # Build the project
make test       # Run tests
make clean      # Clean up build artifacts
make help       # Show all available commands
```

### **Most Common Commands**

```bash
# Just typing 'make' runs the default target (build)
make

# Build the main binary
make build

# Run all tests
make test

# Clean up generated files
make clean

# See all available commands
make help
```

## 📋 **Available Commands Explained**

### **🔨 Building**

| Command | What it does | When to use |
|---------|--------------|-------------|
| `make` or `make build` | Builds the main binary | Most common - when you want to compile your program |
| `make dev` | Same as build (alias) | Clear intent for development builds |
| `make release VERSION=v1.0.0` | Builds with specific version | When creating a release |
| `make build-darwin` | Builds for macOS (both Intel & M1/M2) | When you need platform-specific binaries |

### **🧪 Testing & Quality**

| Command | What it does | When to use |
|---------|--------------|-------------|
| `make test` | Runs all tests | Before committing code changes |
| `make test-coverage` | Tests + HTML coverage report | When you want to see test coverage |
| `make fmt` | Formats your code | Before committing (makes code consistent) |
| `make vet` | Checks for suspicious code | Part of quality assurance |
| `make lint` | Runs advanced code analysis | For high-quality code (requires golangci-lint) |
| `make check` | Runs fmt + vet + lint + test | Before pushing code (does everything) |

### **📦 Installation & Cleanup**

| Command | What it does | When to use |
|---------|--------------|-------------|
| `make install` | Installs binary to /usr/local/bin | When you want to use the tool globally |
| `make clean` | Removes all build artifacts | When you want to start fresh |

### **ℹ️ Information**

| Command | What it does | When to use |
|---------|--------------|-------------|
| `make version` | Shows build information | To check current version/commit |
| `make help` | Shows all available commands | When you forget what's available |

## 💡 **Practical Examples**

### **Daily Development Workflow**

```bash
# 1. Make changes to your code
vim internal/version/version.go

# 2. Build and test
make build
make test

# 3. Run quality checks before committing
make check

# 4. If everything passes, commit your changes
git add .
git commit -m "fix: improve version detection"
```

### **Creating a Release**

```bash
# 1. Build a release version
make release VERSION=v1.2.3

# 2. Test the release binary
./lil-whisper version

# 3. Build for distribution
make build-darwin

# 4. Clean up when done
make clean
```

### **Setting Up for Development**

```bash
# 1. Install development tools (one-time setup)
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# 2. Build and install the tool
make build
make install

# 3. Now you can use it globally
lil-whisper version
```

## 🔧 **Understanding the Makefile Syntax**

### **Variables**
```makefile
VERSION ?= dev           # Default value, can be overridden
COMMIT := $(shell ...)   # Command output

# Usage: make build VERSION=v1.0.0
```

### **Targets**
```makefile
.PHONY: build           # Declares target doesn't create a file
build:                  # Target name
	@echo "Building..."  # @ suppresses command echoing
	go build -o app .    # Actual command to run
```

### **Dependencies**
```makefile
install: build          # 'install' depends on 'build'
	sudo cp app /usr/local/bin/
```

## 🆘 **Troubleshooting**

### **Common Issues**

#### **"make: command not found"**
```bash
# On macOS, install via Xcode Command Line Tools
xcode-select --install

# Or via Homebrew
brew install make
```

#### **"golangci-lint: command not found"**
```bash
# Install the linter
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# Or skip linting
make fmt vet test  # Instead of 'make check'
```

#### **Permission denied during install**
```bash
# The install target requires sudo
make install

# If it fails, try building first
make build
make install
```

#### **Go module issues**
```bash
# Update dependencies
go mod tidy

# Then try building again
make build
```

## 📚 **Learning More**

### **Essential Make Concepts**

1. **Targets**: Named commands you can run (`make build`)
2. **Dependencies**: Targets that must run first (`install: build`)
3. **Variables**: Values you can set and reuse (`VERSION=v1.0.0`)
4. **Phony targets**: Targets that don't create files (`.PHONY: test`)

### **Advanced Usage**

```bash
# Override variables
make build VERSION=v2.0.0 COMMIT=abc123

# Run multiple targets in sequence
make clean build test

# Parallel execution (if targets don't depend on each other)
make -j4 fmt vet  # Run 4 jobs in parallel
```

### **Useful Make Options**

```bash
make -n build     # Show what would be executed (dry run)
make -s build     # Silent mode (suppress command output)
make --help       # Show make's built-in help
```

## 🎯 **Best Practices**

### **For Development**
1. Always run `make test` before committing
2. Use `make check` before pushing to ensure quality
3. Run `make clean` when switching branches or having build issues
4. Use `make help` when you forget available commands

### **For Releases**
1. Use `make release VERSION=vX.Y.Z` for versioned builds
2. Test the release binary before distributing
3. Use `make build-darwin` for distribution binaries

### **For Learning**
1. Start with basic commands: `make`, `make test`, `make clean`
2. Read the help output: `make help`
3. Look at the Makefile itself to understand what commands do
4. Experiment with different targets to see what they do

---

**Remember**: The Makefile is just a collection of shortcuts. You can always run the underlying commands directly if you prefer, but Make makes common tasks much more convenient!