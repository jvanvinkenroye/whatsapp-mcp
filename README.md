# WhatsApp MCP Server

A Model Context Protocol (MCP) server for WhatsApp that lets you search and read messages (including images, videos, documents, and audio), search contacts, and send messages or media to individuals or groups.


> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). Prompt injection could lead to private data exfiltration.

## Architecture

```
Claude Desktop / Cursor
        |
  Python MCP Server  (whatsapp-mcp-server/)
        |
  Go WhatsApp Bridge (whatsapp-bridge/)   <-->  WhatsApp Web API
        |
   SQLite (~/.local/share/wa/)
```

| Component | Description |
|---|---|
| **Go Bridge** | Connects to WhatsApp, handles auth, stores messages in SQLite, exposes REST API on port 8080 |
| **Python MCP Server** | Implements MCP protocol, provides tools to Claude, reads SQLite directly or calls bridge API |
| **wa CLI** | Terminal tool for sending/reading messages without an LLM |

### Data Storage

All data is stored under `~/.local/share/wa/` (configurable via `WA_DATA_DIR`):

| File | Content |
|---|---|
| `whatsapp.db` | WhatsApp session (device keys) |
| `messages.db` | Message history and chat metadata |
| `bridge.log` | Bridge log output |
| `media/` | Downloaded media files |

## Prerequisites

- Go 1.21+
- Python 3.12+
- [uv](https://astral.sh/uv) — `curl -LsSf https://astral.sh/uv/install.sh | sh`
- `fzf` — `brew install fzf` (required for `wa` CLI)
- `ffmpeg` _(optional)_ — `brew install ffmpeg` (for sending voice messages)

## Installation

### 1. Clone

```bash
git clone https://github.com/jvanvinkenroye/whatsapp-mcp.git
cd whatsapp-mcp
```

### 2. Build and start the bridge

```bash
cd whatsapp-bridge
go build -o whatsapp-bridge main.go
./whatsapp-bridge
```

On first run, scan the QR code with your WhatsApp mobile app (Settings > Linked Devices > Link a Device). Sessions last approximately 20 days before re-authentication is required.

To run in the background:

```bash
nohup ./whatsapp-bridge >> ~/.local/share/wa/bridge.log 2>&1 &
```

### 3. Install the `wa` CLI (optional)

```bash
cd whatsapp-bridge
uv tool install --force .
```

### 4. Configure Claude Desktop or Cursor

**Claude Desktop** — edit `~/Library/Application Support/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "uv",
      "args": [
        "--directory",
        "/path/to/whatsapp-mcp/whatsapp-mcp-server",
        "run",
        "main.py"
      ]
    }
  }
}
```

**Cursor** — edit `~/.cursor/mcp.json` with the same content.

Replace `/path/to/whatsapp-mcp` with the output of `pwd` from inside the cloned repo.

Restart Claude Desktop or Cursor after saving.

## `wa` CLI

Send and read messages directly from the terminal:

```bash
wa                        # pick contact via fzf, type message
wa "Hello!"               # pick contact, send message directly
wa -t 4912345678 "Hi"     # send to number
wa -m photo.jpg "Look"    # send with attachment
wa -v                     # record and send voice message
wa -r                     # read messages from a chat
wa -l                     # list all chats
wa --status               # check bridge status
wa --start                # start bridge in background
wa --stop                 # stop bridge
```

## MCP Tools

Claude can access the following tools:

| Tool | Description |
|---|---|
| `search_contacts` | Search contacts by name or phone number |
| `list_chats` | List chats with metadata |
| `list_messages` | Retrieve messages with filters |
| `get_chat` | Get info about a specific chat |
| `get_direct_chat_by_contact` | Find direct chat with a contact |
| `get_contact_chats` | List all chats involving a contact |
| `get_last_interaction` | Most recent message with a contact |
| `get_message_context` | Context around a specific message |
| `send_message` | Send text to a phone number or group JID |
| `send_file` | Send image, video, document, or raw audio |
| `send_audio_message` | Send audio as a WhatsApp voice message |
| `download_media` | Download media from a message, returns local path |

## Media Handling

**Automatic download:** The bridge automatically downloads incoming media (images, videos, documents, audio) to `~/.local/share/wa/media/`. This can be disabled by setting `WA_AUTO_DOWNLOAD=false`.

**Sending voice messages:** Audio files should be `.ogg` Opus format. With FFmpeg installed, other formats (MP3, WAV, etc.) are converted automatically.

**Manual download:** Use the `download_media` MCP tool with the `message_id` and `chat_jid` from a message to download media on demand.

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `WA_DATA_DIR` | `~/.local/share/wa` | Data directory |
| `WA_LOG_LEVEL` | `INFO` | Log level: `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `WA_AUTO_DOWNLOAD` | `true` | Auto-download incoming media (`false` to disable) |

## Troubleshooting

**Session expired (~every 20 days):** Restart the bridge — a new QR code will appear.

**Messages not loading:** After first login, history sync can take a few minutes.

**Multiple bridge instances:** Only one instance should run at a time. Check with `pgrep whatsapp-bridge` and kill extras with `pkill whatsapp-bridge`.

**Reset everything:**
```bash
rm ~/.local/share/wa/messages.db ~/.local/share/wa/whatsapp.db
./whatsapp-bridge  # scan QR code again
```

**Windows:** CGO must be enabled and a C compiler installed (e.g. via [MSYS2](https://www.msys2.org/)):
```bash
go env -w CGO_ENABLED=1
go run main.go
```

For Claude Desktop integration issues, see the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues).
