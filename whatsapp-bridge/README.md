# WhatsApp Bridge

Go-Anwendung, die via [whatsmeow](https://github.com/tulir/whatsmeow) eine Verbindung zur WhatsApp Web API herstellt und eine REST API für den MCP-Server bereitstellt.

## Voraussetzungen

- Go 1.21+
- Python 3.12+ mit [uv](https://astral.sh/uv)
- `fzf` (für den `wa`-CLI) — `brew install fzf`
- `ffmpeg` (optional, für Sprachnachrichten) — `brew install ffmpeg`

## Installation

```bash
# Go-Bridge bauen
go build -o whatsapp-bridge main.go

# wa-CLI installieren
uv tool install --force .
```

Beim ersten Start wird ein QR-Code angezeigt, der mit der WhatsApp-App gescannt werden muss.

## Datenhaltung

Alle Daten liegen unter `~/.local/share/wa/` (überschreibbar via `WA_DATA_DIR`):

| Datei | Inhalt |
|---|---|
| `whatsapp.db` | WhatsApp-Session (Geräteschlüssel) |
| `messages.db` | Nachrichtenhistorie und Chatmetadaten |
| `bridge.log` | Logausgaben der Bridge |
| `media/` | Heruntergeladene Mediendateien |

## Bridge starten

```bash
# Direkt
./whatsapp-bridge

# Im Hintergrund (mit Monitor)
./wa-monitor.sh &
./whatsapp-bridge >> ~/.local/share/wa/bridge.log 2>&1 &
```

Der `wa-monitor.sh` überwacht das Log und zeigt eine macOS-Benachrichtigung, wenn die Session abläuft.

## Umgebungsvariablen

| Variable | Standard | Beschreibung |
|---|---|---|
| `WA_DATA_DIR` | `~/.local/share/wa` | Datenverzeichnis |
| `WA_LOG_LEVEL` | `INFO` | Log-Level: `DEBUG`, `INFO`, `WARN`, `ERROR` |

## REST API

Die Bridge läuft auf Port `8080`.

### `GET /api/health`

Verbindungsstatus und Uptime.

```json
{ "success": true, "status": "ok", "connected": true, "uptime": "2h30m" }
```

### `POST /api/send`

Nachricht oder Datei senden.

```json
{ "recipient": "4912345678", "message": "Hallo!", "media_path": "/pfad/zur/datei.jpg" }
```

`recipient` kann eine Telefonnummer oder eine vollständige JID (`49123@s.whatsapp.net`, `gruppe@g.us`) sein.

### `POST /api/download`

Media aus einer gespeicherten Nachricht herunterladen.

```json
{ "message_id": "ABC123", "chat_jid": "49123@s.whatsapp.net" }
```

### `GET /api/chats`

Alle Chats, sortiert nach letzter Nachricht.

```json
{ "success": true, "chats": [{ "jid": "...", "name": "...", "last_message_time": "..." }] }
```

### `GET /api/messages?chat_jid=...&limit=50`

Nachrichten eines Chats (Standard: 50, neueste zuerst).

```json
{ "success": true, "messages": [{ "timestamp": "...", "sender": "...", "content": "..." }] }
```

## wa CLI

Das `wa`-Tool ermöglicht das Senden von Nachrichten direkt aus dem Terminal.

```bash
wa                        # Kontakt via fzf wählen, Nachricht eingeben
wa "Hallo!"               # Kontakt wählen, Nachricht direkt übergeben
wa -t 4912345678 "Hi"     # An Nummer senden
wa -m foto.jpg "Schau"    # Mit Dateianhang
wa -v                     # Sprachnachricht aufnehmen und senden
wa -l                     # Alle Chats auflisten
wa --status               # Bridge-Status prüfen
wa --start                # Bridge im Hintergrund starten
wa --stop                 # Bridge stoppen
```

## MCP-Server (Claude Desktop)

Die Konfiguration in `~/Library/Application Support/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "uv",
      "args": ["--directory", "/pfad/zu/whatsapp-mcp/whatsapp-mcp-server", "run", "main.py"]
    }
  }
}
```

## Troubleshooting

**Session abgelaufen (ca. alle 20 Tage):** Bridge neu starten — ein neuer QR-Code erscheint.

**Nachrichten fehlen:** Nach dem ersten Login dauert es einige Minuten, bis die Historien-Synchronisation abgeschlossen ist.

**Datenbank zurücksetzen:**
```bash
rm ~/.local/share/wa/messages.db ~/.local/share/wa/whatsapp.db
./whatsapp-bridge  # QR-Code neu scannen
```

**Windows:** CGO muss aktiviert sein (`go env -w CGO_ENABLED=1`) und ein C-Compiler (z.B. via MSYS2) muss installiert sein.
