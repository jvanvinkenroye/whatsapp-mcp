# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is the WhatsApp Bridge component of a WhatsApp MCP (Model Context Protocol) Server. It's a Go 1.25 application that connects directly to WhatsApp's web multidevice API using the whatsmeow library to bridge WhatsApp messages with an MCP server.

## Common Development Commands

### Go Bridge (whatsapp-bridge/)

```bash
# Run the WhatsApp bridge
go run main.go

# Build the application
go build -o whatsapp-bridge main.go

# Format code
go fmt ./...

# Vet code for common issues
go vet ./...

# Run tests
go test ./...

# Update dependencies
go mod tidy

# Download dependencies
go get
```

### Python MCP Server (../whatsapp-mcp-server/)

```bash
# Run the MCP server (from whatsapp-mcp-server directory)
uv run main.py

# Install dependencies
uv sync

# Add new dependencies
uv add package-name
```

## Architecture

### Core Components

1. **WhatsApp Bridge (Go)**: Single-file application (`main.go`) that:
   - Connects to WhatsApp Web API via whatsmeow library
   - Handles QR code authentication for device pairing
   - Stores message history and media metadata in SQLite
   - Provides REST API endpoints for the MCP server
   - Manages media upload/download functionality

2. **Message Store**: SQLite database with two main tables:
   - `chats`: Chat metadata (JID, name, last message time)
   - `messages`: Message content, media info, timestamps, sender data

3. **REST API**: Runs on port 8080 with endpoints:
   - `/api/send`: Send messages (text and media)
   - `/api/download`: Download media files

### Key Data Structures

- `Message`: Represents chat messages with media support
- `MessageStore`: SQLite database abstraction
- `SendMessageRequest`/`DownloadMediaRequest`: API request structures
- `MediaDownloader`: Implements whatsmeow's DownloadableMessage interface

### Authentication Flow

- First run requires QR code scan via terminal
- Session data stored in SQLite (`store/whatsapp.db`)
- Automatic reconnection on subsequent runs
- Re-authentication needed approximately every 20 days

## Important Implementation Details

### Media Handling

- Supports images, videos, audio (Ogg Opus), and documents
- Media files are stored remotely by WhatsApp
- Local database stores metadata (URLs, keys, hashes)
- Download requires message ID and chat JID
- Audio files get synthetic waveform generation for voice messages

### Database Schema

Messages table includes comprehensive media fields:
- `media_key`, `file_sha256`, `file_enc_sha256`: Encryption data
- `url`, `filename`, `file_length`: Media metadata
- `media_type`: image/video/audio/document classification

### Error Handling

- Graceful handling of connection failures
- SQLite foreign key constraints enabled
- Media download validation with fallback mechanisms
- QR code timeout protection (3 minute limit)

### Windows Compatibility

Requires CGO enabled and C compiler (MSYS2 recommended) for SQLite support.

## Development Notes

- Single Go module with minimal external dependencies
- Uses whatsmeow for WhatsApp protocol implementation
- SQLite for local data persistence
- HTTP server for MCP integration
- Designed to run continuously as a service