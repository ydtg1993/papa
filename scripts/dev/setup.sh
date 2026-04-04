#!/bin/bash
# 初始化开发环境
set -e

echo "Installing go dependencies..."
go mod download

echo "Creating logs directory..."
mkdir -p logs downloads

echo "Setting up pre-commit hook..."
cat > .git/hooks/pre-commit << 'EOF'
#!/bin/sh
go fmt ./...
go vet ./...
EOF
chmod +x .git/hooks/pre-commit

echo "Done."