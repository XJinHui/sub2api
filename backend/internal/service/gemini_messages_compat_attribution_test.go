package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConvertClaudeMessagesToGeminiGenerateContent_ClientSystemMetadata(t *testing.T) {
	const (
		billing      = "x-anthropic-billing-header: cc_version=2.1.288.9ae; cc_entrypoint=local-agent;"
		identity     = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
		instructions = "Follow CLAUDE.md. Keep Anthropic API examples unchanged."
	)
	tests := []struct {
		name   string
		system any
		want   string
	}{
		{
			name: "separate Claude SDK blocks",
			system: []any{
				map[string]any{"type": "text", "text": billing},
				map[string]any{"type": "text", "text": identity, "cache_control": map[string]any{"type": "ephemeral"}},
				map[string]any{"type": "text", "text": instructions},
			},
			want: "You are an AI agent.\n" + instructions,
		},
		{
			name:   "combined string with CRLF",
			system: " \t" + billing + "\r\n" + identity + "\n" + instructions,
			want:   "You are an AI agent.\n" + instructions,
		},
		{
			name:   "combined text block",
			system: []any{map[string]any{"type": "text", "text": billing + "\n" + identity + "\n" + instructions}},
			want:   "You are an AI agent.\n" + instructions,
		},
		{
			name:   "Claude Code CLI opener",
			system: "You are Claude Code, Anthropic's official CLI for Claude.\n" + instructions,
			want:   "You are an AI agent.\n" + instructions,
		},
		{name: "metadata only", system: billing, want: ""},
		{
			name:   "ordinary vendor references",
			system: instructions + "\nExample: " + identity + "\n" + billing,
			want:   instructions + "\nExample: " + identity + "\n" + billing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// These same literals are conversation/tool content, not system metadata.
			userText := billing + "\n" + identity
			body, err := json.Marshal(map[string]any{
				"model": "gemini-3.8-flash", "max_tokens": 128,
				"system":   tt.system,
				"messages": []any{map[string]any{"role": "user", "content": userText}},
				"tools": []any{map[string]any{
					"name": "example", "description": userText,
					"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
				}},
			})
			require.NoError(t, err)
			out, err := convertClaudeMessagesToGeminiGenerateContent(body, "gemini-3.8-flash")
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, json.Unmarshal(out, &got))
			if tt.want == "" {
				require.NotContains(t, got, "systemInstruction")
			} else {
				system, ok := got["systemInstruction"].(map[string]any)
				require.True(t, ok)
				parts, ok := system["parts"].([]any)
				require.True(t, ok)
				require.NotEmpty(t, parts)
				part, ok := parts[0].(map[string]any)
				require.True(t, ok)
				require.Equal(t, tt.want, part["text"])
			}
			contents, ok := got["contents"].([]any)
			require.True(t, ok)
			require.NotEmpty(t, contents)
			content, ok := contents[0].(map[string]any)
			require.True(t, ok)
			contentParts, ok := content["parts"].([]any)
			require.True(t, ok)
			require.NotEmpty(t, contentParts)
			contentPart, ok := contentParts[0].(map[string]any)
			require.True(t, ok)
			require.Equal(t, userText, contentPart["text"])

			tools, ok := got["tools"].([]any)
			require.True(t, ok)
			require.NotEmpty(t, tools)
			tool, ok := tools[0].(map[string]any)
			require.True(t, ok)
			declarations, ok := tool["functionDeclarations"].([]any)
			require.True(t, ok)
			require.NotEmpty(t, declarations)
			declaration, ok := declarations[0].(map[string]any)
			require.True(t, ok)
			require.Equal(t, userText, declaration["description"])
		})
	}
}
