package server

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/gofiber/websocket/v2"
)

// Message types from client
type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type OpenFilePayload struct {
	Path string `json:"path"`
}

type ConfigureLSPPayload struct {
	Language            string `json:"language"`
	ServerPath          string `json:"serverPath"`
	CompileCommandsDir  string `json:"compileCommandsDir"`
}

type DeltaPayload struct {
	FromPos int    `json:"fromPos"`
	ToPos   int    `json:"toPos"`
	Insert  string `json:"insert"`
}

type SavePayload struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type LSPRequestPayload struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// HandleWebSocket handles WebSocket connections
func HandleWebSocket(c *websocket.Conn) {
	lspManager := c.Locals("lspManager").(*MultiLSPManager)

	var currentFile string
	var currentContent string
	var mu sync.Mutex

	// Send LSP notifications to client
	go func() {
		notifChan := lspManager.GetNotificationChan()
		for notification := range notifChan {
			// Unmarshal the notification so it gets sent as an object, not raw bytes
			var notifObj interface{}
			if err := json.Unmarshal(notification, &notifObj); err != nil {
				log.Printf("Failed to unmarshal notification: %v", err)
				continue
			}

			response := map[string]interface{}{
				"type":    "lsp_notification",
				"payload": notifObj,
			}
			if err := c.WriteJSON(response); err != nil {
				return
			}
		}
	}()

	for {
		var msg Message
		if err := c.ReadJSON(&msg); err != nil {
			log.Printf("WebSocket read error: %v", err)
			break
		}

		log.Printf("DEBUG: Received message type: %s", msg.Type)

		switch msg.Type {
		case "open_file":
			var payload OpenFilePayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				log.Printf("ERROR: Invalid open_file payload: %v", err)
				sendError(c, "Invalid open_file payload")
				continue
			}

			log.Printf("DEBUG: Opening file: %s", payload.Path)
			content, err := ReadFile(payload.Path)
			if err != nil {
				log.Printf("ERROR: Failed to read file %s: %v", payload.Path, err)
				sendError(c, "Failed to read file: "+err.Error())
				continue
			}

			log.Printf("DEBUG: File read successfully, length: %d bytes", len(content))
			mu.Lock()
			currentFile = payload.Path
			currentContent = content
			mu.Unlock()

			response := map[string]interface{}{
				"type": "file_opened",
				"payload": map[string]string{
					"path":    payload.Path,
					"content": content,
				},
			}
			log.Printf("DEBUG: Sending file_opened response")
			c.WriteJSON(response)
			log.Printf("DEBUG: file_opened response sent")

			// Notify LSP about opened file
			if err := lspManager.RouteNotification("textDocument/didOpen", map[string]interface{}{
				"textDocument": map[string]interface{}{
					"uri":        "file://" + payload.Path,
					"languageId": detectLanguage(payload.Path),
					"version":    1,
					"text":       content,
				},
			}); err != nil {
				log.Printf("Warning: Failed to notify LSP about opened file: %v", err)
			}

		case "configure_lsp":
			var payload ConfigureLSPPayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				sendError(c, "Invalid configure_lsp payload")
				continue
			}

			language := payload.Language
			if language == "" {
				sendError(c, "Language is required")
				continue
			}

			serverPath := payload.ServerPath
			if serverPath == "" {
				// Set default server paths
				if language == "cpp" {
					serverPath = "clangd"
				} else if language == "python" {
					serverPath = "pylsp"
				} else {
					sendError(c, "Unknown language: "+language)
					continue
				}
			}

			// Start the LSP server
			if err := lspManager.StartLSP(language, serverPath, payload.CompileCommandsDir); err != nil {
				sendError(c, "Failed to start LSP: "+err.Error())
				continue
			}

			// Initialize LSP
			rootDir := payload.CompileCommandsDir
			if rootDir == "" {
				rootDir = "/"
			}
			if err := lspManager.InitializeLSP(language, rootDir); err != nil {
				sendError(c, "Failed to initialize LSP: "+err.Error())
				continue
			}

			response := map[string]interface{}{
				"type": "lsp_configured",
				"payload": map[string]interface{}{
					"success":  true,
					"language": language,
				},
			}
			c.WriteJSON(response)

		case "delta":
			var payload DeltaPayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				sendError(c, "Invalid delta payload")
				continue
			}

			mu.Lock()
			newContent, err := ApplyDelta(currentContent, payload.FromPos, payload.ToPos, payload.Insert)
			if err != nil {
				mu.Unlock()
				sendError(c, "Failed to apply delta: "+err.Error())
				continue
			}
			currentContent = newContent
			mu.Unlock()

			// Notify LSP about change
			if err := lspManager.RouteNotification("textDocument/didChange", map[string]interface{}{
				"textDocument": map[string]interface{}{
					"uri":     "file://" + currentFile,
					"version": 1,
				},
				"contentChanges": []interface{}{
					map[string]interface{}{
						"text": currentContent,
					},
				},
			}); err != nil {
				log.Printf("Warning: Failed to notify LSP about change: %v", err)
			}

		case "save":
			var payload SavePayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				sendError(c, "Invalid save payload")
				continue
			}

			if err := WriteFile(payload.Path, payload.Content); err != nil {
				sendError(c, "Failed to save file: "+err.Error())
				continue
			}

			response := map[string]interface{}{
				"type": "file_saved",
				"payload": map[string]bool{
					"success": true,
				},
			}
			c.WriteJSON(response)

			// Notify LSP about save
			if err := lspManager.RouteNotification("textDocument/didSave", map[string]interface{}{
				"textDocument": map[string]interface{}{
					"uri": "file://" + payload.Path,
				},
			}); err != nil {
				log.Printf("Warning: Failed to notify LSP about save: %v", err)
			}

		case "lsp_request":
			var payload LSPRequestPayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				sendError(c, "Invalid lsp_request payload")
				continue
			}

			var params interface{}
			if len(payload.Params) > 0 {
				json.Unmarshal(payload.Params, &params)
			}

			result, err := lspManager.RouteRequest(payload.Method, params)
			if err != nil {
				sendError(c, "LSP request failed: "+err.Error())
				continue
			}

			// Return response with the client's original ID
			// Parse the LSP result to get the actual completion data
			var lspResponse map[string]interface{}
			json.Unmarshal(result, &lspResponse)

			response := map[string]interface{}{
				"type": "lsp_response",
				"payload": map[string]interface{}{
					"id":      payload.ID,  // Use client's ID
					"jsonrpc": "2.0",
					"result":  lspResponse["result"],  // Extract just the result, not the whole LSP response
				},
			}
			c.WriteJSON(response)

		default:
			sendError(c, "Unknown message type: "+msg.Type)
		}
	}
}

func sendError(c *websocket.Conn, message string) {
	c.WriteJSON(map[string]interface{}{
		"type": "error",
		"payload": map[string]string{
			"message": message,
		},
	})
}

func detectLanguage(path string) string {
	// Simple language detection based on file extension
	ext := path[len(path)-3:]
	switch ext {
	case ".py":
		return "python"
	case ".cpp", ".cc", ".cxx":
		return "cpp"
	case ".c":
		return "c"
	case ".h", ".hpp":
		return "cpp"
	default:
		return "plaintext"
	}
}
