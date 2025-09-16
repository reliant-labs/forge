package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/reliant-labs/forge/internal/mcp/tools"
)

const (
	serverName    = "forge-mcp"
	serverVersion = "0.1.0"
)

type MCPMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *MCPError       `json:"error,omitempty"`
}

type MCPError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ServerCapabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

func main() {
	log.SetFlags(0)
	log.SetOutput(os.Stderr)

	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)

	log.Println("forge MCP Server starting...")

	for {
		var msg MCPMessage
		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				log.Println("Connection closed by client")
			} else {
				log.Printf("Error decoding message: %v", err)
			}
			break
		}

		// Handle notifications (no response required)
		if msg.ID == nil {
			switch msg.Method {
			case "notifications/initialized", "initialized":
				log.Println("Received initialized notification")
			case "notifications/cancelled":
				log.Println("Received cancelled notification")
			default:
				log.Printf("Received unknown notification: %s", msg.Method)
			}
			continue
		}

		response := handleMessage(msg)

		if err := encoder.Encode(response); err != nil {
			log.Printf("Error encoding response: %v", err)
			break
		}
	}
}

func handleMessage(msg MCPMessage) MCPMessage {
	log.Printf("Received method: %s", msg.Method)

	switch msg.Method {
	case "initialize":
		return handleInitialize(msg)
	case "ping":
		return handlePing(msg)
	case "tools/list":
		return handleToolsList(msg)
	case "tools/call":
		return handleToolCall(msg)
	default:
		return MCPMessage{
			JSONRPC: "2.0",
			ID:      msg.ID,
			Error: &MCPError{
				Code:    -32601,
				Message: fmt.Sprintf("Method not found: %s", msg.Method),
			},
		}
	}
}

func handleInitialize(msg MCPMessage) MCPMessage {
	return MCPMessage{
		JSONRPC: "2.0",
		ID:      msg.ID,
		Result: map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities": ServerCapabilities{
				Tools: &ToolsCapability{},
			},
			"serverInfo": ServerInfo{
				Name:    serverName,
				Version: serverVersion,
			},
		},
	}
}

func handlePing(msg MCPMessage) MCPMessage {
	return MCPMessage{
		JSONRPC: "2.0",
		ID:      msg.ID,
		Result:  map[string]interface{}{},
	}
}

func handleToolsList(msg MCPMessage) MCPMessage {
	return MCPMessage{
		JSONRPC: "2.0",
		ID:      msg.ID,
		Result: map[string]interface{}{
			"tools": tools.AllTools(),
		},
	}
}

func handleToolCall(msg MCPMessage) MCPMessage {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}

	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return MCPMessage{
			JSONRPC: "2.0",
			ID:      msg.ID,
			Error: &MCPError{
				Code:    -32602,
				Message: fmt.Sprintf("Invalid params: %v", err),
			},
		}
	}

	log.Printf("Tool call: %s", params.Name)

	result, err := tools.ExecuteTool(params.Name, params.Arguments)
	if err != nil {
		// Per MCP spec, tool execution errors are returned as content with isError,
		// not as JSON-RPC protocol errors. Protocol errors (-32xxx) are only for
		// malformed requests, not tool failures.
		return MCPMessage{
			JSONRPC: "2.0",
			ID:      msg.ID,
			Result: map[string]interface{}{
				"content": []map[string]interface{}{
					{
						"type": "text",
						"text": fmt.Sprintf("Error: %s", err.Error()),
					},
				},
				"isError": true,
			},
		}
	}

	return MCPMessage{
		JSONRPC: "2.0",
		ID:      msg.ID,
		Result: map[string]interface{}{
			"content": []map[string]interface{}{
				{
					"type": "text",
					"text": result,
				},
			},
		},
	}
}
