package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

const (
	userJIDSuffix  = "s.whatsapp.net"
	groupJIDSuffix = "g.us"
	defaultPort    = 8080

	mimeJPEG  = "image/jpeg"
	mimePNG   = "image/png"
	mimeGIF   = "image/gif"
	mimeWEBP  = "image/webp"
	mimeOGG   = "audio/ogg; codecs=opus"
	mimeMP4   = "video/mp4"
	mimeAVI   = "video/avi"
	mimeMOV   = "video/quicktime"
	mimeOctet = "application/octet-stream"
)

var startTime = time.Now()

// autoDownload controls whether incoming media is downloaded automatically.
// Disable by setting WA_AUTO_DOWNLOAD=false.
var autoDownload = os.Getenv("WA_AUTO_DOWNLOAD") != "false"

// scheduleMediaDownload downloads media for a stored message in the background.
func scheduleMediaDownload(client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string) {
	go func() {
		// Brief pause so the DB write is committed before we read it back.
		time.Sleep(300 * time.Millisecond)
		_, _, _, path, err := downloadMedia(client, messageStore, messageID, chatJID)
		if err != nil {
			slog.Debug("auto-download failed", "message_id", messageID, "err", err)
		} else {
			slog.Info("auto-downloaded media", "path", path)
		}
	}()
}

// setupLogger configures the global slog logger based on WA_LOG_LEVEL env var.
func setupLogger() {
	level := slog.LevelInfo
	switch strings.ToUpper(os.Getenv("WA_LOG_LEVEL")) {
	case "DEBUG":
		level = slog.LevelDebug
	case "WARN", "WARNING":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

// getDataDir returns the data directory, defaulting to ~/.local/share/wa
func getDataDir() string {
	if dir := os.Getenv("WA_DATA_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "store"
	}
	return filepath.Join(home, ".local", "share", "wa")
}

// parseJID parses a recipient string into a types.JID.
// Strings containing "@" are treated as full JIDs; otherwise as phone numbers.
func parseJID(recipient string) (types.JID, error) {
	if strings.Contains(recipient, "@") {
		return types.ParseJID(recipient)
	}
	return types.JID{User: recipient, Server: userJIDSuffix}, nil
}

// detectMediaType returns the WhatsApp media type and MIME type for a given file path.
func detectMediaType(path string) (whatsmeow.MediaType, string) {
	ext := strings.ToLower(filepath.Ext(path))
	if len(ext) > 1 {
		ext = ext[1:] // strip leading dot
	}
	switch ext {
	case "jpg", "jpeg":
		return whatsmeow.MediaImage, mimeJPEG
	case "png":
		return whatsmeow.MediaImage, mimePNG
	case "gif":
		return whatsmeow.MediaImage, mimeGIF
	case "webp":
		return whatsmeow.MediaImage, mimeWEBP
	case "ogg":
		return whatsmeow.MediaAudio, mimeOGG
	case "mp4":
		return whatsmeow.MediaVideo, mimeMP4
	case "avi":
		return whatsmeow.MediaVideo, mimeAVI
	case "mov":
		return whatsmeow.MediaVideo, mimeMOV
	default:
		return whatsmeow.MediaDocument, mimeOctet
	}
}

// validateMediaPath ensures the resolved absolute path is within the user's home or temp directory.
func validateMediaPath(mediaPath string) error {
	if mediaPath == "" {
		return nil
	}
	abs, err := filepath.Abs(mediaPath)
	if err != nil {
		return fmt.Errorf("invalid media path: %v", err)
	}
	home, _ := os.UserHomeDir()
	tmp := os.TempDir()
	sep := string(filepath.Separator)
	if !strings.HasPrefix(abs, home+sep) &&
		!strings.HasPrefix(abs, tmp+sep) &&
		abs != home {
		return fmt.Errorf("media path must be within home or temp directory")
	}
	return nil
}

// writeJSONError writes a JSON error response with the given HTTP status.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"message": msg,
	})
}

// Message represents a stored chat message.
type Message struct {
	Time      time.Time `json:"timestamp"`
	Sender    string    `json:"sender"`
	Content   string    `json:"content"`
	IsFromMe  bool      `json:"is_from_me"`
	MediaType string    `json:"media_type,omitempty"`
	Filename  string    `json:"filename,omitempty"`
}

// ChatInfo represents a chat with its metadata.
type ChatInfo struct {
	JID             string    `json:"jid"`
	Name            string    `json:"name"`
	LastMessageTime time.Time `json:"last_message_time"`
}

// MessageStore is the SQLite-backed message and chat store.
type MessageStore struct {
	db *sql.DB
}

// NewMessageStore opens (or creates) the messages database and initialises the schema.
func NewMessageStore() (*MessageStore, error) {
	dataDir := getDataDir()

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %v", err)
	}

	dbPath := filepath.Join(dataDir, "messages.db")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);

		CREATE INDEX IF NOT EXISTS idx_messages_chat_timestamp ON messages(chat_jid, timestamp);
		CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	return &MessageStore{db: db}, nil
}

// Close closes the underlying database connection.
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// StoreChat inserts or replaces a chat record.
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, lastMessageTime,
	)
	return err
}

// StoreMessage inserts or replaces a message record. No-ops for empty messages.
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	if content == "" && mediaType == "" {
		return nil
	}

	_, err := store.db.Exec(
		`INSERT OR REPLACE INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
	return err
}

// GetMessages returns the last `limit` messages from a chat, newest first.
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var ts time.Time
		if err := rows.Scan(&msg.Sender, &msg.Content, &ts, &msg.IsFromMe, &msg.MediaType, &msg.Filename); err != nil {
			return nil, err
		}
		msg.Time = ts
		messages = append(messages, msg)
	}

	return messages, nil
}

// GetChats returns all chats sorted by most recent message.
func (store *MessageStore) GetChats() ([]ChatInfo, error) {
	rows, err := store.db.Query("SELECT jid, name, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []ChatInfo
	for rows.Next() {
		var c ChatInfo
		if err := rows.Scan(&c.JID, &c.Name, &c.LastMessageTime); err != nil {
			return nil, err
		}
		chats = append(chats, c)
	}

	return chats, nil
}

// StoreMediaInfo updates the media fields of an existing message row.
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// GetMediaInfo retrieves full media metadata for a message.
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	return mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}

// extractTextContent extracts plain text from a protobuf message.
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}
	if text := msg.GetConversation(); text != "" {
		return text
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	return ""
}

// extractMediaInfo extracts media metadata from a protobuf message.
func extractMediaInfo(msg *waProto.Message) (mediaType string, filename string, url string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + time.Now().Format("20060102_150405") + ".jpg",
			img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + time.Now().Format("20060102_150405") + ".mp4",
			vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + time.Now().Format("20060102_150405") + ".ogg",
			aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		fname := doc.GetFileName()
		if fname == "" {
			fname = "document_" + time.Now().Format("20060102_150405")
		}
		return "document", fname,
			doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}

// SendMessageResponse is the JSON response for the send API.
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// SendMessageRequest is the JSON request body for the send API.
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
}

// sendWhatsAppMessage sends a text or media message to a recipient.
func sendWhatsAppMessage(client *whatsmeow.Client, recipient string, message string, mediaPath string) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	recipientJID, err := parseJID(recipient)
	if err != nil {
		return false, fmt.Sprintf("Error parsing JID: %v", err)
	}

	msg := &waProto.Message{}

	if mediaPath != "" {
		mediaData, err := os.ReadFile(mediaPath)
		if err != nil {
			return false, fmt.Sprintf("Error reading media file: %v", err)
		}

		mediaType, mimeType := detectMediaType(mediaPath)

		resp, err := client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err)
		}

		slog.Debug("media uploaded", "url", resp.URL)

		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waProto.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			var seconds uint32 = 30
			var waveform []byte

			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err)
				}
			} else {
				slog.Debug("not an Ogg Opus file", "mime", mimeType)
			}

			msg.AudioMessage = &waProto.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waProto.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			msg.DocumentMessage = &waProto.DocumentMessage{
				Title:         proto.String(filepath.Base(mediaPath)),
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	_, err = client.SendMessage(context.Background(), recipientJID, msg)
	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err)
	}

	return true, fmt.Sprintf("Message sent to %s", recipient)
}

// handleMessage stores an incoming WhatsApp message.
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	chatJID := msg.Info.Chat.String()
	sender := msg.Info.Sender.User

	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, sender, logger)

	if err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp); err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	content := extractTextContent(msg.Message)
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)

	if content == "" && mediaType == "" {
		return
	}

	err := messageStore.StoreMessage(
		msg.Info.ID, chatJID, sender, content,
		msg.Info.Timestamp, msg.Info.IsFromMe,
		mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
		return
	}

	direction := "recv"
	if msg.Info.IsFromMe {
		direction = "sent"
	}
	if mediaType != "" {
		slog.Info("message", "dir", direction, "sender", sender, "media", mediaType, "file", filename, "caption", content)
		if autoDownload {
			scheduleMediaDownload(client, messageStore, msg.Info.ID, chatJID)
		}
	} else {
		slog.Info("message", "dir", direction, "sender", sender, "content", content)
	}
}

// DownloadMediaRequest is the JSON request body for the download API.
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse is the JSON response for the download API.
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// MediaDownloader implements the whatsmeow.DownloadableMessage interface.
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

func (d *MediaDownloader) GetDirectPath() string             { return d.DirectPath }
func (d *MediaDownloader) GetURL() string                    { return d.URL }
func (d *MediaDownloader) GetMediaKey() []byte               { return d.MediaKey }
func (d *MediaDownloader) GetFileLength() uint64             { return d.FileLength }
func (d *MediaDownloader) GetFileSHA256() []byte             { return d.FileSHA256 }
func (d *MediaDownloader) GetFileEncSHA256() []byte          { return d.FileEncSHA256 }
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType { return d.MediaType }

// downloadMedia downloads media from a stored message to the local filesystem.
func downloadMedia(client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string) (bool, string, string, string, error) {
	chatDir := filepath.Join(getDataDir(), "media", strings.ReplaceAll(chatJID, ":", "_"))

	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err := messageStore.GetMediaInfo(messageID, chatJID)
	if err != nil {
		// Fall back to basic lookup if extended info is unavailable
		var basicType, basicFilename string
		if err2 := messageStore.db.QueryRow(
			"SELECT media_type, filename FROM messages WHERE id = ? AND chat_jid = ?",
			messageID, chatJID,
		).Scan(&basicType, &basicFilename); err2 != nil {
			return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
		}
		mediaType = basicType
		filename = basicFilename
	}

	if mediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	if err := os.MkdirAll(chatDir, 0755); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	localPath := filepath.Join(chatDir, filename)
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to get absolute path: %v", err)
	}

	// Return cached file if it already exists
	if _, err := os.Stat(localPath); err == nil {
		return true, mediaType, filename, absPath, nil
	}

	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}

	slog.Info("downloading media", "message_id", messageID, "chat_jid", chatJID)

	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	downloader := &MediaDownloader{
		URL:           url,
		DirectPath:    extractDirectPathFromURL(url),
		MediaKey:      mediaKey,
		FileLength:    fileLength,
		FileSHA256:    fileSHA256,
		FileEncSHA256: fileEncSHA256,
		MediaType:     waMediaType,
	}

	mediaData, err := client.Download(context.Background(), downloader)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to download media: %v", err)
	}

	if err := os.WriteFile(localPath, mediaData, 0644); err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}

	slog.Info("media downloaded", "path", absPath, "bytes", len(mediaData))
	return true, mediaType, filename, absPath, nil
}

// extractDirectPathFromURL extracts the direct path component from a WhatsApp media URL.
func extractDirectPathFromURL(url string) string {
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return url
	}
	return "/" + strings.SplitN(parts[1], "?", 2)[0]
}

// startRESTServer registers all API handlers and starts the HTTP server.
// Returns the server instance for graceful shutdown.
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, port int) *http.Server {
	mux := http.NewServeMux()

	// POST /api/send
	mux.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request format")
			return
		}
		if req.Recipient == "" {
			writeJSONError(w, http.StatusBadRequest, "recipient is required")
			return
		}
		if req.Message == "" && req.MediaPath == "" {
			writeJSONError(w, http.StatusBadRequest, "message or media_path is required")
			return
		}
		if err := validateMediaPath(req.MediaPath); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		slog.Debug("send request", "recipient", req.Recipient, "has_media", req.MediaPath != "")

		success, message := sendWhatsAppMessage(client, req.Recipient, req.Message, req.MediaPath)

		w.Header().Set("Content-Type", "application/json")
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		_ = json.NewEncoder(w).Encode(SendMessageResponse{Success: success, Message: message})
	})

	// POST /api/download
	mux.HandleFunc("/api/download", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request format")
			return
		}
		if req.MessageID == "" || req.ChatJID == "" {
			writeJSONError(w, http.StatusBadRequest, "message_id and chat_jid are required")
			return
		}

		success, mediaType, filename, path, err := downloadMedia(client, messageStore, req.MessageID, req.ChatJID)
		if err != nil {
			status := http.StatusInternalServerError
			msg := err.Error()
			if strings.Contains(msg, "failed to find message") || strings.Contains(msg, "not a media message") {
				status = http.StatusNotFound
			}
			writeJSONError(w, status, msg)
			return
		}
		if !success {
			writeJSONError(w, http.StatusInternalServerError, "download failed")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	})

	// GET /api/chats
	mux.HandleFunc("/api/chats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		chats, err := messageStore.GetChats()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get chats: %v", err))
			return
		}
		if chats == nil {
			chats = []ChatInfo{}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"chats":   chats,
		})
	})

	// GET /api/messages?chat_jid=...&limit=...
	mux.HandleFunc("/api/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		chatJID := r.URL.Query().Get("chat_jid")
		if chatJID == "" {
			writeJSONError(w, http.StatusBadRequest, "chat_jid query parameter is required")
			return
		}

		limit := 50
		if ls := r.URL.Query().Get("limit"); ls != "" {
			if n, err := strconv.Atoi(ls); err == nil && n > 0 {
				limit = n
			}
		}

		messages, err := messageStore.GetMessages(chatJID, limit)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get messages: %v", err))
			return
		}
		if messages == nil {
			messages = []Message{}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  true,
			"messages": messages,
		})
	})

	// GET /api/health
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		connected := client.IsConnected()
		status := "ok"
		if !connected {
			status = "disconnected"
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"status":    status,
			"connected": connected,
			"uptime":    time.Since(startTime).String(),
		})
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	slog.Info("starting REST API server", "addr", srv.Addr)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("REST API server error", "err", err)
		}
	}()

	return srv
}

func main() {
	setupLogger()

	logger := waLog.Stdout("Client", "INFO", true)
	dbLog := waLog.Stdout("Database", "INFO", true)

	slog.Info("starting WhatsApp bridge")

	dataDir := getDataDir()
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		slog.Error("failed to create data directory", "err", err)
		return
	}
	slog.Info("using data directory", "path", dataDir)

	waDbPath := filepath.Join(dataDir, "whatsapp.db")
	container, err := sqlstore.New(context.Background(), "sqlite3", "file:"+waDbPath+"?_foreign_keys=on", dbLog)
	if err != nil {
		slog.Error("failed to connect to database", "err", err)
		return
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			deviceStore = container.NewDevice()
			slog.Info("created new device")
		} else {
			slog.Error("failed to get device", "err", err)
			return
		}
	}

	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		slog.Error("failed to create WhatsApp client")
		return
	}

	messageStore, err := NewMessageStore()
	if err != nil {
		slog.Error("failed to initialize message store", "err", err)
		return
	}
	defer messageStore.Close()

	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			handleMessage(client, messageStore, v, logger)
		case *events.HistorySync:
			handleHistorySync(client, messageStore, v, logger)
		case *events.Connected:
			slog.Info("connected to WhatsApp")
		case *events.LoggedOut:
			slog.Warn("device logged out, please scan QR code to log in again")
		}
	})

	connected := make(chan bool, 1)

	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			slog.Error("failed to connect", "err", err)
			return
		}

		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				connected <- true
				break
			}
		}

		select {
		case <-connected:
			slog.Info("successfully connected and authenticated")
		case <-time.After(3 * time.Minute):
			slog.Error("timeout waiting for QR code scan")
			return
		}
	} else {
		err = client.Connect()
		if err != nil {
			slog.Error("failed to connect", "err", err)
			return
		}
		connected <- true
	}

	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		slog.Error("failed to establish stable connection")
		return
	}

	slog.Info("connected to WhatsApp")

	srv := startRESTServer(client, messageStore, defaultPort)

	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	slog.Info("press Ctrl+C to disconnect and exit")
	<-exitChan

	slog.Info("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown error", "err", err)
	}

	client.Disconnect()
	slog.Info("disconnected")
}

// GetChatName determines the display name for a chat.
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	var existingName string
	if err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName); err == nil && existingName != "" {
		return existingName
	}

	var name string

	if jid.Server == groupJIDSuffix {
		if conversation != nil {
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()
				if f := v.FieldByName("DisplayName"); f.IsValid() && f.Kind() == reflect.Ptr && !f.IsNil() {
					if dn := f.Elem().String(); dn != "" {
						name = dn
					}
				}
				if name == "" {
					if f := v.FieldByName("Name"); f.IsValid() && f.Kind() == reflect.Ptr && !f.IsNil() {
						name = f.Elem().String()
					}
				}
			}
		}

		if name == "" {
			if groupInfo, err := client.GetGroupInfo(context.Background(), jid); err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}
	} else {
		if contact, err := client.Store.Contacts.GetContact(context.Background(), jid); err == nil && contact.FullName != "" {
			name = contact.FullName
		} else if sender != "" {
			name = sender
		} else {
			name = jid.User
		}
	}

	return name
}

// handleHistorySync processes WhatsApp history sync events.
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	slog.Info("received history sync", "conversations", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}

		name := GetChatName(client, messageStore, jid, chatJID, conversation, "", logger)

		messages := conversation.Messages
		if len(messages) == 0 {
			continue
		}

		latestMsg := messages[0]
		if latestMsg == nil || latestMsg.Message == nil {
			continue
		}

		latestTS := latestMsg.Message.GetMessageTimestamp()
		if latestTS == 0 {
			continue
		}
		messageStore.StoreChat(chatJID, name, time.Unix(int64(latestTS), 0))

		for _, msg := range messages {
			if msg == nil || msg.Message == nil {
				continue
			}

			var content string
			if msg.Message.Message != nil {
				content = extractTextContent(msg.Message.Message)
			}

			var mediaType, filename, url string
			var mediaKey, fileSHA256, fileEncSHA256 []byte
			var fileLength uint64
			if msg.Message.Message != nil {
				mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
			}

			if content == "" && mediaType == "" {
				continue
			}

			var sender string
			isFromMe := false
			if msg.Message.Key != nil {
				if msg.Message.Key.FromMe != nil {
					isFromMe = *msg.Message.Key.FromMe
				}
				if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
					sender = *msg.Message.Key.Participant
				} else if isFromMe {
					sender = client.Store.ID.User
				} else {
					sender = jid.User
				}
			} else {
				sender = jid.User
			}

			msgID := ""
			if msg.Message.Key != nil && msg.Message.Key.ID != nil {
				msgID = *msg.Message.Key.ID
			}

			ts := msg.Message.GetMessageTimestamp()
			if ts == 0 {
				continue
			}

			msgTime := time.Unix(int64(ts), 0)
			err = messageStore.StoreMessage(
				msgID, chatJID, sender, content, msgTime, isFromMe,
				mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength,
			)
			if err != nil {
				logger.Warnf("Failed to store history message: %v", err)
			} else {
				syncedCount++
				// Only auto-download recent media — older URLs are typically expired.
				if autoDownload && mediaType != "" && time.Since(msgTime) < 7*24*time.Hour {
					scheduleMediaDownload(client, messageStore, msgID, chatJID)
				}
			}
		}
	}

	slog.Info("history sync complete", "stored", syncedCount)
}

// requestHistorySync sends a history sync request to the WhatsApp server.
func requestHistorySync(client *whatsmeow.Client) {
	if client == nil {
		slog.Error("client is not initialized")
		return
	}
	if !client.IsConnected() {
		slog.Error("client is not connected")
		return
	}
	if client.Store.ID == nil {
		slog.Error("client is not logged in")
		return
	}

	historyMsg := client.BuildHistorySyncRequest(nil, 100)
	if historyMsg == nil {
		slog.Error("failed to build history sync request")
		return
	}

	_, err := client.SendMessage(context.Background(), types.JID{
		Server: userJIDSuffix,
		User:   "status",
	}, historyMsg)
	if err != nil {
		slog.Error("failed to request history sync", "err", err)
	} else {
		slog.Info("history sync requested")
	}
}

// analyzeOggOpus extracts the duration and generates a synthetic waveform from an Ogg Opus file.
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	var lastGranule uint64
	var sampleRate uint32 = 48000
	var preSkip uint16
	var foundOpusHead bool

	for i := 0; i < len(data); {
		if i+27 >= len(data) {
			break
		}
		if string(data[i:i+4]) != "OggS" {
			i++
			continue
		}

		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}

		if !foundOpusHead && pageSeqNum <= 1 {
			pageData := data[i : i+pageSize]
			if headPos := bytes.Index(pageData, []byte("OpusHead")); headPos >= 0 {
				headPos += 8 // skip "OpusHead" marker
				if headPos+16 <= len(pageData) {
					preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
					sampleRate = binary.LittleEndian.Uint32(pageData[headPos+12 : headPos+16])
					foundOpusHead = true
					slog.Debug("found OpusHead", "sample_rate", sampleRate, "pre_skip", preSkip)
				}
			}
		}

		if granulePos != 0 {
			lastGranule = granulePos
		}
		i += pageSize
	}

	if !foundOpusHead {
		slog.Warn("OpusHead not found, using default values")
	}

	if lastGranule > 0 {
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
		slog.Debug("opus duration calculated", "seconds", durationSeconds)
	} else {
		slog.Warn("no valid granule position found, using estimation")
		duration = uint32(float64(len(data)) / 2000.0)
	}

	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	waveform = placeholderWaveform(duration)
	slog.Debug("ogg opus analysis complete", "size_bytes", len(data), "duration_s", duration)
	return duration, waveform, nil
}

// min returns the smaller of x or y.
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// placeholderWaveform generates a synthetic 64-byte waveform for WhatsApp voice messages.
func placeholderWaveform(duration uint32) []byte {
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	rand.Seed(int64(duration)) //nolint:staticcheck

	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		pos := float64(i) / float64(waveformLength)

		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)
		val += (rand.Float64() - 0.5) * 15
		val = val * (0.7 + 0.3*math.Sin(pos*math.Pi))
		val += 50

		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}
		waveform[i] = byte(val)
	}

	return waveform
}
