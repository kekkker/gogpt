package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ============== MCP Protocol Types ==============

type MCPServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

type MCPConfig struct {
	MCPServers map[string]MCPServerConfig `json:"mcpServers"`
}

type MCPTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
	ServerName  string
}

type JSONRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *int        `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ============== MCP Client ==============

type MCPClient struct {
	name      string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	stderr    io.ReadCloser
	requestID int
	tools     []MCPTool
	mu        sync.Mutex
	ready     bool
}

func NewMCPClient(name string, command string, args []string, env map[string]string) (*MCPClient, error) {
	cmd := exec.Command(command, args...)

	// Set up environment
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start MCP server: %w", err)
	}

	client := &MCPClient{
		name:   name,
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		stderr: stderr,
	}

	// Drain stderr in background to prevent blocking
	go func() {
		stderrReader := bufio.NewReader(stderr)
		for {
			line, err := stderrReader.ReadString('\n')
			if err != nil {
				return
			}
			// Uncomment for debugging:
			// fmt.Fprintf(os.Stderr, "[MCP %s stderr] %s", name, line)
			_ = line
		}
	}()

	// Give the server a moment to start
	time.Sleep(100 * time.Millisecond)

	// Initialize the MCP server
	if err := client.initialize(); err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to initialize MCP server: %w", err)
	}

	// Get available tools
	if err := client.listTools(); err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to list tools: %w", err)
	}

	client.ready = true
	return client, nil
}

func (c *MCPClient) sendRequest(method string, params interface{}) (*JSONRPCResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.requestID++
	id := c.requestID
	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	// MCP uses newline-delimited JSON
	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("failed to write to MCP server: %w", err)
	}

	// Read response with timeout
	responseChan := make(chan string, 1)
	errChan := make(chan error, 1)

	go func() {
		line, err := c.stdout.ReadString('\n')
		if err != nil {
			errChan <- err
			return
		}
		responseChan <- line
	}()

	select {
	case line := <-responseChan:
		var resp JSONRPCResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			return nil, fmt.Errorf("failed to parse MCP response: %w (raw: %s)", err, line)
		}

		if resp.Error != nil {
			return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
		}

		return &resp, nil

	case err := <-errChan:
		return nil, fmt.Errorf("failed to read from MCP server: %w", err)

	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("timeout waiting for MCP response")
	}
}

func (c *MCPClient) sendNotification(method string, params interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	req := JSONRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return err
	}

	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write notification: %w", err)
	}

	return nil
}

func (c *MCPClient) initialize() error {
	params := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]interface{}{
			"roots": map[string]interface{}{
				"listChanged": false,
			},
		},
		"clientInfo": map[string]interface{}{
			"name":    "gogpt-cli",
			"version": "1.0.0",
		},
	}

	_, err := c.sendRequest("initialize", params)
	if err != nil {
		return err
	}

	// Send initialized notification
	return c.sendNotification("notifications/initialized", nil)
}

func (c *MCPClient) listTools() error {
	resp, err := c.sendRequest("tools/list", map[string]interface{}{})
	if err != nil {
		return err
	}

	var result struct {
		Tools []struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			InputSchema map[string]interface{} `json:"inputSchema"`
		} `json:"tools"`
	}

	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("failed to parse tools list: %w", err)
	}

	c.tools = make([]MCPTool, len(result.Tools))
	for i, t := range result.Tools {
		c.tools[i] = MCPTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			ServerName:  c.name,
		}
	}

	return nil
}

func (c *MCPClient) CallTool(name string, arguments map[string]interface{}) (string, error) {
	params := map[string]interface{}{
		"name":      name,
		"arguments": arguments,
	}

	resp, err := c.sendRequest("tools/call", params)
	if err != nil {
		return "", err
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}

	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", fmt.Errorf("failed to parse tool result: %w", err)
	}

	// Concatenate all text content
	var sb strings.Builder
	for _, content := range result.Content {
		if content.Type == "text" {
			sb.WriteString(content.Text)
		}
	}

	if result.IsError {
		return "", fmt.Errorf("tool error: %s", sb.String())
	}

	return sb.String(), nil
}

func (c *MCPClient) GetTools() []MCPTool {
	return c.tools
}

func (c *MCPClient) Close() error {
	if c.stdin != nil {
		c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Kill()
		c.cmd.Wait()
	}
	return nil
}

// ============== MCP Manager ==============

type MCPManager struct {
	clients map[string]*MCPClient
	tools   map[string]*MCPClient // tool name -> client that owns it
	mu      sync.RWMutex
}

func NewMCPManager() *MCPManager {
	return &MCPManager{
		clients: make(map[string]*MCPClient),
		tools:   make(map[string]*MCPClient),
	}
}

func (m *MCPManager) LoadConfig() error {
	configPath := getPath("mcp.json")

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create default config with example
			defaultConfig := MCPConfig{
				MCPServers: map[string]MCPServerConfig{
					"_example_zig-docs": {
						Command: "npx",
						Args:    []string{"-y", "zig-mcp@latest"},
					},
				},
			}
			data, _ := json.MarshalIndent(defaultConfig, "", "  ")
			os.WriteFile(configPath, data, 0644)
			fmt.Printf("Created MCP config at %s\n", configPath)
			fmt.Println("Edit this file to add MCP servers (remove '_example_' prefix to enable)")
			return nil
		}
		return err
	}

	var config MCPConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse MCP config: %w", err)
	}

	for name, server := range config.MCPServers {
		// Skip example entries
		if strings.HasPrefix(name, "_") {
			continue
		}

		fmt.Printf("Starting MCP server: %s... ", name)
		client, err := NewMCPClient(name, server.Command, server.Args, server.Env)
		if err != nil {
			fmt.Printf("FAILED: %v\n", err)
			continue
		}

		m.mu.Lock()
		m.clients[name] = client
		for _, tool := range client.GetTools() {
			m.tools[tool.Name] = client
		}
		m.mu.Unlock()

		fmt.Printf("OK (%d tools)\n", len(client.GetTools()))
	}

	return nil
}

func (m *MCPManager) GetAllTools() []MCPTool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var tools []MCPTool
	for _, client := range m.clients {
		tools = append(tools, client.GetTools()...)
	}
	return tools
}

func (m *MCPManager) HasTools() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.tools) > 0
}

func (m *MCPManager) CallTool(name string, arguments map[string]interface{}) (string, error) {
	m.mu.RLock()
	client, ok := m.tools[name]
	m.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("unknown MCP tool: %s", name)
	}
	return client.CallTool(name, arguments)
}

func (m *MCPManager) GetToolsPrompt() string {
	tools := m.GetAllTools()
	if len(tools) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n\n---\nYou have access to the following external tools via MCP (Model Context Protocol):\n\n")

	for _, tool := range tools {
		sb.WriteString(fmt.Sprintf("### Tool: %s\n", tool.Name))
		sb.WriteString(fmt.Sprintf("Server: %s\n", tool.ServerName))
		sb.WriteString(fmt.Sprintf("Description: %s\n", tool.Description))

		if tool.InputSchema != nil {
			if props, ok := tool.InputSchema["properties"].(map[string]interface{}); ok {
				sb.WriteString("Parameters:\n")
				required := make(map[string]bool)
				if reqList, ok := tool.InputSchema["required"].([]interface{}); ok {
					for _, r := range reqList {
						if rs, ok := r.(string); ok {
							required[rs] = true
						}
					}
				}
				for paramName, paramInfo := range props {
					reqMark := ""
					if required[paramName] {
						reqMark = " (required)"
					}
					if paramMap, ok := paramInfo.(map[string]interface{}); ok {
						paramType, _ := paramMap["type"].(string)
						paramDesc, _ := paramMap["description"].(string)
						sb.WriteString(fmt.Sprintf("  - %s: %s%s", paramName, paramType, reqMark))
						if paramDesc != "" {
							sb.WriteString(fmt.Sprintf(" - %s", paramDesc))
						}
						sb.WriteString("\n")
					}
				}
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString(`To use an MCP tool, you MUST follow this EXACT protocol:

1. Output the tool call on its own line in this EXACT format:
<mcp_tool_call>
{"tool": "tool_name", "arguments": {"param1": "value1"}}
</mcp_tool_call>

2. IMMEDIATELY STOP writing after the closing </mcp_tool_call> tag
3. Do NOT continue your response - wait for the tool result
4. The tool result will be provided in a follow-up message
5. Only then should you continue with your response

CRITICAL RULES:
- Output ONLY ONE tool call, then STOP completely
- Do NOT write any text after </mcp_tool_call>
- Do NOT say "let me wait" or "I'll check" - just output the tag and stop
- Do NOT provide fallback answers - trust that the tool will respond
- If you need multiple tools, call them one at a time, waiting for each result
---

`)

	return sb.String()
}

func (m *MCPManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, client := range m.clients {
		fmt.Printf("Stopping MCP server: %s\n", name)
		client.Close()
	}
	m.clients = make(map[string]*MCPClient)
	m.tools = make(map[string]*MCPClient)
}

// ============== Tool Call Parser ==============

type MCPToolCall struct {
	Tool      string                 `json:"tool"`
	Arguments map[string]interface{} `json:"arguments"`
}

func ParseMCPToolCalls(text string) []MCPToolCall {
	var calls []MCPToolCall

	// Find all <mcp_tool_call>...</mcp_tool_call> blocks
	startTag := "<mcp_tool_call>"
	endTag := "</mcp_tool_call>"

	remaining := text
	for {
		startIdx := strings.Index(remaining, startTag)
		if startIdx == -1 {
			break
		}

		endIdx := strings.Index(remaining[startIdx:], endTag)
		if endIdx == -1 {
			break
		}

		jsonContent := remaining[startIdx+len(startTag) : startIdx+endIdx]
		jsonContent = strings.TrimSpace(jsonContent)

		var call MCPToolCall
		if err := json.Unmarshal([]byte(jsonContent), &call); err == nil {
			if call.Tool != "" {
				calls = append(calls, call)
			}
		}

		remaining = remaining[startIdx+endIdx+len(endTag):]
	}

	return calls
}

func StripMCPToolCalls(text string) string {
	// Remove all <mcp_tool_call>...</mcp_tool_call> blocks from display
	startTag := "<mcp_tool_call>"
	endTag := "</mcp_tool_call>"

	result := text
	for {
		startIdx := strings.Index(result, startTag)
		if startIdx == -1 {
			break
		}

		endIdx := strings.Index(result[startIdx:], endTag)
		if endIdx == -1 {
			break
		}

		result = result[:startIdx] + result[startIdx+endIdx+len(endTag):]
	}

	// Clean up extra newlines
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}

	return strings.TrimSpace(result)
}
