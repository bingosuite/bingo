#!/bin/bash
set -e  # Exit on error

echo "🚀 Setting up development environment..."

# Display Go version
echo "📦 Go version:"
go version

# Install Go tools
echo "🔧 Installing Go development tools..."
go install github.com/evilmartians/lefthook@latest
go install golang.org/x/tools/cmd/goimports@latest
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# Install all dependencies
echo "Installing Go dependencies..."
go mod tidy

# Install lefthook git hooks
echo "🪝 Installing lefthook git hooks..."
lefthook install

echo "✅ Setup complete!"
