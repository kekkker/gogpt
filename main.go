package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	baseDir      = ".gogpt"
	cookiesFile  = "cookies.enc"
	artifactsDir = "artifacts"
	pageSize     = 10
)

// ANSI color codes
const (
	colorUser   = "\033[0m"
	colorClaude = "\033[0m"
	colorReset  = "\033[0m"
	separator   = "────────────────────────────────────────────────────────────"
)

// ============== termios (pure syscall, no cgo) ==============

type termios struct {
	Iflag  uint32
	Oflag  uint32
	Cflag  uint32
	Lflag  uint32
	Cc     [20]byte
	Ispeed uint32
	Ospeed uint32
}

const (
	tcGets = 0x5401
	tcSets = 0x5402
	icanon = 0x2
	echoF  = 0x8
	vminI  = 6
	vtimeI = 5
)

func tcGetAttr(fd int) (*termios, error) {
	var t termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), tcGets, uintptr(unsafe.Pointer(&t)))
	if errno != 0 {
		return nil, errno
	}
	return &t, nil
}

func tcSetAttr(fd int, t *termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), tcSets, uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}

var savedTermios *termios

func rawOn() error {
	t, err := tcGetAttr(0)
	if err != nil {
		return err
	}
	savedTermios = &termios{}
	*savedTermios = *t

	t.Lflag &^= icanon | echoF
	t.Cc[vminI] = 1
	t.Cc[vtimeI] = 0
	return tcSetAttr(0, t)
}

func rawOff() {
	if savedTermios != nil {
		tcSetAttr(0, savedTermios)
		savedTermios = nil
	}
}

func readKey() byte {
	buf := make([]byte, 3)
	n, _ := syscall.Read(0, buf[:1])
	if n != 1 {
		return 0
	}
	ch := buf[0]
	if ch == 27 {
		n1, _ := syscall.Read(0, buf[:1])
		n2, _ := syscall.Read(0, buf[1:2])
		if n1 == 1 && n2 == 1 && buf[0] == '[' {
			switch buf[1] {
			case 'A':
				return 'k'
			case 'B':
				return 'j'
			case 'C':
				return '>'
			case 'D':
				return '<'
			}
		}
		return 27
	}
	return ch
}

// ============== Data Structures ==============

type ClaudeClient struct {
	organizationID   string
	cookies          string
	deviceID         string
	sessionArtifacts map[string][]Artifact
	streaming        bool
	cancelStream     chan bool
	streamMutex      sync.RWMutex
}

type ModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Description string
}

type ConversationResponse struct {
	UUID         string        `json:"uuid"`
	Name         string        `json:"name"`
	Summary      string        `json:"summary"`
	CreatedAt    string        `json:"created_at"`
	UpdatedAt    string        `json:"updated_at"`
	ChatMessages []ChatMessage `json:"chat_messages"`
}

type ChatMessage struct {
	UUID        string                   `json:"uuid"`
	Sender      string                   `json:"sender"`
	Text        string                   `json:"text"`
	Index       int                      `json:"index"`
	Attachments []map[string]interface{} `json:"attachments"`
	Content     json.RawMessage          `json:"content"`
	Files       []map[string]interface{} `json:"files"`
}

type OrganizationResponse struct {
	UUID string `json:"uuid"`
}

type CompletionRequest struct {
	Prompt             string                   `json:"prompt"`
	ParentMessageUUID  string                   `json:"parent_message_uuid,omitempty"`
	Timezone           string                   `json:"timezone"`
	PersonalizedStyles []map[string]interface{} `json:"personalized_styles"`
	Locale             string                   `json:"locale"`
	Tools              []map[string]interface{} `json:"tools"`
	Attachments        []interface{}            `json:"attachments"`
	Files              []interface{}            `json:"files"`
	SyncSources        []interface{}            `json:"sync_sources"`
	RenderingMode      string                   `json:"rendering_mode"`
	Model              string                   `json:"model,omitempty"`
}

type Artifact struct {
	Type       string    `json:"type"`
	Title      string    `json:"title"`
	Content    string    `json:"content"`
	Language   string    `json:"language"`
	ID         string    `json:"id"`
	Identifier string    `json:"identifier,omitempty"`
	FileName   string    `json:"file_name,omitempty"`
	Size       int64     `json:"size,omitempty"`
	MessageIdx int       `json:"message_idx,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type ArtifactStore struct {
	ConversationID string     `json:"conversation_id"`
	Artifacts      []Artifact `json:"artifacts"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type StreamState struct {
	InThinking           bool
	InCodeBlock          bool
	InToolUse            bool
	CurrentTool          string
	CurrentToolID        string
	CurrentLanguage      string
	ThinkingContent      strings.Builder
	ToolInput            strings.Builder
	ToolResultContent    strings.Builder
	NeedsContinuation    bool
	CapturedArtifacts    []Artifact
	CurrentArtifactInput map[string]interface{}
	EditedFiles          map[string][]EditOperation
	ViewedFilePath       string
}

type EditOperation struct {
	OldStr string
	NewStr string
}

type ContentBlock struct {
	Type     string
	Name     string
	Language string
	Title    string
	ID       string
}

// ============== Available Models ==============

type ModelsAPIResponse struct {
	Data []struct {
		ID          string    `json:"id"`
		DisplayName string    `json:"display_name"`
		CreatedAt   time.Time `json:"created_at"`
		Type        string    `json:"type"`
	} `json:"data"`
}

func (c *ClaudeClient) fetchAvailableModels() ([]ModelInfo, error) {
	url := "https://api.anthropic.com/v1/models"

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "dummy")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch models: status %d", resp.StatusCode)
	}

	var apiResp ModelsAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}

	models := make([]ModelInfo, 0, len(apiResp.Data))
	for _, m := range apiResp.Data {
		description := getModelDescription(m.ID)
		models = append(models, ModelInfo{
			ID:          m.ID,
			DisplayName: m.DisplayName,
			Description: description,
		})
	}

	return models, nil
}

func getModelDescription(modelID string) string {
	if strings.Contains(modelID, "opus-4-6") || strings.Contains(modelID, "opus-4.6") {
		return "Best for: multi-day projects, enterprise workflows, frontier intelligence"
	} else if strings.Contains(modelID, "opus-4-1") || strings.Contains(modelID, "opus-4.1") {
		return "Best for: AI agents, agentic search, expert coding"
	} else if strings.Contains(modelID, "sonnet-4-5") || strings.Contains(modelID, "sonnet-4.5") {
		return "Best for: complex code, computer use, agents, office files"
	} else if strings.Contains(modelID, "sonnet-4") {
		return "Best for: everyday tasks with speed and cost balance"
	} else if strings.Contains(modelID, "haiku-4-5") || strings.Contains(modelID, "haiku-4.5") {
		return "Best for: fast responses, high volume, cost efficiency"
	} else if strings.Contains(modelID, "sonnet-3") {
		return "Legacy model"
	}
	return "Claude AI model"
}

func getFallbackModels() []ModelInfo {
	return []ModelInfo{
		{
			ID:          "claude-sonnet-4-5-20250929",
			DisplayName: "Claude Sonnet 4.5",
			Description: "Best for: complex code, computer use, agents, office files",
		},
		{
			ID:          "claude-sonnet-4-20250514",
			DisplayName: "Claude Sonnet 4",
			Description: "Best for: everyday tasks with speed and cost balance",
		},
		{
			ID:          "claude-opus-4-6",
			DisplayName: "Claude Opus 4.6",
			Description: "Best for: multi-day projects, enterprise workflows, frontier intelligence",
		},
		{
			ID:          "claude-opus-4-1-20250805",
			DisplayName: "Claude Opus 4.1",
			Description: "Best for: AI agents, agentic search, expert coding",
		},
		{
			ID:          "claude-haiku-4-5-20251001",
			DisplayName: "Claude Haiku 4.5",
			Description: "Best for: fast responses, high volume, cost efficiency",
		},
		{
			ID:          "claude-3-7-sonnet-20250219",
			DisplayName: "Claude 3.7 Sonnet",
			Description: "Legacy: older sonnet model",
		},
	}
}

// ============== Path Utilities ==============

func getBasePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, baseDir)
}

func getPath(subpath string) string {
	return filepath.Join(getBasePath(), subpath)
}

func ensureBaseDir() {
	base := getBasePath()
	if _, err := os.Stat(base); os.IsNotExist(err) {
		os.MkdirAll(base, 0755)
	}
}

func ensureArtifactsDir() {
	dir := getPath(artifactsDir)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		os.MkdirAll(dir, 0755)
	}
}

// ============== Artifact Functions ==============

func getArtifactFilePath(conversationID string) string {
	return filepath.Join(getPath(artifactsDir), conversationID+".json")
}

func loadArtifacts(conversationID string) []Artifact {
	ensureArtifactsDir()
	filePath := getArtifactFilePath(conversationID)

	data, err := os.ReadFile(filePath)
	if err != nil {
		return []Artifact{}
	}

	var store ArtifactStore
	if err := json.Unmarshal(data, &store); err != nil {
		return []Artifact{}
	}

	return store.Artifacts
}

func saveArtifacts(conversationID string, artifacts []Artifact) error {
	ensureArtifactsDir()
	filePath := getArtifactFilePath(conversationID)

	store := ArtifactStore{
		ConversationID: conversationID,
		Artifacts:      artifacts,
		UpdatedAt:      time.Now(),
	}

	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filePath, data, 0644)
}

func appendArtifacts(conversationID string, newArtifacts []Artifact) error {
	existing := loadArtifacts(conversationID)

	existingIDs := make(map[string]bool)
	for _, a := range existing {
		existingIDs[a.ID] = true
	}

	for _, a := range newArtifacts {
		if !existingIDs[a.ID] {
			existing = append(existing, a)
		}
	}

	return saveArtifacts(conversationID, existing)
}

func inferLanguageFromFilename(filename string) string {
	filename = strings.ToLower(filename)
	if idx := strings.LastIndex(filename, "/"); idx != -1 {
		filename = filename[idx+1:]
	}

	extensions := map[string]string{
		".go": "go", ".py": "python", ".js": "javascript", ".ts": "typescript",
		".jsx": "jsx", ".tsx": "tsx", ".html": "html", ".css": "css",
		".json": "json", ".sh": "bash", ".md": "markdown", ".yaml": "yaml",
		".yml": "yaml", ".sql": "sql", ".rs": "rust", ".java": "java",
		".c": "c", ".h": "c", ".cpp": "cpp", ".hpp": "cpp", ".cc": "cpp",
		".zig": "zig",
	}

	for ext, lang := range extensions {
		if strings.HasSuffix(filename, ext) {
			return lang
		}
	}
	return "text"
}

// ============== Remote Artifact Fetching (Wiggle API) ==============

func (c *ClaudeClient) listRemoteFiles(conversationID string) ([]Artifact, error) {
	url := fmt.Sprintf("https://claude.ai/api/organizations/%s/conversations/%s/wiggle/list-files?prefix=",
		c.organizationID, conversationID)

	resp, err := c.makeRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to list files: %d - %s", resp.StatusCode, string(body))
	}

	var result struct {
		Success       bool     `json:"success"`
		Files         []string `json:"files"`
		FilesMetadata []struct {
			Path        string `json:"path"`
			Size        int64  `json:"size"`
			ContentType string `json:"content_type"`
			CreatedAt   string `json:"created_at"`
		} `json:"files_metadata"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if !result.Success {
		return nil, fmt.Errorf("list-files returned success: false")
	}

	artifacts := make([]Artifact, 0, len(result.FilesMetadata))
	for _, meta := range result.FilesMetadata {
		createdAt, _ := time.Parse(time.RFC3339, meta.CreatedAt)
		fileName := filepath.Base(meta.Path)

		artifacts = append(artifacts, Artifact{
			ID:        fmt.Sprintf("remote_%s", meta.Path),
			Type:      "file",
			Title:     fileName,
			FileName:  meta.Path,
			Size:      meta.Size,
			Language:  inferLanguageFromFilename(fileName),
			CreatedAt: createdAt,
			Content:   "",
		})
	}

	return artifacts, nil
}

func (c *ClaudeClient) downloadRemoteFile(conversationID, filePath string) (string, error) {
	encodedPath := url.QueryEscape(filePath)
	url := fmt.Sprintf("https://claude.ai/api/organizations/%s/conversations/%s/wiggle/download-file?path=%s",
		c.organizationID, conversationID, encodedPath)

	resp, err := c.makeRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to download file: %d - %s", resp.StatusCode, string(body))
	}

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(content), nil
}

// ============== Claude Client ==============

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func base64Decode(s string) string {
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		log.Fatal("Error decoding base64:", err)
	}
	return string(decoded)
}

func generateDeviceID() string {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(time.Now().UnixNano() % 256)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func getCookies() string {
	ensureBaseDir()
	cookiesPath := getPath(cookiesFile)

	if _, err := os.Stat(cookiesPath); os.IsNotExist(err) {
		os.Create(cookiesPath)
	}

	data, err := os.ReadFile(cookiesPath)
	if err != nil {
		log.Fatal("Error reading cookies file:", err)
	}

	cookies := string(data)
	if cookies != "" {
		return base64Decode(cookies)
	}

	fmt.Print("SESSION COOKIES MISSING\nInput cookies > ")
	reader := bufio.NewReader(os.Stdin)
	cookies, _ = reader.ReadString('\n')
	cookies = strings.TrimSpace(cookies)

	encoded := base64Encode(cookies)
	os.WriteFile(cookiesPath, []byte(encoded), 0644)

	return cookies
}

func NewClaudeClient() *ClaudeClient {
	cookies := getCookies()
	client := &ClaudeClient{
		cookies:          cookies,
		deviceID:         generateDeviceID(),
		sessionArtifacts: make(map[string][]Artifact),
		cancelStream:     make(chan bool, 1),
	}
	client.organizationID = client.getOrganizationID()

	fmt.Println()

	return client
}

func (c *ClaudeClient) isStreaming() bool {
	c.streamMutex.RLock()
	defer c.streamMutex.RUnlock()
	return c.streaming
}

func (c *ClaudeClient) setStreaming(val bool) {
	c.streamMutex.Lock()
	defer c.streamMutex.Unlock()
	c.streaming = val
}

func (c *ClaudeClient) makeRequest(method, url string, body io.Reader) (*http.Response, error) {
	client := &http.Client{Timeout: 0}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
	req.Header.Set("Accept", "text/event-stream, text/event-stream")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", "https://claude.ai/chats")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-client-platform", "web_claude_ai")
	req.Header.Set("anthropic-device-id", c.deviceID)
	req.Header.Set("Origin", "https://claude.ai")
	req.Header.Set("Cookie", c.cookies)

	return client.Do(req)
}

func (c *ClaudeClient) getOrganizationID() string {
	url := "https://claude.ai/api/organizations"
	resp, err := c.makeRequest("GET", url, nil)
	if err != nil {
		log.Fatal("Error getting organization ID:", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		fmt.Println(string(respBody))
		log.Fatal("Failed to get organization ID. Please check your cookies.")
	}

	var orgs []OrganizationResponse
	if err := json.NewDecoder(resp.Body).Decode(&orgs); err != nil {
		log.Fatal("Error decoding organization response:", err)
	}

	if len(orgs) == 0 {
		log.Fatal("No organizations found")
	}

	return orgs[0].UUID
}

func (c *ClaudeClient) getConversations() []ConversationResponse {
	url := fmt.Sprintf("https://claude.ai/api/organizations/%s/chat_conversations", c.organizationID)
	resp, err := c.makeRequest("GET", url, nil)
	if err != nil {
		log.Fatal("Error getting conversations:", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Fatal("Failed to get conversations. Status:", resp.StatusCode)
	}

	var conversations []ConversationResponse
	if err := json.NewDecoder(resp.Body).Decode(&conversations); err != nil {
		log.Fatal("Error decoding conversations response:", err)
	}

	sort.Slice(conversations, func(i, j int) bool {
		return conversations[i].UpdatedAt > conversations[j].UpdatedAt
	})

	return conversations
}

func (c *ClaudeClient) getConversationDetails(conversationID string) *ConversationResponse {
	url := fmt.Sprintf("https://claude.ai/api/organizations/%s/chat_conversations/%s?tree=True&rendering_mode=raw",
		c.organizationID, conversationID)

	resp, err := c.makeRequest("GET", url, nil)
	if err != nil {
		log.Fatal("Error getting conversation details:", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Error reading response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		log.Fatalf("Failed to get conversation details. Status: %d", resp.StatusCode)
	}

	var conversation ConversationResponse
	if err := json.Unmarshal(bodyBytes, &conversation); err != nil {
		log.Fatalf("Error decoding conversation details: %v", err)
	}

	return &conversation
}

func (c *ClaudeClient) createNewChat() string {
	url := fmt.Sprintf("https://claude.ai/api/organizations/%s/chat_conversations", c.organizationID)

	payload := map[string]string{"name": ""}
	jsonData, _ := json.Marshal(payload)
	resp, err := c.makeRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		log.Fatal("Error creating new chat:", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		log.Fatal("Failed to create chat. Status:", resp.StatusCode)
	}

	var result ConversationResponse
	json.Unmarshal(bodyBytes, &result)
	return result.UUID
}

func (c *ClaudeClient) stopResponse(conversationID string) error {
	url := fmt.Sprintf("https://claude.ai/api/organizations/%s/chat_conversations/%s/stop_response",
		c.organizationID, conversationID)

	resp, err := c.makeRequest("POST", url, bytes.NewBuffer([]byte{}))
	if err != nil {
		return fmt.Errorf("failed to stop response: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("stop response failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (c *ClaudeClient) autoGenerateTitle(conversationID string, messageContent string) error {
	url := fmt.Sprintf("https://claude.ai/api/organizations/%s/chat_conversations/%s/title",
		c.organizationID, conversationID)

	payload := map[string]interface{}{
		"message_content": messageContent,
		"recent_titles":   []string{},
	}

	jsonData, _ := json.Marshal(payload)
	resp, err := c.makeRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to generate title: %d - %s", resp.StatusCode, string(body))
	}

	var result struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}

	if result.Title == "" {
		return fmt.Errorf("no title received")
	}

	return c.renameConversation(conversationID, result.Title)
}

func (c *ClaudeClient) renameConversation(conversationID, newName string) error {
	url := fmt.Sprintf("https://claude.ai/api/organizations/%s/chat_conversations/%s",
		c.organizationID, conversationID)

	payload := map[string]interface{}{
		"name": newName,
	}

	jsonData, _ := json.Marshal(payload)
	resp, err := c.makeRequest("PUT", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to rename conversation: %d - %s", resp.StatusCode, string(body))
	}

	return nil
}

func (c *ClaudeClient) sendMessage(ctx context.Context, prompt, conversationID, modelID string) (string, bool, []Artifact, map[string][]EditOperation, bool) {
	url := fmt.Sprintf("https://claude.ai/api/organizations/%s/chat_conversations/%s/completion",
		c.organizationID, conversationID)

	conv := c.getConversationDetails(conversationID)
	var parentUUID string
	if len(conv.ChatMessages) > 0 {
		parentUUID = conv.ChatMessages[len(conv.ChatMessages)-1].UUID
	}

	tools := []map[string]interface{}{
		{"type": "web_search_v0", "name": "web_search"},
		{"type": "artifacts_v0", "name": "artifacts"},
		{"type": "repl_v0", "name": "repl"},
	}

	reqBody := CompletionRequest{
		Prompt:            prompt,
		ParentMessageUUID: parentUUID,
		Timezone:          "UTC",
		PersonalizedStyles: []map[string]interface{}{
			{"type": "default", "key": "Default", "name": "Normal", "nameKey": "normal_style_name",
				"prompt": "Normal", "summary": "Default responses from Claude", "summaryKey": "normal_style_summary", "isDefault": true},
		},
		Locale:        "en-US",
		Tools:         tools,
		Attachments:   []interface{}{},
		Files:         []interface{}{},
		SyncSources:   []interface{}{},
		RenderingMode: "messages",
		Model:         modelID,
	}

	jsonData, _ := json.Marshal(reqBody)

	client := &http.Client{Timeout: 0}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		log.Fatal("Error creating request:", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
	req.Header.Set("Accept", "text/event-stream, text/event-stream")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", "https://claude.ai/chats")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-client-platform", "web_claude_ai")
	req.Header.Set("anthropic-device-id", c.deviceID)
	req.Header.Set("Origin", "https://claude.ai")
	req.Header.Set("Cookie", c.cookies)

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, nil, nil, true
		}
		log.Fatal("Error sending message:", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Fatal("Failed to send message. Status:", resp.StatusCode, "Body:", string(body))
	}

	c.setStreaming(true)
	response, needsContinue, artifacts, editedFiles, cancelled := c.readStreamResponse(ctx, resp.Body)
	c.setStreaming(false)

	return response, needsContinue, artifacts, editedFiles, cancelled
}

func (c *ClaudeClient) readStreamResponse(ctx context.Context, body io.ReadCloser) (string, bool, []Artifact, map[string][]EditOperation, bool) {
	reader := bufio.NewReader(body)
	var fullResponse strings.Builder
	var currentEvent string
	state := &StreamState{
		CurrentArtifactInput: make(map[string]interface{}),
		EditedFiles:          make(map[string][]EditOperation),
	}
	contentBlocks := make(map[int]*ContentBlock)
	needsContinuation := false
	cancelled := false

	for {
		select {
		case <-ctx.Done():
			cancelled = true
			return fullResponse.String(), needsContinuation, state.CapturedArtifacts, state.EditedFiles, cancelled
		case <-c.cancelStream:
			cancelled = true
			return fullResponse.String(), needsContinuation, state.CapturedArtifacts, state.EditedFiles, cancelled
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			break
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")

			if currentEvent == "message_stop" {
				break
			}

			var jsonData map[string]interface{}
			if err := json.Unmarshal([]byte(data), &jsonData); err != nil {
				continue
			}

			switch currentEvent {
			case "content_block_start":
				c.handleContentBlockStart(jsonData, state, contentBlocks)
			case "content_block_delta":
				c.handleContentBlockDelta(jsonData, state, contentBlocks, &fullResponse)
			case "content_block_stop":
				c.handleContentBlockStop(jsonData, state, contentBlocks)
			case "message_delta":
				if delta, ok := jsonData["delta"].(map[string]interface{}); ok {
					if stopReason, ok := delta["stop_reason"].(string); ok && stopReason == "max_tokens" {
						needsContinuation = true
					}
				}
			}
		}
	}

	return fullResponse.String(), needsContinuation, state.CapturedArtifacts, state.EditedFiles, cancelled
}

func (c *ClaudeClient) handleContentBlockStart(jsonData map[string]interface{}, state *StreamState, blocks map[int]*ContentBlock) {
	index := int(jsonData["index"].(float64))
	contentBlock, ok := jsonData["content_block"].(map[string]interface{})
	if !ok {
		return
	}

	blockType, _ := contentBlock["type"].(string)
	block := &ContentBlock{Type: blockType}

	if blockType == "tool_use" {
		toolName, _ := contentBlock["name"].(string)
		toolID, _ := contentBlock["id"].(string)
		block.Name = toolName
		block.ID = toolID

		// Skip present_files - it's UI-only, not relevant in terminal
		if toolName == "present_files" {
			state.InToolUse = false
			blocks[index] = block
			return
		}

		state.InToolUse = true
		state.CurrentTool = toolName
		state.CurrentToolID = toolID
		state.ToolInput.Reset()

		fmt.Print("\n")

		switch toolName {
		case "web_search":
			fmt.Print("[searching web] ")
		case "artifacts":
			fmt.Print("[creating artifact] ")
		case "create_file", "file_create":
			fmt.Print("[creating file] ")
		case "str_replace":
			fmt.Print("[editing file] ")
		case "repl":
			fmt.Print("[running code] ")
		case "view":
			fmt.Print("[viewing file] ")
		case "bash_tool":
			fmt.Print("[running bash] ")
		default:
			fmt.Printf("[tool: %s] ", toolName)
		}
	}

	blocks[index] = block
}

func (c *ClaudeClient) handleContentBlockDelta(jsonData map[string]interface{}, state *StreamState, blocks map[int]*ContentBlock, fullResponse *strings.Builder) {
	delta, ok := jsonData["delta"].(map[string]interface{})
	if !ok {
		return
	}

	deltaType, _ := delta["type"].(string)

	switch deltaType {
	case "text_delta":
		if text, ok := delta["text"].(string); ok {
			fmt.Print(text)
			fullResponse.WriteString(text)
		}

	case "input_json_delta":
		if partialJSON, ok := delta["partial_json"].(string); ok {
			state.ToolInput.WriteString(partialJSON)
		}

	case "tool_result_delta":
		if content, ok := delta["content"].(string); ok {
			state.ToolResultContent.WriteString(content)
		}
	}
}

func (c *ClaudeClient) handleContentBlockStop(jsonData map[string]interface{}, state *StreamState, blocks map[int]*ContentBlock) {
	index := int(jsonData["index"].(float64))
	block, exists := blocks[index]
	if !exists || block == nil {
		return
	}

	if block.Type == "tool_use" && state.InToolUse {
		fullInput := state.ToolInput.String()
		var input map[string]interface{}
		if err := json.Unmarshal([]byte(fullInput), &input); err == nil {

			artifact := Artifact{
				ID:        state.CurrentToolID,
				Type:      state.CurrentTool,
				CreatedAt: time.Now(),
			}

			switch state.CurrentTool {
			case "create_file", "file_create":
				if path, ok := input["path"].(string); ok {
					artifact.FileName = path
					artifact.Title = path
				}
				if content, ok := input["file_text"].(string); ok {
					artifact.Content = content
				}
				artifact.Language = inferLanguageFromFilename(artifact.FileName)
				artifact.Type = "file"

			case "str_replace":
				if path, ok := input["path"].(string); ok {
					oldStr, _ := input["old_str"].(string)
					newStr, _ := input["new_str"].(string)

					edit := EditOperation{
						OldStr: oldStr,
						NewStr: newStr,
					}
					state.EditedFiles[path] = append(state.EditedFiles[path], edit)
				}
				artifact.Content = ""

			case "view":
				if path, ok := input["path"].(string); ok {
					artifact.FileName = path
					artifact.Title = fmt.Sprintf("View: %s", path)
					artifact.Language = inferLanguageFromFilename(path)
					artifact.Type = "view"
					state.ViewedFilePath = path
					artifact.Content = ""
				}
			}

			if artifact.Content != "" {
				state.CapturedArtifacts = append(state.CapturedArtifacts, artifact)
			}

			fmt.Print("done\n")
		}

		state.InToolUse = false
		state.CurrentTool = ""
		state.CurrentToolID = ""
		state.ToolInput.Reset()
	}

	if block.Type == "tool_result" && state.InToolUse {
		if state.ToolResultContent.Len() > 0 && state.ViewedFilePath != "" {
			artifact := Artifact{
				ID:        fmt.Sprintf("view_%d", time.Now().UnixNano()),
				Type:      "view",
				Title:     state.ViewedFilePath,
				FileName:  state.ViewedFilePath,
				Content:   state.ToolResultContent.String(),
				Language:  inferLanguageFromFilename(state.ViewedFilePath),
				CreatedAt: time.Now(),
			}
			state.CapturedArtifacts = append(state.CapturedArtifacts, artifact)
		}

		state.InToolUse = false
		state.ToolResultContent.Reset()
		state.ViewedFilePath = ""
	}
}

// ============== TUI ==============

func cls() {
	fmt.Print("\033[2J\033[H")
}

func pickConvo(convos []ConversationResponse) (int, *ConversationResponse) {
	n := len(convos)
	cur, pg := 0, 0
	pgs := (n + pageSize - 1) / pageSize
	if pgs < 1 {
		pgs = 1
	}

	if err := rawOn(); err != nil {
		fmt.Fprintln(os.Stderr, "raw mode failed:", err)
		return -1, nil
	}
	defer rawOff()

	for {
		st := pg * pageSize
		en := st + pageSize
		if en > n {
			en = n
		}
		cnt := en - st

		cls()
		fmt.Print("Select a Chat Session\n\n")
		if cur == 0 {
			fmt.Println("> Start New Chat")
		} else {
			fmt.Println("  Start New Chat")
		}

		for i := 0; i < cnt; i++ {
			nm := convos[st+i].Name
			if nm == "" {
				nm = convos[st+i].Summary
			}
			if nm == "" {
				nm = "Untitled chat"
			}
			if len(nm) > 60 {
				nm = nm[:60]
			}
			m := "  "
			if cur == i+1 {
				m = ">"
			}
			fmt.Printf("%s Continue: %s (%.8s)\n", m, nm, convos[st+i].UUID)
		}

		if n > pageSize {
			fmt.Printf("\nPage %d/%d (%d total)\n(j/k:move enter:select q:quit </>:page)\n", pg+1, pgs, n)
		} else {
			fmt.Println("\n(j/k:move enter:select q:quit)")
		}

		ch := readKey()
		switch ch {
		case 'q':
			return -1, nil
		case 'k':
			if cur > 0 {
				cur--
			} else if pg > 0 {
				pg--
				newEn := pg*pageSize + pageSize
				if newEn > n {
					newEn = n
				}
				cur = newEn - pg*pageSize
			}
		case 'j':
			if cur < cnt {
				cur++
			} else if pg < pgs-1 {
				pg++
				cur = 0
			}
		case '<':
			if pg > 0 {
				pg--
				cur = 0
			}
		case '>':
			if pg < pgs-1 {
				pg++
				cur = 0
			}
		case '\r', '\n':
			if cur == 0 {
				return 0, nil
			}
			idx := pg*pageSize + (cur - 1)
			if idx < n {
				return 1, &convos[idx]
			}
		}
	}
}

func pickModel(client *ClaudeClient) (string, bool) {
	models, err := client.fetchAvailableModels()
	if err != nil {
		models = getFallbackModels()
	}

	n := len(models)
	cur := 0

	if err := rawOn(); err != nil {
		return "", false
	}
	defer rawOff()

	for {
		cls()
		fmt.Print("Select Claude Model\n\n")
		for i := 0; i < n; i++ {
			m := "  "
			if cur == i {
				m = ">"
			}
			fmt.Printf("%s %s\n   %s\n", m, models[i].DisplayName, models[i].Description)
			if i < n-1 {
				fmt.Println()
			}
		}
		fmt.Println("\n(j/k:move enter:select q:quit)")

		ch := readKey()
		switch ch {
		case 'q':
			return "", false
		case 'k':
			if cur > 0 {
				cur--
			}
		case 'j':
			if cur < n-1 {
				cur++
			}
		case '\r', '\n':
			return models[cur].ID, true
		}
	}
}

// ============== Simple CLI Chat ==============

func runSimpleChat(client *ClaudeClient, conversationID string, isNew bool, modelID string) bool {
	conv := client.getConversationDetails(conversationID)

	if len(conv.ChatMessages) > 0 {
		fmt.Println("--- Chat History Loaded ---")
		for _, msg := range conv.ChatMessages {
			var separatorColor string
			if msg.Sender == "human" {
				separatorColor = colorUser
			} else {
				separatorColor = colorClaude
			}

			if msg.Sender == "human" {
				fmt.Printf("%s%s%s\n", separatorColor, separator, colorReset)
				fmt.Printf("%sYou:%s %s\n", colorUser, colorReset, msg.Text)
				fmt.Printf("\n%s%s%s\n", separatorColor, separator, colorReset)
			} else {
				displayText := msg.Text
				artifactPattern := regexp.MustCompile(`(?s)<antArtifact[^>]*>.*?`)
				displayText = artifactPattern.ReplaceAllString(displayText, "[Artifact]")

				fmt.Printf("%s%s%s\n", separatorColor, separator, colorReset)
				fmt.Printf("%sClaude:%s %s\n", colorClaude, colorReset, displayText)
				fmt.Printf("\n%s%s%s\n", separatorColor, separator, colorReset)
			}
		}
		fmt.Printf("%s%s%s\n", colorClaude, separator, colorReset)
	} else {
		fmt.Printf("%s%s%s\n", colorClaude, separator, colorReset)
	}

	var modelDisplayName string
	for _, m := range getFallbackModels() {
		if m.ID == modelID {
			modelDisplayName = m.DisplayName
			break
		}
	}
	if modelDisplayName == "" {
		modelDisplayName = modelID
	}
	fmt.Printf("Using model: %s\n", modelDisplayName)

	fmt.Println("Type your message, then '.' on a new line to send.")
	fmt.Println("Commands: !exit, !files")
	fmt.Println("Press ESC during streaming to cancel the response.")

	scanner := bufio.NewScanner(os.Stdin)
	isFirstMessage := isNew

	for {
		fmt.Printf("%s%s%s\n", colorUser, separator, colorReset)
		fmt.Printf("%sYou:%s ", colorUser, colorReset)

		var lines []string
		for scanner.Scan() {
			line := scanner.Text()
			if line == "." {
				break
			}

			if len(lines) == 0 {
				trimmed := strings.TrimSpace(line)

				if trimmed == "!exit" {
					fmt.Printf("%s%s%s\n", colorUser, separator, colorReset)
					return true
				}

				if trimmed == "!files" {
					fmt.Printf("%s%s%s\n", colorUser, separator, colorReset)

					fmt.Println("Fetching files from Claude's servers...")
					remoteArtifacts, err := client.listRemoteFiles(conversationID)
					if err != nil {
						fmt.Printf("Error fetching remote files: %v\n", err)
						continue
					}

					if len(remoteArtifacts) == 0 {
						fmt.Println("No files in this conversation.")
						fmt.Println("Tip: Ask Claude to create files using 'Create a file...'")
					} else {
						fmt.Printf("\n=== Files from Claude.ai (%d) ===\n", len(remoteArtifacts))
						fmt.Println("(Always up-to-date with web interface)\n")

						for i, art := range remoteArtifacts {
							sizeStr := ""
							if art.Size > 0 {
								if art.Size < 1024 {
									sizeStr = fmt.Sprintf(" | Size: %d bytes", art.Size)
								} else if art.Size < 1024*1024 {
									sizeStr = fmt.Sprintf(" | Size: %.1f KB", float64(art.Size)/1024)
								} else {
									sizeStr = fmt.Sprintf(" | Size: %.1f MB", float64(art.Size)/(1024*1024))
								}
							}

							timeStr := art.CreatedAt.Format("Jan 2, 15:04")
							fmt.Printf("[%d] %s\n", i+1, art.Title)
							fmt.Printf("    Type: %s | Created: %s%s\n", art.Language, timeStr, sizeStr)
							fmt.Printf("    Path: %s\n", art.FileName)
						}

						fmt.Print("\nEnter number to view, 'a' to download all, or press enter to skip: ")
						reader := bufio.NewReader(os.Stdin)
						choiceStr, _ := reader.ReadString('\n')
						choiceStr = strings.TrimSpace(strings.ToLower(choiceStr))

						if choiceStr == "a" || choiceStr == "all" {
							fmt.Print("\nWhere to download? (default: /tmp): ")
							baseDir, _ := reader.ReadString('\n')
							baseDir = strings.TrimSpace(baseDir)

							if baseDir == "" {
								baseDir = "/tmp"
							}

							fmt.Printf("\nDownloading %d file(s) to: %s\n", len(remoteArtifacts), baseDir)
							fmt.Println("(Preserving directory structure)\n")

							successCount := 0
							failCount := 0

							for i, art := range remoteArtifacts {
								relativePath := art.FileName
								if strings.HasPrefix(relativePath, "/mnt/user-data/outputs/") {
									relativePath = strings.TrimPrefix(relativePath, "/mnt/user-data/outputs/")
								}

								destinationPath := filepath.Join(baseDir, relativePath)

								destDir := filepath.Dir(destinationPath)
								if err := os.MkdirAll(destDir, 0755); err != nil {
									fmt.Printf("[%d/%d] %s... ✗ Failed to create directory: %v\n", i+1, len(remoteArtifacts), art.Title, err)
									failCount++
									continue
								}

								fmt.Printf("[%d/%d] %s... ", i+1, len(remoteArtifacts), relativePath)

								content, err := client.downloadRemoteFile(conversationID, art.FileName)
								if err != nil {
									fmt.Printf("✗ Failed: %v\n", err)
									failCount++
									continue
								}

								if err := os.WriteFile(destinationPath, []byte(content), 0644); err != nil {
									fmt.Printf("✗ Failed to write: %v\n", err)
									failCount++
									continue
								}

								fmt.Printf("✓\n")
								successCount++
							}

							fmt.Printf("\n✓ Downloaded %d file(s)", successCount)
							if failCount > 0 {
								fmt.Printf(" (%d failed)", failCount)
							}
							fmt.Printf("\n\nFiles saved to: %s\n", baseDir)

						} else if choiceStr != "" {
							choice, err := strconv.Atoi(choiceStr)
							if err == nil && choice >= 1 && choice <= len(remoteArtifacts) {
								art := remoteArtifacts[choice-1]

								fmt.Printf("\nDownloading %s...\n", art.Title)
								content, err := client.downloadRemoteFile(conversationID, art.FileName)
								if err != nil {
									fmt.Printf("Error downloading file: %v\n", err)
									continue
								}

								fmt.Printf("\n=== %s (%d bytes) ===\n\n", art.Title, len(content))
								lang := art.Language
								if lang == "" {
									lang = "text"
								}
								fmt.Printf("```%s\n", lang)
								fmt.Print(content)
								if !strings.HasSuffix(content, "\n") {
									fmt.Println()
								}
								fmt.Printf("```\n\n")
							}
						}
					}
					continue
				}
			}

			lines = append(lines, line)
		}

		if err := scanner.Err(); err != nil {
			fmt.Printf("Error reading input: %v\n", err)
			return false
		}

		userInput := strings.Join(lines, "\n")
		if strings.TrimSpace(userInput) == "" {
			fmt.Printf("%s%s%s\n", colorUser, separator, colorReset)
			continue
		}

		fmt.Printf("%s%s%s\n\n", colorUser, separator, colorReset)
		fmt.Printf("%s%s%s\n", colorClaude, separator, colorReset)
		fmt.Printf("%sClaude:%s ", colorClaude, colorReset)

		select {
		case <-client.cancelStream:
		default:
		}

		ctx, cancel := context.WithCancel(context.Background())

		// ESC handler goroutine
		go func() {
			origTermios, termErr := tcGetAttr(0)
			if termErr != nil {
				return
			}

			raw := *origTermios
			raw.Lflag &^= icanon | echoF
			raw.Cc[vminI] = 1
			raw.Cc[vtimeI] = 0
			tcSetAttr(0, &raw)

			defer func() {
				tcSetAttr(0, origTermios)
			}()

			buf := make([]byte, 1)
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				if !client.isStreaming() {
					return
				}

				n, _ := syscall.Read(0, buf)
				if n == 1 && buf[0] == 27 {
					if client.isStreaming() {
						go client.stopResponse(conversationID)
						select {
						case client.cancelStream <- true:
						default:
						}
						cancel()
						return
					}
				}
			}
		}()

		response, _, artifacts, editedFiles, cancelled := client.sendMessage(ctx, userInput, conversationID, modelID)

		cancel()

		if cancelled {
			fmt.Printf("\n\n[Stream cancelled by user]\n")
			fmt.Printf("%s%s%s\n\n", colorClaude, separator, colorReset)
			continue
		}

		fmt.Printf("\n%s%s%s\n\n", colorClaude, separator, colorReset)

		if len(artifacts) > 0 {
			client.sessionArtifacts[conversationID] = append(client.sessionArtifacts[conversationID], artifacts...)
			appendArtifacts(conversationID, artifacts)

			for _, art := range artifacts {
				if art.Content != "" && (art.Type == "file" || art.Type == "artifact") {
					fmt.Printf("\n[artifact] %s\n", art.Title)
					lang := art.Language
					if lang == "" {
						lang = "text"
					}
					fmt.Printf("```%s\n", lang)
					fmt.Print(art.Content)
					if !strings.HasSuffix(art.Content, "\n") {
						fmt.Println()
					}
					fmt.Printf("```\n\n")
				}
			}
		}

		if len(editedFiles) > 0 {
			editedFilePaths := make([]string, 0, len(editedFiles))
			for filePath := range editedFiles {
				editedFilePaths = append(editedFilePaths, filePath)
			}
			sort.Strings(editedFilePaths)

			fmt.Printf("\n[edited %d file(s)] ", len(editedFilePaths))
			for i, path := range editedFilePaths {
				if i > 0 {
					fmt.Print(", ")
				}
				fmt.Print(filepath.Base(path))
			}
			fmt.Println()

			time.Sleep(500 * time.Millisecond)

			fmt.Println("Fetching updated files from server...")
			remoteFiles, err := client.listRemoteFiles(conversationID)
			if err != nil {
				fmt.Printf("Warning: Could not fetch remote files: %v\n", err)
			} else {
				remoteFileMap := make(map[string]Artifact)
				for _, rf := range remoteFiles {
					baseName := filepath.Base(rf.FileName)
					remoteFileMap[baseName] = rf
				}

				if len(editedFilePaths) == 1 {
					filePath := editedFilePaths[0]
					baseName := filepath.Base(filePath)

					if remoteArt, exists := remoteFileMap[baseName]; exists {
						content, err := client.downloadRemoteFile(conversationID, remoteArt.FileName)
						if err != nil {
							fmt.Printf("Warning: Could not download %s: %v\n", baseName, err)
						} else {
							fmt.Printf("\n=== Updated: %s ===\n\n", baseName)
							lang := remoteArt.Language
							if lang == "" {
								lang = "text"
							}
							fmt.Printf("```%s\n", lang)
							fmt.Print(content)
							if !strings.HasSuffix(content, "\n") {
								fmt.Println()
							}
							fmt.Printf("```\n\n")
						}
					} else {
						fmt.Printf("Warning: %s not found in remote files\n", baseName)
					}
				} else {
					fmt.Println("\nMultiple files were edited. Which would you like to view?")
					for i, path := range editedFilePaths {
						baseName := filepath.Base(path)
						fmt.Printf("  [%d] %s\n", i+1, baseName)
					}
					fmt.Printf("  [a] View all\n")
					fmt.Printf("  [n] Skip viewing (files are saved)\n")

					reader := bufio.NewReader(os.Stdin)
					fmt.Print("\nChoice: ")
					choice, _ := reader.ReadString('\n')
					choice = strings.TrimSpace(strings.ToLower(choice))

					filesToShow := []string{}
					if choice == "a" || choice == "all" {
						filesToShow = editedFilePaths
					} else if choice == "n" || choice == "" {
						fmt.Println()
					} else if choiceNum, err := strconv.Atoi(choice); err == nil && choiceNum >= 1 && choiceNum <= len(editedFilePaths) {
						filesToShow = []string{editedFilePaths[choiceNum-1]}
					}

					for _, filePath := range filesToShow {
						baseName := filepath.Base(filePath)

						if remoteArt, exists := remoteFileMap[baseName]; exists {
							content, err := client.downloadRemoteFile(conversationID, remoteArt.FileName)
							if err != nil {
								fmt.Printf("Warning: Could not download %s: %v\n", baseName, err)
								continue
							}

							fmt.Printf("\n=== Updated: %s ===\n\n", baseName)
							lang := remoteArt.Language
							if lang == "" {
								lang = "text"
							}
							fmt.Printf("```%s\n", lang)
							fmt.Print(content)
							if !strings.HasSuffix(content, "\n") {
								fmt.Println()
							}
							fmt.Printf("```\n\n")
						} else {
							fmt.Printf("Warning: %s not found in remote files\n", baseName)
						}
					}
				}
			}
		}

		// Suppress response variable
		_ = response

		if isFirstMessage {
			isFirstMessage = false
			go func() {
				client.autoGenerateTitle(conversationID, userInput)
			}()
		}
	}
}

// ============== Main ==============

func main() {
	client := NewClaudeClient()

	fmt.Println("Claude Terminal Client (gogpt)")
	fmt.Println()

	for {
		conversations := client.getConversations()

		choice, sel := pickConvo(conversations)
		if choice < 0 {
			fmt.Println("Goodbye!")
			return
		}

		var conversationID string
		var isNew bool
		var selectedModel string

		if choice == 0 {
			mid, ok := pickModel(client)
			if !ok {
				fmt.Println("Goodbye!")
				return
			}
			selectedModel = mid
			conversationID = client.createNewChat()
			isNew = true
		} else {
			conversationID = sel.UUID
			isNew = false
			selectedModel = "claude-sonnet-4-5-20250929"
		}

		if selectedModel == "" {
			selectedModel = "claude-sonnet-4-5-20250929"
		}

		continueLoop := runSimpleChat(client, conversationID, isNew, selectedModel)
		if !continueLoop {
			fmt.Println("Goodbye!")
			return
		}
	}
}
