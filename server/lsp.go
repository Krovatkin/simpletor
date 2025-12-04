package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
)

// LSPManager manages the clangd LSP server process
type LSPManager struct {
	cmd              *exec.Cmd
	stdin            io.WriteCloser
	stdout           io.ReadCloser
	stderr           io.ReadCloser
	mu               sync.Mutex
	running          bool
	compileCommands  string
	messageID        int
	responseHandlers map[int]chan json.RawMessage
	notificationChan chan json.RawMessage
}

// NewLSPManager creates a new LSP manager
func NewLSPManager() *LSPManager {
	return &LSPManager{
		responseHandlers: make(map[int]chan json.RawMessage),
		notificationChan: make(chan json.RawMessage, 100),
	}
}

// Start starts the clangd process
func (lsp *LSPManager) Start(clangdPath, compileCommandsDir string) error {
	lsp.mu.Lock()
	defer lsp.mu.Unlock()

	if lsp.running {
		lsp.shutdown()
	}

	args := []string{}
	if compileCommandsDir != "" {
		args = append(args, fmt.Sprintf("--compile-commands-dir=%s", compileCommandsDir))
	}

	lsp.cmd = exec.Command(clangdPath, args...)

	var err error
	lsp.stdin, err = lsp.cmd.StdinPipe()
	if err != nil {
		return err
	}

	lsp.stdout, err = lsp.cmd.StdoutPipe()
	if err != nil {
		return err
	}

	lsp.stderr, err = lsp.cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := lsp.cmd.Start(); err != nil {
		return err
	}

	lsp.running = true
	lsp.compileCommands = compileCommandsDir

	// Start reading responses
	go lsp.readMessages()
	go lsp.logStderr()

	return nil
}

// shutdown stops the clangd process (must be called with lock held)
func (lsp *LSPManager) shutdown() {
	if lsp.running && lsp.cmd != nil && lsp.cmd.Process != nil {
		lsp.cmd.Process.Kill()
		lsp.cmd.Wait()
	}
	lsp.running = false
}

// Shutdown stops the clangd process
func (lsp *LSPManager) Shutdown() {
	lsp.mu.Lock()
	defer lsp.mu.Unlock()
	lsp.shutdown()
}

// SendRequest sends a JSON-RPC request to clangd
func (lsp *LSPManager) SendRequest(method string, params interface{}) (json.RawMessage, error) {
	lsp.mu.Lock()
	if !lsp.running {
		lsp.mu.Unlock()
		return nil, fmt.Errorf("LSP server not running")
	}

	lsp.messageID++
	id := lsp.messageID
	responseChan := make(chan json.RawMessage, 1)
	lsp.responseHandlers[id] = responseChan
	lsp.mu.Unlock()

	request := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}

	if err := lsp.writeMessage(request); err != nil {
		lsp.mu.Lock()
		delete(lsp.responseHandlers, id)
		lsp.mu.Unlock()
		return nil, err
	}

	// Wait for response
	response := <-responseChan
	return response, nil
}

// SendNotification sends a JSON-RPC notification to clangd
func (lsp *LSPManager) SendNotification(method string, params interface{}) error {
	request := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}

	return lsp.writeMessage(request)
}

// GetNotificationChan returns the channel for LSP notifications
func (lsp *LSPManager) GetNotificationChan() <-chan json.RawMessage {
	return lsp.notificationChan
}

// writeMessage writes a JSON-RPC message to clangd
func (lsp *LSPManager) writeMessage(message interface{}) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}

	// Log completion requests with full details
	if msgMap, ok := message.(map[string]interface{}); ok {
		if method, ok := msgMap["method"].(string); ok && method == "textDocument/completion" {
			log.Printf("=== COMPLETION REQUEST TO CLANGD ===")
			log.Printf("%s", string(data))
			log.Printf("====================================")
		}
	}

	content := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(data), data)

	lsp.mu.Lock()
	defer lsp.mu.Unlock()

	if !lsp.running {
		return fmt.Errorf("LSP server not running")
	}

	_, err = lsp.stdin.Write([]byte(content))
	return err
}

// readMessages reads messages from clangd stdout
func (lsp *LSPManager) readMessages() {
	reader := bufio.NewReader(lsp.stdout)

	for lsp.running {
		// Read Content-Length header
		var contentLength int
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}

			if line == "\r\n" {
				break
			}

			if n, err := fmt.Sscanf(line, "Content-Length: %d", &contentLength); err == nil && n == 1 {
				continue
			}
		}

		// Read the JSON content
		content := make([]byte, contentLength)
		if _, err := io.ReadFull(reader, content); err != nil {
			return
		}

		// Parse the message
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}

		if err := json.Unmarshal(content, &msg); err != nil {
			log.Printf("Failed to parse LSP message: %v", err)
			continue
		}

		// Handle response or notification
		if msg.ID != nil {
			lsp.mu.Lock()
			if ch, ok := lsp.responseHandlers[*msg.ID]; ok {
				ch <- content
				delete(lsp.responseHandlers, *msg.ID)
			}
			lsp.mu.Unlock()
		} else if msg.Method != "" {
			// It's a notification from the server
			select {
			case lsp.notificationChan <- content:
			default:
				log.Println("Notification channel full, dropping message")
			}
		}
	}
}

// logStderr logs clangd stderr output
func (lsp *LSPManager) logStderr() {
	scanner := bufio.NewScanner(lsp.stderr)
	for scanner.Scan() {
		log.Printf("clangd stderr: %s", scanner.Text())
	}
}
