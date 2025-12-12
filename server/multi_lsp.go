package server

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
)

// MultiLSPManager manages multiple LSP servers (one per language)
type MultiLSPManager struct {
	lspServers       map[string]*LSPManager
	mu               sync.RWMutex
	notificationChan chan json.RawMessage
}

// LSPConfig holds configuration for an LSP server
type LSPConfig struct {
	Language           string
	ServerPath         string
	CompileCommandsDir string
	RootDir            string
}

// NewMultiLSPManager creates a new multi-LSP manager
func NewMultiLSPManager() *MultiLSPManager {
	m := &MultiLSPManager{
		lspServers:       make(map[string]*LSPManager),
		notificationChan: make(chan json.RawMessage, 100),
	}
	return m
}

// StartLSP starts an LSP server for a specific language
func (m *MultiLSPManager) StartLSP(language, serverPath, compileCommandsDir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if LSP already exists for this language
	if lsp, exists := m.lspServers[language]; exists {
		// Shutdown existing LSP
		lsp.Shutdown()
	}

	// Create new LSP manager
	lsp := NewLSPManager()
	if err := lsp.Start(serverPath, compileCommandsDir); err != nil {
		return fmt.Errorf("failed to start %s LSP: %v", language, err)
	}

	m.lspServers[language] = lsp

	// Start forwarding notifications from this LSP to the merged channel
	go m.forwardNotifications(language, lsp)

	log.Printf("Started %s LSP server (%s)", language, serverPath)
	return nil
}

// forwardNotifications forwards notifications from an LSP to the merged channel
func (m *MultiLSPManager) forwardNotifications(language string, lsp *LSPManager) {
	notifChan := lsp.GetNotificationChan()
	for notification := range notifChan {
		select {
		case m.notificationChan <- notification:
		default:
			log.Printf("MultiLSP notification channel full, dropping message from %s", language)
		}
	}
}

// InitializeLSP initializes an LSP server with the standard initialize request
func (m *MultiLSPManager) InitializeLSP(language, rootDir string) error {
	initParams := map[string]interface{}{
		"processId": nil,
		"rootUri":   "file://" + rootDir,
		"capabilities": map[string]interface{}{
			"textDocument": map[string]interface{}{
				"completion": map[string]interface{}{
					"completionItem": map[string]interface{}{
						"snippetSupport": true,
					},
				},
				"publishDiagnostics": map[string]interface{}{},
			},
		},
	}

	if _, err := m.SendRequest(language, "initialize", initParams); err != nil {
		return err
	}

	if err := m.SendNotification(language, "initialized", map[string]interface{}{}); err != nil {
		return err
	}

	log.Printf("Initialized %s LSP", language)
	return nil
}

// SendRequest sends a request to a specific language's LSP server
func (m *MultiLSPManager) SendRequest(language, method string, params interface{}) (json.RawMessage, error) {
	m.mu.RLock()
	lsp, exists := m.lspServers[language]
	m.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("no LSP server configured for language: %s", language)
	}

	return lsp.SendRequest(method, params)
}

// SendNotification sends a notification to a specific language's LSP server
func (m *MultiLSPManager) SendNotification(language, method string, params interface{}) error {
	m.mu.RLock()
	lsp, exists := m.lspServers[language]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("no LSP server configured for language: %s", language)
	}

	return lsp.SendNotification(method, params)
}

// RouteRequest routes a request based on the textDocument URI in params
// This extracts the language from the file path automatically
func (m *MultiLSPManager) RouteRequest(method string, params interface{}) (json.RawMessage, error) {
	language, err := m.extractLanguageFromParams(params)
	if err != nil {
		return nil, err
	}

	return m.SendRequest(language, method, params)
}

// RouteNotification routes a notification based on the textDocument URI in params
func (m *MultiLSPManager) RouteNotification(method string, params interface{}) error {
	language, err := m.extractLanguageFromParams(params)
	if err != nil {
		return err
	}

	return m.SendNotification(language, method, params)
}

// extractLanguageFromParams extracts the language from textDocument.uri in params
func (m *MultiLSPManager) extractLanguageFromParams(params interface{}) (string, error) {
	// Convert params to map
	paramsMap, ok := params.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("params is not a map")
	}

	// Extract textDocument.uri
	textDoc, ok := paramsMap["textDocument"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("textDocument not found in params")
	}

	uri, ok := textDoc["uri"].(string)
	if !ok {
		return "", fmt.Errorf("uri not found in textDocument")
	}

	// Remove "file://" prefix
	filePath := strings.TrimPrefix(uri, "file://")

	// Detect language from file extension
	language := detectLanguageForLSP(filePath)
	if language == "" {
		return "", fmt.Errorf("could not detect language for file: %s", filePath)
	}

	return language, nil
}

// detectLanguageForLSP detects which LSP to use based on file extension
func detectLanguageForLSP(path string) string {
	if strings.HasSuffix(path, ".py") {
		return "python"
	}
	if strings.HasSuffix(path, ".cpp") || strings.HasSuffix(path, ".cc") ||
		strings.HasSuffix(path, ".cxx") || strings.HasSuffix(path, ".c") ||
		strings.HasSuffix(path, ".h") || strings.HasSuffix(path, ".hpp") {
		return "cpp"
	}
	return ""
}

// GetNotificationChan returns the merged notification channel
func (m *MultiLSPManager) GetNotificationChan() <-chan json.RawMessage {
	return m.notificationChan
}

// IsRunning checks if an LSP server is running for a specific language
func (m *MultiLSPManager) IsRunning(language string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	lsp, exists := m.lspServers[language]
	if !exists {
		return false
	}

	return lsp.running
}

// ShutdownAll stops all LSP servers
func (m *MultiLSPManager) ShutdownAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for language, lsp := range m.lspServers {
		log.Printf("Shutting down %s LSP", language)
		lsp.Shutdown()
	}

	m.lspServers = make(map[string]*LSPManager)
}

// GetConfiguredLanguages returns a list of languages that have LSP servers configured
func (m *MultiLSPManager) GetConfiguredLanguages() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	languages := make([]string, 0, len(m.lspServers))
	for lang := range m.lspServers {
		languages = append(languages, lang)
	}
	return languages
}
