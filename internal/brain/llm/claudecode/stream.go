package claudecode

import (
	"encoding/json"
	"strings"
)

// mcpServer is the name of DOGGIE's MCP server in the CLI's config. The CLI
// names its tools "mcp__doggie__<tool>".
const mcpServer = "doggie"

const toolPrefix = "mcp__" + mcpServer + "__"

// event is one line of the CLI's stream-json output. Only the fields DOGGIE
// uses are decoded.
type event struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system/init
	Model      string `json:"model"`
	MCPServers []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`

	// stream_event (partial messages)
	Event *struct {
		Delta *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`

	// assistant
	Message *struct {
		Model   string `json:"model"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`

	// rate_limit_event
	RateLimit *struct {
		Windows map[string]struct {
			Utilization float64 `json:"utilization"`
		} `json:"unifiedWindows"`
	} `json:"rate_limit_info"`

	// result
	IsError        bool            `json:"is_error"`
	Result         string          `json:"result"`
	APIErrorStatus json.RawMessage `json:"api_error_status"`
}

// toolsConnected reports whether an init event lists DOGGIE's MCP server as
// connected.
func (e *event) toolsConnected() bool {
	for _, s := range e.MCPServers {
		if s.Name == mcpServer {
			return s.Status == "connected"
		}
	}
	return false
}

// isTextDelta reports whether a stream event carries text.
func (e *event) isTextDelta() bool {
	return e.Event != nil && e.Event.Delta != nil && e.Event.Delta.Type == "text_delta" && e.Event.Delta.Text != ""
}

// failure describes a result that is an error, or "" for a success.
func (e *event) failure() string {
	if !e.IsError && e.Subtype == "success" {
		return ""
	}
	msg := strings.TrimSpace(e.Result)
	if msg == "" {
		msg = e.Subtype
	}
	if s := string(e.APIErrorStatus); s != "" && s != "null" {
		msg += " (API status " + s + ")"
	}
	if msg == "" {
		msg = "unknown error"
	}
	return msg
}
