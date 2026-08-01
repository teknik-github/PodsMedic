package config

import (
	"strings"
	"testing"
)

func TestDeepseekAliasResolvesOpenAIDefaults(t *testing.T) {
	t.Setenv("PODSMEDIC_PROVIDER", "deepseek")
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	t.Setenv("PODSMEDIC_DRY_RUN", "true")

	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Provider != "openai" {
		t.Errorf("provider = %q, want openai", c.Provider)
	}
	if c.BaseURL != deepseekBaseURL {
		t.Errorf("baseURL = %q, want %q", c.BaseURL, deepseekBaseURL)
	}
	if c.Model != "deepseek-chat" {
		t.Errorf("model = %q, want deepseek-chat", c.Model)
	}
	if c.APIKey != "sk-test" {
		t.Errorf("apiKey = %q, want sk-test", c.APIKey)
	}
}

func TestOpenAIProviderMissingKeyFails(t *testing.T) {
	t.Setenv("PODSMEDIC_PROVIDER", "openai")
	t.Setenv("PODSMEDIC_DRY_RUN", "true")

	if _, err := Load(); err == nil {
		t.Fatal("expected an error when the openai API key is unset")
	}
}

func TestExplicitModelAndBaseURLOverrideDefaults(t *testing.T) {
	t.Setenv("PODSMEDIC_PROVIDER", "openai")
	t.Setenv("PODSMEDIC_API_KEY", "sk-test")
	t.Setenv("PODSMEDIC_BASE_URL", "https://llm.internal/v1")
	t.Setenv("PODSMEDIC_MODEL", "custom-model")
	t.Setenv("PODSMEDIC_DRY_RUN", "true")

	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.BaseURL != "https://llm.internal/v1" {
		t.Errorf("baseURL = %q, want the explicit value", c.BaseURL)
	}
	if c.Model != "custom-model" {
		t.Errorf("model = %q, want custom-model", c.Model)
	}
}

func TestDefaultProviderIsAnthropic(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	t.Setenv("PODSMEDIC_DRY_RUN", "true")

	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", c.Provider)
	}
	if c.Model != "claude-opus-4-8" {
		t.Errorf("model = %q, want claude-opus-4-8", c.Model)
	}
}

func TestParseReplicaCap(t *testing.T) {
	cases := []struct {
		raw      string
		wantAuto bool
		wantMax  int32
		wantErr  bool
	}{
		{"auto", true, 0, false}, // derive, no hand-set ceiling
		{"", true, 0, false},     // unset behaves as auto
		{"AUTO", true, 0, false}, // Load lowercases before parsing
		{"0", false, 0, false},   // scaling disabled entirely
		{"8", true, 8, false},    // derive, but never past 8
		{"-1", false, 0, true},   // nonsense
		{"lots", false, 0, true}, // nonsense
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			auto, max, err := parseReplicaCap(strings.ToLower(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if auto != tc.wantAuto || max != tc.wantMax {
				t.Fatalf("got auto=%v max=%d, want auto=%v max=%d", auto, max, tc.wantAuto, tc.wantMax)
			}
		})
	}
}

func TestHealOptionsDefaultsToAutoReplicas(t *testing.T) {
	t.Setenv("PODSMEDIC_PROVIDER", "anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("PODSMEDIC_DRY_RUN", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	opts, err := cfg.HealOptions()
	if err != nil {
		t.Fatalf("heal options: %v", err)
	}
	if !opts.AutoReplicas {
		t.Fatal("replica count should be derived by default, not hand-set")
	}
	if opts.MaxReplicas != 0 {
		t.Fatalf("expected no hand-set backstop by default, got %d", opts.MaxReplicas)
	}
	if opts.TargetCPURatio <= 0 || opts.TargetCPURatio > 1 {
		t.Fatalf("target CPU ratio %.2f is not a usable default", opts.TargetCPURatio)
	}
}
