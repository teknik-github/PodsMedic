package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peceldev/podsmedic/internal/detect"
	"github.com/peceldev/podsmedic/internal/k8s"
)

// TestOpenAIDiagnoseWireFormat drives the OpenAI-compatible client against a
// stub server, asserting the request shape a DeepSeek endpoint expects and that
// a json_object reply is parsed into a Diagnosis.
func TestOpenAIDiagnoseWireFormat(t *testing.T) {
	var gotAuth, gotPath string
	var gotReq chatRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReq)

		diag := Diagnosis{
			Title: "Pod OOMKilled", Severity: "critical",
			Summary: "Killed for exceeding its memory limit.", RootCause: "128Mi limit too small",
			Evidence:    []string{"exit code 137"},
			Remediation: []Step{{Description: "Raise memory limit to 256Mi", Command: "kubectl set resources ..."}},
			Confidence:  "high",
		}
		content, _ := json.Marshal(diag)
		resp := chatResponse{}
		resp.Choices = append(resp.Choices, struct {
			Message      chatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		}{Message: chatMessage{Role: "assistant", Content: string(content)}, FinishReason: "stop"})
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := newOpenAI(Options{
		Provider: "openai", APIKey: "sk-test", BaseURL: srv.URL,
		Model: "deepseek-chat", MaxTokens: 4000,
	})

	bundle := &k8s.Bundle{Problem: detect.Problem{Namespace: "api", Pod: "web", Kind: detect.KindOOMKilled}}
	d, err := c.Diagnose(context.Background(), bundle)
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}

	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth = %q, want Bearer sk-test", gotAuth)
	}
	if gotReq.Model != "deepseek-chat" {
		t.Errorf("model = %q, want deepseek-chat", gotReq.Model)
	}
	if gotReq.ResponseFormat["type"] != "json_object" {
		t.Errorf("response_format = %v, want json_object", gotReq.ResponseFormat)
	}
	if len(gotReq.Messages) != 2 || gotReq.Messages[0].Role != "system" {
		t.Fatalf("unexpected messages: %+v", gotReq.Messages)
	}
	// json_object mode requires the literal word for DeepSeek to engage.
	if !strings.Contains(strings.ToLower(gotReq.Messages[0].Content), "json") {
		t.Error("system prompt must mention json for json_object mode")
	}
	if d.Severity != "critical" || d.Confidence != "high" {
		t.Errorf("parsed diagnosis wrong: %+v", d)
	}
}

// TestOpenAIHandlesCodeFence verifies the shared parser strips a stray markdown
// fence, which some OpenAI-compatible models add even in json_object mode.
func TestOpenAIHandlesCodeFence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := "```json\n{\"title\":\"x\",\"severity\":\"info\",\"summary\":\"s\",\"root_cause\":\"r\",\"evidence\":[],\"remediation\":[],\"confidence\":\"low\"}\n```"
		resp := chatResponse{}
		resp.Choices = append(resp.Choices, struct {
			Message      chatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		}{Message: chatMessage{Content: content}, FinishReason: "stop"})
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := newOpenAI(Options{APIKey: "k", BaseURL: srv.URL, Model: "m"})
	d, err := c.Diagnose(context.Background(), &k8s.Bundle{})
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if d.Title != "x" || d.Severity != "info" {
		t.Errorf("parsed wrong: %+v", d)
	}
}

// TestOpenAIParsesUsage checks the token accounting is lifted from the reply's
// usage block for cost metering.
func TestOpenAIParsesUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"title\":\"x\",\"severity\":\"info\",\"summary\":\"s\",\"root_cause\":\"r\",\"evidence\":[],\"remediation\":[],\"confidence\":\"low\"}"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1200, "completion_tokens": 300, "total_tokens": 1500}
		}`)
	}))
	defer srv.Close()

	c := newOpenAI(Options{APIKey: "k", BaseURL: srv.URL, Model: "m"})
	d, err := c.Diagnose(context.Background(), &k8s.Bundle{})
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if d.Usage == nil {
		t.Fatal("usage should be populated from the response")
	}
	if d.Usage.InputTokens != 1200 || d.Usage.OutputTokens != 300 {
		t.Fatalf("usage = %+v, want input=1200 output=300", d.Usage)
	}
}

// TestOpenAISurfacesTruncation ensures a length-capped reply becomes a clear
// error rather than a JSON parse failure.
func TestOpenAISurfacesTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatResponse{}
		resp.Choices = append(resp.Choices, struct {
			Message      chatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		}{Message: chatMessage{Content: "{partial"}, FinishReason: "length"})
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := newOpenAI(Options{APIKey: "k", BaseURL: srv.URL, Model: "m"})
	if _, err := c.Diagnose(context.Background(), &k8s.Bundle{}); err == nil ||
		!strings.Contains(err.Error(), "truncated") {
		t.Fatalf("expected a truncation error, got %v", err)
	}
}
