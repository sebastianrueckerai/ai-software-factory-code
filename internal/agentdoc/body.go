package agentdoc

import (
	"encoding/json"
	"fmt"
)

// ContentPart is one part of a structured message content: a text part or an
// image part (the vision call).
type ContentPart struct {
	Type     string
	Text     string
	ImageURL string
}

// Message is one message of a captured request. Content holds the decoded
// text when the content was a plain string; Parts holds it when it was a
// structured part list.
type Message struct {
	Role    string
	Content string
	Parts   []ContentPart
}

// Tool is one tool schema of a captured request, with its raw json so
// provenance can be located in the body bytes.
type Tool struct {
	Name string
	Raw  json.RawMessage
}

// Body is the parsed envelope of a captured chat request.
type Body struct {
	Model          string
	MaxTokens      int
	Stream         bool
	ResponseFormat string
	Messages       []Message
	Tools          []Tool
}

func ParseBody(raw []byte) (Body, error) {
	var wire struct {
		Model          string          `json:"model"`
		MaxTokens      int             `json:"max_tokens"`
		Stream         bool            `json:"stream"`
		ResponseFormat json.RawMessage `json:"response_format"`
		Messages       []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Body{}, fmt.Errorf("unreadable request body: %w", err)
	}
	var b Body
	b.Model, b.MaxTokens, b.Stream = wire.Model, wire.MaxTokens, wire.Stream
	var rf struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(wire.ResponseFormat, &rf) == nil {
		b.ResponseFormat = rf.Type
	}
	for _, m := range wire.Messages {
		msg := Message{Role: m.Role}
		if json.Unmarshal(m.Content, &msg.Content) != nil {
			var parts []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL *struct {
					URL string `json:"url"`
				} `json:"image_url"`
			}
			if err := json.Unmarshal(m.Content, &parts); err != nil {
				return Body{}, fmt.Errorf("message %s has neither string nor part-list content", m.Role)
			}
			for _, p := range parts {
				cp := ContentPart{Type: p.Type, Text: p.Text}
				if p.ImageURL != nil {
					cp.ImageURL = p.ImageURL.URL
				}
				msg.Parts = append(msg.Parts, cp)
			}
		}
		b.Messages = append(b.Messages, msg)
	}
	for _, raw := range wire.Tools {
		var t struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &t); err != nil || t.Function.Name == "" {
			return Body{}, fmt.Errorf("unreadable tool schema: %s", raw)
		}
		b.Tools = append(b.Tools, Tool{Name: t.Function.Name, Raw: raw})
	}
	return b, nil
}
