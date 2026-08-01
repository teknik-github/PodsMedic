// Command podsmedic watches a Kubernetes cluster for failing pods, asks Claude
// to diagnose them, and posts the explanation to Slack or Telegram.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/peceldev/podsmedic/internal/agent"
	"github.com/peceldev/podsmedic/internal/chat"
	"github.com/peceldev/podsmedic/internal/config"
	"github.com/peceldev/podsmedic/internal/k8s"
	"github.com/peceldev/podsmedic/internal/live"
	"github.com/peceldev/podsmedic/internal/llm"
	"github.com/peceldev/podsmedic/internal/metrics"
	"github.com/peceldev/podsmedic/internal/notify"
	"github.com/peceldev/podsmedic/internal/ui"

	corev1 "k8s.io/api/core/v1"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))

	if err := run(log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	kube, err := k8s.New(cfg.Kubeconfig)
	if err != nil {
		return err
	}

	brain, err := llm.New(llm.Options{
		Provider:  cfg.Provider,
		APIKey:    cfg.APIKey,
		BaseURL:   cfg.BaseURL,
		Model:     cfg.Model,
		MaxTokens: cfg.MaxTokens,
		Effort:    cfg.Effort,
	})
	if err != nil {
		return err
	}

	notifier := buildNotifier(cfg)

	log.Info("podsmedic starting",
		"provider", cfg.Provider,
		"model", cfg.Model,
		"interval", cfg.Interval.String(),
		"cooldown", cfg.Cooldown.String(),
		"namespaces", namespaceLabel(cfg.Namespaces),
		"sinks", notifier.Name(),
		"metrics", cfg.MetricsAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sre := agent.New(cfg, kube, brain, notifier, log)

	// The live view, when enabled. It gets its own port and its own switch: see
	// the internal/ui package comment on why it does not join /metrics.
	if cfg.UIAddr != "" {
		stream := live.NewStream(cfg.UIEventBuffer)
		sre.SetLive(stream)

		// A watch, not the sweep. Everything podsmedic *does* stays on its
		// interval; only what it *shows* is live.
		go func() {
			err := kube.WatchPods(ctx, cfg.Namespaces, func(old, cur *corev1.Pod) {
				for _, e := range live.Transitions(old, cur, time.Now()) {
					stream.Publish(e)
				}
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Error("pod watch stopped; the live view will only update each sweep", "err", err)
			}
		}()

		gate := ui.NewGate(cfg.UIToken, cfg.UISessionTTL)
		go func() {
			if err := ui.New(stream, sre, gate, log).Serve(ctx, cfg.UIAddr); err != nil {
				log.Error("live view stopped", "err", err)
			}
		}()
	}

	// Inbound Telegram, when enabled: operators can ask questions and get
	// answers from the same cluster state the sweep works from. Read-only — it
	// never heals — and a misconfiguration is logged rather than fatal, so a bad
	// chat ID cannot stop the agent watching the cluster.
	switch {
	case cfg.TelegramListen:
		if bot, err := buildChatBot(cfg, sre, log); err != nil {
			log.Error("telegram chat listener not started", "err", err)
		} else {
			go func() {
				if err := bot.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					log.Error("telegram chat listener stopped", "err", err)
				}
			}()
		}
	case cfg.TelegramBotToken != "":
		// A configured bot that stays silent to questions is confusing enough to
		// be worth one line at startup, rather than leaving the operator to
		// wonder why their message went nowhere.
		log.Info("telegram chat listener is off; set PODSMEDIC_TELEGRAM_LISTEN=true to answer questions")
	}

	// Validate sinks up front so a bad webhook or token surfaces now, not on the
	// first real alert. Non-fatal: a transient network blip should not stop the
	// agent, and the failure is metered.
	checkSinks(ctx, log, notifier)

	// Observability endpoints run alongside the poll loop.
	metrics.Up.Set(1)
	go func() {
		if err := metrics.Serve(ctx, cfg.MetricsAddr, nil); err != nil {
			log.Error("metrics server stopped", "err", err)
		}
	}()

	return sre.Run(ctx)
}

// buildChatBot wires the inbound Telegram listener. The allowed-chat list is
// parsed here rather than in config so a malformed ID names itself in the log.
func buildChatBot(cfg *config.Config, answerer chat.Answerer, log *slog.Logger) (*chat.Bot, error) {
	if cfg.TelegramBotToken == "" {
		return nil, errors.New("PODSMEDIC_TELEGRAM_LISTEN is set but TELEGRAM_BOT_TOKEN is empty")
	}
	var ids []int64
	for _, raw := range cfg.TelegramAllowedChats {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("chat id %q is not a number", raw)
		}
		ids = append(ids, id)
	}
	return chat.New(chat.Options{
		Token:        cfg.TelegramBotToken,
		Allowed:      chat.NewAllowlist(ids),
		Answerer:     answerer,
		MaxPerMinute: cfg.ChatMaxPerMinute,
		Log:          log,
	})
}

// checkSinks runs each notifier's startup validation, logging and metering any
// failure without aborting the run.
func checkSinks(ctx context.Context, log *slog.Logger, notifier notify.Notifier) {
	checker, ok := notifier.(interface {
		CheckEach(context.Context) []notify.SinkCheck
	})
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, c := range checker.CheckEach(ctx) {
		if c.Err != nil {
			metrics.SinkCheckFailures.Inc(c.Name)
			log.Warn("notification sink check failed", "sink", c.Name, "err", c.Err)
			continue
		}
		log.Info("notification sink ok", "sink", c.Name)
	}
}

func buildNotifier(cfg *config.Config) notify.Notifier {
	var sinks notify.Multi
	if cfg.SlackWebhookURL != "" {
		sinks = append(sinks, notify.NewSlack(cfg.SlackWebhookURL))
	}
	if cfg.TelegramBotToken != "" {
		sinks = append(sinks, notify.NewTelegram(cfg.TelegramBotToken, cfg.TelegramChatID))
	}
	if cfg.DryRun || len(sinks) == 0 {
		sinks = append(sinks, notify.NewStdout(os.Stdout))
	}
	return sinks
}

func namespaceLabel(ns []string) string {
	if len(ns) == 0 {
		return "<all>"
	}
	out := ns[0]
	for _, n := range ns[1:] {
		out += "," + n
	}
	return out
}

func logLevel() slog.Level {
	switch os.Getenv("PODSMEDIC_LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
