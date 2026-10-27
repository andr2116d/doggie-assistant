package claudecode

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
)

// mcpServer is the name of DOGGIE's MCP server in the CLI's config. The CLI
// names its tools "mcp__doggie__<tool>".
const mcpServer = "doggie"

const toolPrefix = "mcp__" + mcpServer + "__"

// syntheticModel is the model of the messages the CLI writes itself.
const syntheticModel = "<synthetic>"

// event is one line of the CLI's stream-json output. Only the fields DOGGIE
// uses are decoded.
type event struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system/init, and system/status: "requesting" when the CLI sends a
	// request to the model
	Status     string `json:"status"`
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
	RateLimit *rateLimit `json:"rate_limit_info"`

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

// apiStatus is the HTTP status of the API error a result reports, or 0.
func (e *event) apiStatus() int {
	var n int
	if json.Unmarshal(e.APIErrorStatus, &n) != nil {
		return 0
	}
	return n
}

// rateLimit is how much of the subscription's usage limits is spent. Times
// are Unix seconds.
type rateLimit struct {
	// Status is "rejected" once a limit is spent, and ResetsAt is when that
	// limit renews.
	Status   string `json:"status"`
	ResetsAt int64  `json:"resetsAt"`
	Windows  map[string]struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    int64   `json:"resetsAt"`
		Status      string  `json:"status"`
	} `json:"unifiedWindows"`
}

// usage is r as the brain sees it.
func (r *rateLimit) usage() *llm.Usage {
	five, seven := r.Windows["five_hour"], r.Windows["seven_day"]
	return &llm.Usage{
		FiveHour: five.Utilization, FiveHourRenews: unixTime(five.ResetsAt),
		SevenDay: seven.Utilization, SevenDayRenews: unixTime(seven.ResetsAt),
	}
}

// spent reports whether a usage limit is spent, and when it renews (zero if
// unknown).
func (r *rateLimit) spent() (bool, time.Time) {
	if r == nil || r.Status != "rejected" {
		return false, time.Time{}
	}
	renews := r.ResetsAt
	if renews <= 0 {
		// Spent until the last spent window renews.
		for _, w := range r.Windows {
			if w.Status == "rejected" {
				renews = max(renews, w.ResetsAt)
			}
		}
	}
	return true, unixTime(renews)
}

func unixTime(s int64) time.Time {
	if s <= 0 {
		return time.Time{}
	}
	return time.Unix(s, 0)
}
