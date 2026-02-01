#!/usr/bin/env bash
set -euo pipefail

# WhatsApp CLI Installer
# Installs the wa command globally

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
INSTALL_DIR="${HOME}/.local/bin"
SHELL_RC=""

# Detect shell
if [[ -n "${ZSH_VERSION:-}" ]] || [[ "$SHELL" == */zsh ]]; then
    SHELL_RC="$HOME/.zshrc"
elif [[ -n "${BASH_VERSION:-}" ]] || [[ "$SHELL" == */bash ]]; then
    SHELL_RC="$HOME/.bashrc"
fi

echo "WhatsApp CLI Installer"
echo "======================"
echo ""
echo "Bridge directory: $SCRIPT_DIR"
echo "Install directory: $INSTALL_DIR"
echo "Shell config: $SHELL_RC"
echo ""

# Create install directory
mkdir -p "$INSTALL_DIR"

# Build Go bridge
echo "Building WhatsApp bridge..."
cd "$SCRIPT_DIR"
go build -o whatsapp-bridge main.go
echo "✓ Bridge built"

# Create uv environment
echo "Setting up Python environment..."
uv sync 2>/dev/null || uv pip install rich requests
echo "✓ Python dependencies installed"

# Create data directory
DATA_DIR="$HOME/.local/share/wa"
mkdir -p "$DATA_DIR"
echo "✓ Data directory: $DATA_DIR"

# Create wrapper script that works from anywhere
cat > "$INSTALL_DIR/wa" << EOF
#!/usr/bin/env bash
export WA_BRIDGE_DIR="$SCRIPT_DIR"
export WA_DATA_DIR="$DATA_DIR"
cd "$SCRIPT_DIR" && exec uv run wa.py "\$@"
EOF
chmod +x "$INSTALL_DIR/wa"
echo "✓ CLI installed to $INSTALL_DIR/wa"

# Check if ~/.local/bin is in PATH
if [[ ":$PATH:" != *":$INSTALL_DIR:"* ]]; then
    echo ""
    echo "Adding $INSTALL_DIR to PATH..."
    if [[ -n "$SHELL_RC" ]]; then
        echo "" >> "$SHELL_RC"
        echo "# WhatsApp CLI" >> "$SHELL_RC"
        echo "export PATH=\"\$HOME/.local/bin:\$PATH\"" >> "$SHELL_RC"
        echo "✓ Added to $SHELL_RC"
        echo ""
        echo "Run: source $SHELL_RC"
    else
        echo "Add this to your shell config:"
        echo "  export PATH=\"\$HOME/.local/bin:\$PATH\""
    fi
fi

echo ""
echo "Installation complete!"
echo ""
echo "Usage:"
echo "  wa -l              List chats"
echo "  wa \"Hello\"         Send message (select contact with fzf)"
echo "  wa -v              Record and send voice message"
echo "  wa --status        Check if bridge is running"
echo "  wa --help          Show all options"
