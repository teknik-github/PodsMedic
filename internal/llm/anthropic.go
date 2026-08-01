package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/teknik-github/PodsMedic/internal/k8s"
)

// anthropicClient calls the native Claude Messages API.
type anthropicClient struct {
	api       anthropic.Client
	model     anthropic.Model
	maxTokens int64
	effort    anthropic.OutputConfigEffort
}

func newAnthropic(opts Options) *anthropicClient {
	return &anthropicClient{
		api:       anthropic.NewClient(option.WithAPIKey(opts.APIKey)),
		model:     anthropic.Model(opts.Model),
		maxTokens: opts.MaxTokens,
		effort:    anthropic.OutputConfigEffort(opts.Effort),
	}
}

// Diagnose sends one evidence bundle to Claude and returns the parsed verdict.
func (c *anthropicClient) Diagnose(ctx context.Context, b *k8s.Bundle) (*Diagnosis, error) {
	payload, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal bundle: %w", err)
	}

	resp, err := c.api.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		// Adaptive thinking lets Claude decide how much reasoning a given
		// failure warrants; effort caps the overall spend.
		Thinking: anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
		},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: c.effort,
			Format: anthropic.JSONOutputFormatParam{Schema: diagnosisSchema},
		},
		// The system prompt is the stable prefix; caching it means only the
		// per-pod evidence is billed at full rate on subsequent alerts.
		System: []anthropic.TextBlockParam{{
			Text:         systemPrompt,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userPrompt(payload))),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("claude request: %w", err)
	}

	switch resp.StopReason {
	case anthropic.StopReasonRefusal:
		return nil, fmt.Errorf("claude refused the request: %s", resp.StopDetails.Explanation)
	case anthropic.StopReasonMaxTokens:
		return nil, fmt.Errorf("response truncated at max_tokens=%d; raise PODSMEDIC_MAX_TOKENS", c.maxTokens)
	}

	var text strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	if text.Len() == 0 {
		return nil, fmt.Errorf("claude returned no text content (stop_reason=%s)", resp.StopReason)
	}

	d, err := parseDiagnosis(text.String())
	if err != nil {
		return nil, err
	}
	// Claude bills cached prefix reads separately; surface them so cost
	// accounting reflects the caching discount.
	d.Usage = &Usage{
		InputTokens:     resp.Usage.InputTokens,
		OutputTokens:    resp.Usage.OutputTokens,
		CacheReadTokens: resp.Usage.CacheReadInputTokens,
	}
	return d, nil
}

// Answer replies to an operator's question in prose. Unlike Diagnose there is
// no output schema — a chat reply is text — and no action is requested.
func (c *anthropicClient) Answer(ctx context.Context, question string, evidence []byte) (*Answer, error) {
	resp, err := c.api.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		Thinking: anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
		},
		OutputConfig: anthropic.OutputConfigParam{Effort: c.effort},
		System: []anthropic.TextBlockParam{{
			Text:         answerSystemPrompt,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(answerPrompt(question, evidence))),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("claude request: %w", err)
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return nil, fmt.Errorf("claude refused the request: %s", resp.StopDetails.Explanation)
	}

	var text strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	if text.Len() == 0 {
		return nil, fmt.Errorf("claude returned no text content (stop_reason=%s)", resp.StopReason)
	}

	return &Answer{
		Text: strings.TrimSpace(text.String()),
		Usage: &Usage{
			InputTokens:     resp.Usage.InputTokens,
			OutputTokens:    resp.Usage.OutputTokens,
			CacheReadTokens: resp.Usage.CacheReadInputTokens,
		},
	}, nil
}

// parseDiagnosis unmarshals the model's JSON reply, tolerating a stray code
// fence some models wrap around structured output.
func parseDiagnosis(text string) (*Diagnosis, error) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
	}

	var d Diagnosis
	if err := json.Unmarshal([]byte(text), &d); err != nil {
		return nil, fmt.Errorf("parse diagnosis JSON: %w", err)
	}
	return &d, nil
}
