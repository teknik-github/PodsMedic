// Package config loads runtime settings from environment variables and flags.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/peceldev/podsmedic/internal/detect"
	"github.com/peceldev/podsmedic/internal/heal"
	"github.com/peceldev/podsmedic/internal/playbook"
	"github.com/peceldev/podsmedic/internal/rightsize"
)

// Config holds every knob podsmedic needs at runtime.
type Config struct {
	// Kubernetes
	Kubeconfig   string
	Namespaces   []string // empty means all namespaces
	Interval     time.Duration
	LogTailLines int64
	MaxEvents    int
	MinRestarts  int32
	// RestartWindow bounds how recent a restart must be to count as a storm.
	RestartWindow time.Duration
	// NotReadyGrace is how long a pod may stay not-ready before it is flagged.
	NotReadyGrace time.Duration
	// VolumeMountGrace is how long a scheduled pod may sit in ContainerCreating
	// before its volumes are presumed stuck.
	VolumeMountGrace time.Duration

	// Alert throttling
	// Cooldown is how long an incident may go unseen before it is considered
	// resolved (and can alert afresh if it recurs).
	Cooldown time.Duration
	// MaxAlertsPerCycle caps LLM calls per poll, so a cluster-wide outage
	// cannot turn into an unbounded API bill.
	MaxAlertsPerCycle int
	Concurrency       int

	// LLM
	Provider  string // "anthropic" or "openai" (OpenAI-compatible: DeepSeek, etc.)
	APIKey    string
	BaseURL   string // OpenAI-compatible endpoint base; ignored for anthropic
	Model     string
	MaxTokens int64
	Effort    string
	// Per-million-token prices in USD, for cost accounting. Zero disables cost
	// metering (tokens are still counted).
	PriceInputPerMTok  float64
	PriceOutputPerMTok float64

	// Notification sinks
	SlackWebhookURL  string
	TelegramBotToken string
	TelegramChatID   string

	// Telegram chat (inbound). Off by default: an inbound channel exposes
	// cluster state and spends tokens, so it is an explicit opt-in rather than
	// something a bot token alone turns on.
	TelegramListen bool
	// TelegramAllowedChats are the chat IDs permitted to ask questions. Empty
	// falls back to TelegramChatID; if that is empty too, nobody is served.
	TelegramAllowedChats []string
	// ChatMaxPerMinute caps questions per chat, bounding the token spend a chatty
	// operator can cause.
	ChatMaxPerMinute int

	// Auto-heal
	AutoHeal             bool    // master switch; off by default
	HealApply            bool    // false = server-side dry run only
	HealMaxMemory        string  // absolute ceiling for a memory bump
	HealMaxCPU           string  // absolute ceiling for a cpu bump
	HealMaxMultiplier    float64 // max growth relative to the current value
	HealMinConfidence    string  // only heal at/above this diagnosis confidence
	HealPatchRequests    bool    // allow patching requests (moves scheduling floor)
	HealMaxProbeDelay    int     // cap for probe initialDelaySeconds
	HealMaxProbePeriod   int     // cap for probe periodSeconds
	HealMaxProbeTimeout  int     // cap for probe timeoutSeconds
	HealMaxProbeFailures int     // cap for probe failureThreshold
	// HealMaxReplicas is the raw PODSMEDIC_HEAL_MAX_REPLICAS value: "auto"
	// (derive the target from load and capacity), "0" (scaling off), or a number
	// (derive, but never past this hand-set backstop).
	HealMaxReplicas string
	// HealScaleTargetCPU is the CPU utilisation a derived scale-up aims for.
	HealScaleTargetCPU float64
	// HealCapacityReserve is the fraction of each node's allocatable held back
	// from any heal, so podsmedic never fills the cluster to the brim.
	HealCapacityReserve float64
	// PVC auto-creation: the only create podsmedic performs. Off by default, and
	// additionally requires an explicit HealAllowNS.
	PVCAutoCreate      bool
	PVCDefaultSize     string
	PVCStorageClass    string
	PVCMaxSize         string
	HealAllowedKinds   []string      // detected problem kinds eligible to heal
	HealDenyNS         []string      // namespaces never healed
	HealAllowNS        []string      // if set, the only namespaces healed
	HealCooldown       time.Duration // per-workload silence between heals
	HealAllowGitOps    bool          // patch GitOps-managed workloads despite the guard
	HealVerify         bool          // re-check applied heals and roll back failures
	HealVerifyAfter    time.Duration // grace before verifying an applied heal
	HealStateName      string        // ConfigMap that persists pending heals
	IncidentStateName  string        // ConfigMap that persists open incidents; empty disables
	AuditName          string        // ConfigMap for the heal audit trail; empty disables
	AuditMaxEvents     int           // cap on retained audit events
	PlaybookName       string        // ConfigMap for the learned-heal playbook; empty disables
	PlaybookMaxEntries int           // cap on remembered remedies
	// Playbook retirement. A remedy that is only ever learned eventually
	// describes a cluster that no longer exists, so the book forgets too.
	PlaybookMaxFailures   int           // rollbacks before a workload+kind is quarantined
	PlaybookQuarantineFor time.Duration // how long a quarantined pair stays unlearnable
	PlaybookFailureDecay  time.Duration // how long a rollback counts against a pair
	PlaybookMaxAge        time.Duration // retire a remedy unconfirmed this long (0 = never)

	// Predictive heal: flag containers whose memory sits near the limit and heal
	// them before the OOM kill.
	Predict          bool    // enable prediction
	PredictHighRatio float64 // usage/limit fraction considered near-limit
	PredictMinChecks int     // consecutive high sweeps before flagging

	// Cluster-wide brakes. Every other heal limit is per workload, so N distinct
	// workloads failing at once passes all of them; these bound the whole sweep.
	HealMaxPerSweep       int     // heals allowed in one sweep (0 = unlimited)
	HealSurgeRatio        float64 // share of workloads failing that suspends healing (0 = off)
	HealSurgeMinWorkloads int     // smallest cluster the ratio is applied to

	// Circuit breaker: suspend healing a workload that keeps failing its heals.
	HealBreaker             bool          // enable the per-workload breaker
	HealBreakerWindow       time.Duration // span over which heals/rollbacks are counted
	HealBreakerMaxHeals     int           // heals in window that trip the breaker (0 = off)
	HealBreakerMaxRollbacks int           // rollbacks in window that trip the breaker (0 = off)
	HealBreakerOpenFor      time.Duration // how long healing stays suspended after a trip

	// Rightsizing. Report-only by construction: heal.Validate only ever raises a
	// value, and lowering a request moves a workload's scheduling floor and
	// eviction priority. The suggestion goes to a human.
	Rightsize           bool
	RightsizeName       string // ConfigMap holding the usage history; empty disables persistence
	RightsizeMaxTracked int
	RightsizeMinSamples int
	RightsizeMinWindow  time.Duration
	RightsizeOverRatio  float64 // peak below this fraction of the request is oversized
	RightsizeHeadroom   float64 // multiplier on the observed peak for the suggestion

	// Node health. Report-only, and separate from everything else because every
	// other signal starts from a pod: a node says it is in trouble before its
	// pods fall over. podsmedic never writes to a node, so this ends at an alert.
	NodeHealth         bool
	NodeGrace          time.Duration // how long a condition must hold before reporting
	NodeCooldown       time.Duration // silence between repeats of the same fault
	NodeReportCordoned bool          // include deliberately cordoned nodes

	// Behaviour
	DryRun bool
	// MetricsAddr is the listen address for /healthz, /readyz, /metrics. Empty
	// disables the endpoints.
	MetricsAddr string
	// UIAddr is the listen address for the live cluster view. Empty (the default)
	// disables it. Deliberately separate from MetricsAddr: /metrics is safe to
	// expose to a scraper, whereas the view shows every workload name and every
	// change podsmedic made, and has no authentication.
	UIAddr string
	// UIEventBuffer bounds how many recent events a newly-opened view replays.
	UIEventBuffer int
	// UIToken gates the live view. Required whenever UIAddr is not loopback:
	// the view lists every workload and every change podsmedic made, and it
	// speaks plain HTTP, so an unauthenticated non-local bind is refused at
	// startup rather than warned about.
	UIToken string
	// UISessionTTL is how long a live-view sign-in lasts.
	UISessionTTL time.Duration
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		Kubeconfig:          os.Getenv("KUBECONFIG"),
		MaxAlertsPerCycle:   envInt("PODSMEDIC_MAX_ALERTS_PER_CYCLE", 10),
		Concurrency:         envInt("PODSMEDIC_CONCURRENCY", 3),
		Interval:            envDuration("PODSMEDIC_INTERVAL", 60*time.Second),
		Cooldown:            envDuration("PODSMEDIC_COOLDOWN", 30*time.Minute),
		LogTailLines:        int64(envInt("PODSMEDIC_LOG_TAIL_LINES", 120)),
		MaxEvents:           envInt("PODSMEDIC_MAX_EVENTS", 25),
		MinRestarts:         int32(envInt("PODSMEDIC_MIN_RESTARTS", 3)),
		RestartWindow:       envDuration("PODSMEDIC_RESTART_WINDOW", time.Hour),
		NotReadyGrace:       envDuration("PODSMEDIC_NOT_READY_GRACE", 10*time.Minute),
		VolumeMountGrace:    envDuration("PODSMEDIC_VOLUME_MOUNT_GRACE", 2*time.Minute),
		Provider:            strings.ToLower(envString("PODSMEDIC_PROVIDER", "anthropic")),
		BaseURL:             os.Getenv("PODSMEDIC_BASE_URL"),
		MaxTokens:           int64(envInt("PODSMEDIC_MAX_TOKENS", 8000)),
		Effort:              envString("PODSMEDIC_EFFORT", "high"),
		PriceInputPerMTok:   envFloat("PODSMEDIC_LLM_PRICE_INPUT", 0),
		PriceOutputPerMTok:  envFloat("PODSMEDIC_LLM_PRICE_OUTPUT", 0),
		SlackWebhookURL:     os.Getenv("SLACK_WEBHOOK_URL"),
		TelegramBotToken:    os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramChatID:      os.Getenv("TELEGRAM_CHAT_ID"),
		TelegramListen:      envBool("PODSMEDIC_TELEGRAM_LISTEN", false),
		ChatMaxPerMinute:    envInt("PODSMEDIC_CHAT_MAX_PER_MINUTE", 6),
		DryRun:              envBool("PODSMEDIC_DRY_RUN", false),
		Rightsize:           envBool("PODSMEDIC_RIGHTSIZE", true),
		RightsizeName:       envStringAllowEmpty("PODSMEDIC_RIGHTSIZE_CONFIGMAP", "podsmedic-rightsize"),
		RightsizeMaxTracked: envInt("PODSMEDIC_RIGHTSIZE_MAX_TRACKED", rightsize.DefaultMaxTracked),
		RightsizeMinSamples: envInt("PODSMEDIC_RIGHTSIZE_MIN_SAMPLES", 60),
		RightsizeMinWindow:  envDuration("PODSMEDIC_RIGHTSIZE_MIN_WINDOW", 24*time.Hour),
		RightsizeOverRatio:  envFloat("PODSMEDIC_RIGHTSIZE_OVER_RATIO", 0.40),
		RightsizeHeadroom:   envFloat("PODSMEDIC_RIGHTSIZE_HEADROOM", 1.5),

		NodeHealth:         envBool("PODSMEDIC_NODE_HEALTH", true),
		NodeGrace:          envDuration("PODSMEDIC_NODE_GRACE", 3*time.Minute),
		NodeCooldown:       envDuration("PODSMEDIC_NODE_COOLDOWN", 2*time.Hour),
		NodeReportCordoned: envBool("PODSMEDIC_NODE_REPORT_CORDONED", false),
		MetricsAddr:        envStringAllowEmpty("PODSMEDIC_METRICS_ADDR", ":9090"),
		UIAddr:             envStringAllowEmpty("PODSMEDIC_UI_ADDR", ""),
		UIEventBuffer:      envInt("PODSMEDIC_UI_EVENT_BUFFER", 200),
		UIToken:            os.Getenv("PODSMEDIC_UI_TOKEN"),
		UISessionTTL:       envDuration("PODSMEDIC_UI_SESSION_TTL", 12*time.Hour),

		AutoHeal:             envBool("PODSMEDIC_AUTOHEAL", false),
		HealApply:            envBool("PODSMEDIC_HEAL_APPLY", false),
		HealMaxMemory:        envString("PODSMEDIC_HEAL_MAX_MEMORY", "4Gi"),
		HealMaxCPU:           envString("PODSMEDIC_HEAL_MAX_CPU", "4"),
		HealMaxMultiplier:    envFloat("PODSMEDIC_HEAL_MAX_MULTIPLIER", 4.0),
		HealMinConfidence:    envString("PODSMEDIC_HEAL_MIN_CONFIDENCE", "high"),
		HealPatchRequests:    envBool("PODSMEDIC_HEAL_PATCH_REQUESTS", false),
		HealMaxProbeDelay:    envInt("PODSMEDIC_HEAL_MAX_PROBE_DELAY", 600),
		HealMaxProbePeriod:   envInt("PODSMEDIC_HEAL_MAX_PROBE_PERIOD", 300),
		HealMaxProbeTimeout:  envInt("PODSMEDIC_HEAL_MAX_PROBE_TIMEOUT", 60),
		HealMaxProbeFailures: envInt("PODSMEDIC_HEAL_MAX_PROBE_FAILURES", 20),
		HealMaxReplicas:      strings.ToLower(envString("PODSMEDIC_HEAL_MAX_REPLICAS", "auto")),
		HealScaleTargetCPU:   envFloat("PODSMEDIC_HEAL_SCALE_TARGET_CPU", 0.70),
		HealCapacityReserve:  envFloat("PODSMEDIC_HEAL_CAPACITY_RESERVE", 0.20),
		PVCAutoCreate:        envBool("PODSMEDIC_PVC_AUTOCREATE", false),
		PVCDefaultSize:       envString("PODSMEDIC_PVC_DEFAULT_SIZE", "1Gi"),
		PVCStorageClass:      envStringAllowEmpty("PODSMEDIC_PVC_DEFAULT_CLASS", ""),
		PVCMaxSize:           envString("PODSMEDIC_PVC_MAX_SIZE", "10Gi"),
		HealAllowedKinds:     envList("PODSMEDIC_HEAL_KINDS", []string{"OOMKilled"}),
		HealDenyNS:           envList("PODSMEDIC_HEAL_DENY_NAMESPACES", []string{"kube-system", "kube-public", "kube-node-lease"}),
		HealAllowNS:          envList("PODSMEDIC_HEAL_NAMESPACES", nil),
		HealCooldown:         envDuration("PODSMEDIC_HEAL_COOLDOWN", time.Hour),
		HealAllowGitOps:      envBool("PODSMEDIC_HEAL_ALLOW_GITOPS", false),
		HealVerify:           envBool("PODSMEDIC_HEAL_VERIFY", true),
		HealVerifyAfter:      envDuration("PODSMEDIC_HEAL_VERIFY_AFTER", 10*time.Minute),
		HealStateName:        envString("PODSMEDIC_HEAL_STATE_CONFIGMAP", "podsmedic-heal-state"),
		IncidentStateName:    envStringAllowEmpty("PODSMEDIC_INCIDENT_STATE_CONFIGMAP", "podsmedic-incident-state"),
		AuditName:            envStringAllowEmpty("PODSMEDIC_AUDIT_CONFIGMAP", "podsmedic-audit"),
		AuditMaxEvents:       envInt("PODSMEDIC_AUDIT_MAX_EVENTS", 500),
		PlaybookName:         envStringAllowEmpty("PODSMEDIC_PLAYBOOK_CONFIGMAP", "podsmedic-playbook"),
		PlaybookMaxEntries:   envInt("PODSMEDIC_PLAYBOOK_MAX_ENTRIES", 500),

		PlaybookMaxFailures:   envInt("PODSMEDIC_PLAYBOOK_MAX_FAILURES", playbook.DefaultMaxFailures),
		PlaybookQuarantineFor: envDuration("PODSMEDIC_PLAYBOOK_QUARANTINE_FOR", playbook.DefaultQuarantineFor),
		PlaybookFailureDecay:  envDuration("PODSMEDIC_PLAYBOOK_FAILURE_DECAY", playbook.DefaultFailureDecay),
		PlaybookMaxAge:        envDuration("PODSMEDIC_PLAYBOOK_MAX_AGE", playbook.DefaultMaxAge),

		Predict:          envBool("PODSMEDIC_PREDICT", false),
		PredictHighRatio: envFloat("PODSMEDIC_PREDICT_MEMORY_RATIO", 0.90),
		PredictMinChecks: envInt("PODSMEDIC_PREDICT_MIN_CHECKS", 3),

		HealMaxPerSweep:       envInt("PODSMEDIC_HEAL_MAX_PER_SWEEP", 3),
		HealSurgeRatio:        envFloat("PODSMEDIC_HEAL_SURGE_RATIO", 0.25),
		HealSurgeMinWorkloads: envInt("PODSMEDIC_HEAL_SURGE_MIN_WORKLOADS", 8),

		HealBreaker:             envBool("PODSMEDIC_HEAL_BREAKER", true),
		HealBreakerWindow:       envDuration("PODSMEDIC_HEAL_BREAKER_WINDOW", 6*time.Hour),
		HealBreakerMaxHeals:     envInt("PODSMEDIC_HEAL_BREAKER_MAX_HEALS", 4),
		HealBreakerMaxRollbacks: envInt("PODSMEDIC_HEAL_BREAKER_MAX_ROLLBACKS", 2),
		HealBreakerOpenFor:      envDuration("PODSMEDIC_HEAL_BREAKER_OPEN_FOR", 6*time.Hour),
	}

	c.applyProviderDefaults()

	c.TelegramAllowedChats = envList("PODSMEDIC_TELEGRAM_ALLOWED_CHATS", nil)
	if len(c.TelegramAllowedChats) == 0 && c.TelegramChatID != "" {
		// The chat alerts already go to is the natural default for the chat
		// allowed to ask questions.
		c.TelegramAllowedChats = []string{c.TelegramChatID}
	}

	if ns := strings.TrimSpace(os.Getenv("PODSMEDIC_NAMESPACES")); ns != "" {
		for _, part := range strings.Split(ns, ",") {
			if p := strings.TrimSpace(part); p != "" {
				c.Namespaces = append(c.Namespaces, p)
			}
		}
	}

	return c, c.validate()
}

// Known OpenAI-compatible providers, so a user only has to set PODSMEDIC_PROVIDER
// and the API key rather than remember each vendor's base URL and model name.
const deepseekBaseURL = "https://api.deepseek.com"

// applyProviderDefaults fills in API key, base URL, and model based on the
// selected provider. Each is only defaulted when the user did not set it, so an
// explicit PODSMEDIC_MODEL or PODSMEDIC_BASE_URL always wins.
func (c *Config) applyProviderDefaults() {
	switch c.Provider {
	case "openai", "deepseek":
		// "deepseek" is accepted as a friendly alias for the OpenAI-compatible
		// path pointed at DeepSeek's endpoint.
		c.Provider = "openai"
		c.APIKey = firstNonEmpty(
			os.Getenv("PODSMEDIC_API_KEY"),
			os.Getenv("DEEPSEEK_API_KEY"),
			os.Getenv("OPENAI_API_KEY"),
		)
		if c.BaseURL == "" {
			c.BaseURL = deepseekBaseURL
		}
		if os.Getenv("PODSMEDIC_MODEL") == "" {
			c.Model = "deepseek-chat"
		} else {
			c.Model = os.Getenv("PODSMEDIC_MODEL")
		}
	default:
		c.Provider = "anthropic"
		c.APIKey = os.Getenv("ANTHROPIC_API_KEY")
		c.Model = envString("PODSMEDIC_MODEL", "claude-opus-4-8")
	}
}

func (c *Config) validate() error {
	if c.APIKey == "" {
		if c.Provider == "openai" {
			return errors.New("API key is required: set PODSMEDIC_API_KEY (or DEEPSEEK_API_KEY / OPENAI_API_KEY)")
		}
		return errors.New("ANTHROPIC_API_KEY is required")
	}
	if c.Provider == "openai" && c.BaseURL == "" {
		return errors.New("PODSMEDIC_BASE_URL is required for the openai provider")
	}
	if c.Interval < 10*time.Second {
		return fmt.Errorf("PODSMEDIC_INTERVAL must be at least 10s, got %s", c.Interval)
	}
	if !c.DryRun && c.SlackWebhookURL == "" && c.TelegramBotToken == "" {
		return errors.New("no notifier configured: set SLACK_WEBHOOK_URL or TELEGRAM_BOT_TOKEN (or PODSMEDIC_DRY_RUN=true)")
	}
	if c.TelegramBotToken != "" && c.TelegramChatID == "" {
		return errors.New("TELEGRAM_CHAT_ID is required when TELEGRAM_BOT_TOKEN is set")
	}
	if c.AutoHeal {
		if _, err := c.HealOptions(); err != nil {
			return err
		}
	}
	return nil
}

// HealOptions builds the validated auto-heal options, parsing the resource caps
// so a bad quantity fails at startup rather than at heal time.
func (c *Config) HealOptions() (heal.Options, error) {
	maxMem, err := resource.ParseQuantity(c.HealMaxMemory)
	if err != nil {
		return heal.Options{}, fmt.Errorf("PODSMEDIC_HEAL_MAX_MEMORY %q: %w", c.HealMaxMemory, err)
	}
	maxCPU, err := resource.ParseQuantity(c.HealMaxCPU)
	if err != nil {
		return heal.Options{}, fmt.Errorf("PODSMEDIC_HEAL_MAX_CPU %q: %w", c.HealMaxCPU, err)
	}

	autoReplicas, maxReplicas, err := parseReplicaCap(c.HealMaxReplicas)
	if err != nil {
		return heal.Options{}, err
	}
	// Range-check the ratios here so a typo fails at startup rather than being
	// discovered as a declined heal during an incident.
	if c.HealScaleTargetCPU <= 0 || c.HealScaleTargetCPU > 1 {
		return heal.Options{}, fmt.Errorf("PODSMEDIC_HEAL_SCALE_TARGET_CPU %.2f: must be greater than 0 and at most 1", c.HealScaleTargetCPU)
	}
	if c.HealCapacityReserve < 0 || c.HealCapacityReserve >= 1 {
		return heal.Options{}, fmt.Errorf("PODSMEDIC_HEAL_CAPACITY_RESERVE %.2f: must be at least 0 and below 1", c.HealCapacityReserve)
	}

	pvcMax, err := resource.ParseQuantity(c.PVCMaxSize)
	if err != nil {
		return heal.Options{}, fmt.Errorf("PODSMEDIC_PVC_MAX_SIZE %q: %w", c.PVCMaxSize, err)
	}
	if c.PVCAutoCreate {
		if _, err := resource.ParseQuantity(c.PVCDefaultSize); err != nil {
			return heal.Options{}, fmt.Errorf("PODSMEDIC_PVC_DEFAULT_SIZE %q: %w", c.PVCDefaultSize, err)
		}
	}

	kinds := make(map[detect.Kind]bool, len(c.HealAllowedKinds))
	for _, k := range c.HealAllowedKinds {
		kinds[detect.Kind(k)] = true
	}
	return heal.Options{
		AutoReplicas:                autoReplicas,
		TargetCPURatio:              c.HealScaleTargetCPU,
		MaxMemory:                   maxMem,
		MaxCPU:                      maxCPU,
		MaxMultiplier:               c.HealMaxMultiplier,
		MinConfidence:               c.HealMinConfidence,
		AllowRequests:               c.HealPatchRequests,
		MaxProbeInitialDelaySeconds: int32(c.HealMaxProbeDelay),
		MaxProbePeriodSeconds:       int32(c.HealMaxProbePeriod),
		MaxProbeTimeoutSeconds:      int32(c.HealMaxProbeTimeout),
		MaxProbeFailureThreshold:    int32(c.HealMaxProbeFailures),
		MaxReplicas:                 maxReplicas,
		PVCAutoCreate:               c.PVCAutoCreate,
		PVCDefaultSize:              c.PVCDefaultSize,
		PVCStorageClass:             c.PVCStorageClass,
		PVCMaxSize:                  pvcMax,
		AllowedKinds:                kinds,
		DenyNamespaces:              toSet(c.HealDenyNS),
		AllowNamespaces:             toSet(c.HealAllowNS),
	}, nil
}

// parseReplicaCap reads PODSMEDIC_HEAL_MAX_REPLICAS, which is tri-state:
//
//	"auto" (default) — derive the target from measured load and free capacity,
//	                   with no hand-set ceiling over it
//	"0"              — scaling disabled entirely
//	a number         — still derive the target, but never exceed this backstop
//
// A number therefore narrows the automatic behaviour rather than replacing it,
// so an operator keeps a hard ceiling without going back to picking the replica
// count by hand.
func parseReplicaCap(raw string) (auto bool, max int32, err error) {
	switch v := strings.TrimSpace(raw); v {
	case "", "auto":
		return true, 0, nil
	default:
		n, convErr := strconv.Atoi(v)
		if convErr != nil {
			return false, 0, fmt.Errorf("PODSMEDIC_HEAL_MAX_REPLICAS %q: want \"auto\", 0, or a positive replica count", raw)
		}
		if n < 0 {
			return false, 0, fmt.Errorf("PODSMEDIC_HEAL_MAX_REPLICAS %q: must not be negative", raw)
		}
		if n == 0 {
			return false, 0, nil // scaling off
		}
		return true, int32(n), nil
	}
}

func toSet(vals []string) map[string]bool {
	if len(vals) == 0 {
		return nil
	}
	m := make(map[string]bool, len(vals))
	for _, v := range vals {
		m[v] = true
	}
	return m
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envStringAllowEmpty is like envString but an explicitly empty value is
// honoured (returns ""), so a setting can be cleared to disable a feature.
func envStringAllowEmpty(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// envList parses a comma-separated env var, returning def when it is unset.
// An explicit empty string yields an empty (non-nil) list, so a user can clear
// a default such as the namespace denylist.
func envList(key string, def []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
