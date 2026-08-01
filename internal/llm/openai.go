package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/teknik-github/PodsMedic/internal/k8s"
)

// openAIClient calls any OpenAI-compatible /chat/completions endpoint. DeepSeek
// is the default target, but any provider that speaks the same wire format
// (base URL + Bearer key + chat completions) works by pointing BaseURL at it.
//
// It is a hand-rolled HTTP client rather than an SDK dependency: the request is
// a handful of JSON fields, and keeping it dependency-free means the OpenAI path
// adds nothing to the build.
type openAIClient struct {
	http      *http.Client
	baseURL   string
	apiKey    string
	model     string
	maxTokens int64
}

func newOpenAI(opts Options) *openAIClient {
	return &openAIClient{
		http:      &http.Client{Timeout: 3 * time.Minute},
		baseURL:   strings.TrimRight(opts.BaseURL, "/"),
		apiKey:    opts.APIKey,
		model:     opts.Model,
		maxTokens: opts.MaxTokens,
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string            `json:"model"`
	Messages       []chatMessage     `json:"messages"`
	MaxTokens      int64             `json:"max_tokens,omitempty"`
	ResponseFormat map[string]string `json:"response_format,omitempty"`
	Stream         bool              `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

// Diagnose sends one evidence bundle to the chat endpoint and parses the reply.
func (c *openAIClient) Diagnose(ctx context.Context, b *k8s.Bundle) (*Diagnosis, error) {
	payload, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal bundle: %w", err)
	}

	// json_object mode is the OpenAI-compatible way to force valid JSON. Unlike
	// Anthropic's json_schema it does not enforce the shape, so the schema is
	// spelled out in the prompt and validated on parse.
	content, usage, err := c.complete(ctx, []chatMessage{
		{Role: "system", Content: openAISystemPrompt()},
		{Role: "user", Content: userPrompt(payload)},
	}, true)
	if err != nil {
		return nil, err
	}

	d, err := parseDiagnosis(content)
	if err != nil {
		return nil, err
	}
	d.Usage = usage
	return d, nil
}

// Answer replies to an operator's question in prose — no JSON mode, no schema,
// and no action field for the same reason the Anthropic backend has none: the
// chat path cannot change the cluster.
func (c *openAIClient) Answer(ctx context.Context, question string, evidence []byte) (*Answer, error) {
	content, usage, err := c.complete(ctx, []chatMessage{
		{Role: "system", Content: answerSystemPrompt},
		{Role: "user", Content: answerPrompt(question, evidence)},
	}, false)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(content)
	if text == "" {
		return nil, fmt.Errorf("chat endpoint returned an empty answer")
	}
	return &Answer{Text: text, Usage: usage}, nil
}

// complete performs one chat-completions round trip and returns the assistant's
// content plus normalised token usage.
func (c *openAIClient) complete(ctx context.Context, messages []chatMessage, jsonMode bool) (string, *Usage, error) {
	reqBody := chatRequest{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		Messages:  messages,
	}
	if jsonMode {
		reqBody.ResponseFormat = map[string]string{"type": "json_object"}
	}

	raw, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("chat request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("chat endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", nil, fmt.Errorf("decode response: %w", err)
	}
	if parsed.Error != nil {
		return "", nil, fmt.Errorf("chat endpoint error (%s): %s", parsed.Error.Type, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", nil, fmt.Errorf("chat endpoint returned no choices")
	}
	choice := parsed.Choices[0]
	if choice.FinishReason == "length" {
		return "", nil, fmt.Errorf("response truncated (finish_reason=length); raise PODSMEDIC_MAX_TOKENS")
	}

	var usage *Usage
	if parsed.Usage != nil {
		usage = &Usage{InputTokens: parsed.Usage.PromptTokens, OutputTokens: parsed.Usage.CompletionTokens}
	}
	return choice.Message.Content, usage, nil
}

// openAISystemPrompt is the shared instructions plus an explicit schema. The
// literal word "json" is required for DeepSeek's json_object mode to engage,
// and spelling out the shape substitutes for the schema enforcement the
// Anthropic backend gets from json_schema.
func openAISystemPrompt() string {
	schema, _ := json.MarshalIndent(diagnosisSchema, "", "  ")
	return systemPrompt + "\n\nReturn a single json object and nothing else. It must conform to this JSON schema:\n" + string(schema)
}
