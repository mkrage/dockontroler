// Package config loads docKontroler's settings from environment variables.
//
// Invalid values abort startup instead of silently falling back to a default:
// a typo in the Telegram allow-list or the listen address should be obvious
// immediately, not three weeks later.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully validated runtime configuration.
type Config struct {
	// ListenAddr is the address the web UI binds to, e.g. ":8080".
	ListenAddr string
	// DockerSocket is the path to the Engine's unix socket.
	DockerSocket string
	// RefreshInterval is how often the overview reloads itself in the browser.
	RefreshInterval time.Duration
	// StopTimeout is how long a container may take to shut down before it is
	// killed. Also used for the stop step of a recreate.
	StopTimeout time.Duration

	// TelegramToken is empty when the bot is disabled.
	TelegramToken string
	// TelegramChatIDs is the allow-list of chats permitted to control Docker.
	// Never empty when TelegramToken is set.
	TelegramChatIDs []int64

	// SelfContainerID overrides docKontroler's own container detection. Empty
	// means autodetect.
	SelfContainerID string

	LogLevel slog.Level
}

// BotEnabled reports whether the Telegram bot should be started.
func (c Config) BotEnabled() bool { return c.TelegramToken != "" }

// Default values, exported so the README and tests cannot drift from the code.
const (
	DefaultListenAddr      = ":8080"
	DefaultDockerSocket    = "/var/run/docker.sock"
	DefaultRefreshInterval = 5 * time.Second
	DefaultStopTimeout     = 10 * time.Second
)

// Load reads and validates the environment.
//
// All problems are reported together rather than one per restart, since a fresh
// deployment usually has more than one thing wrong at once.
func Load() (Config, error) {
	var problems []error
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	cfg := Config{
		ListenAddr:      DefaultListenAddr,
		DockerSocket:    DefaultDockerSocket,
		RefreshInterval: DefaultRefreshInterval,
		StopTimeout:     DefaultStopTimeout,
		LogLevel:        slog.LevelInfo,
		SelfContainerID: strings.TrimSpace(os.Getenv("DOCKONTROLER_SELF_ID")),
	}

	if raw := strings.TrimSpace(os.Getenv("LISTEN_ADDR")); raw != "" {
		if _, _, err := net.SplitHostPort(raw); err != nil {
			fail("LISTEN_ADDR %q is not a host:port address (use e.g. \":8080\" or \"192.168.1.10:8080\")", raw)
		} else {
			cfg.ListenAddr = raw
		}
	}

	if raw := strings.TrimSpace(os.Getenv("DOCKER_SOCKET")); raw != "" {
		socket, err := normalizeSocket(raw)
		if err != nil {
			problems = append(problems, err)
		} else {
			cfg.DockerSocket = socket
		}
	}

	if d, ok := loadDuration("REFRESH_INTERVAL", fail); ok {
		if d < time.Second {
			fail("REFRESH_INTERVAL must be at least 1s, got %s", d)
		} else {
			cfg.RefreshInterval = d
		}
	}

	if d, ok := loadDuration("STOP_TIMEOUT", fail); ok {
		switch {
		case d < 0:
			fail("STOP_TIMEOUT must not be negative, got %s", d)
		case d > 10*time.Minute:
			fail("STOP_TIMEOUT must be at most 10m, got %s", d)
		default:
			cfg.StopTimeout = d
		}
	}

	if raw := strings.TrimSpace(os.Getenv("LOG_LEVEL")); raw != "" {
		level, err := parseLogLevel(raw)
		if err != nil {
			problems = append(problems, err)
		} else {
			cfg.LogLevel = level
		}
	}

	cfg.TelegramToken = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	chatIDs, err := parseChatIDs(os.Getenv("TELEGRAM_ALLOWED_CHAT_IDS"))
	if err != nil {
		problems = append(problems, err)
	}
	cfg.TelegramChatIDs = chatIDs

	// The important guard: a bot with no allow-list would let anyone who finds
	// it control every container on the host. Refuse to start instead.
	if cfg.TelegramToken != "" && len(cfg.TelegramChatIDs) == 0 {
		fail("TELEGRAM_BOT_TOKEN is set but TELEGRAM_ALLOWED_CHAT_IDS is empty — " +
			"without an allow-list anyone who finds the bot could control your containers. " +
			"Send the bot a message and it will reply with your chat id")
	}

	if len(problems) > 0 {
		return Config{}, errors.Join(problems...)
	}
	return cfg, nil
}

// normalizeSocket accepts both a bare path and a "unix://" URL, because people
// reasonably copy the value of DOCKER_HOST into this variable.
func normalizeSocket(raw string) (string, error) {
	switch {
	case strings.HasPrefix(raw, "unix://"):
		return strings.TrimPrefix(raw, "unix://"), nil
	case strings.Contains(raw, "://"):
		return "", fmt.Errorf("DOCKER_SOCKET %q is not supported — dockontroler talks to a local unix socket, "+
			"so pass a path such as %q", raw, DefaultDockerSocket)
	default:
		return raw, nil
	}
}

// loadDuration reads an optional duration variable. ok is false when the
// variable is unset or invalid, in which case the caller keeps its default.
func loadDuration(name string, fail func(string, ...any)) (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	// A bare number is a common mistake and the intent is unambiguous.
	if n, err := strconv.Atoi(raw); err == nil {
		return time.Duration(n) * time.Second, true
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		fail("%s %q is not a duration (use e.g. \"5s\", \"1m\" or a plain number of seconds)", name, raw)
		return 0, false
	}
	return d, true
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("LOG_LEVEL %q is not one of debug, info, warn, error", raw)
	}
}

// parseChatIDs reads a comma-separated list of Telegram chat ids. Group and
// channel ids are negative, so no sign check is applied.
func parseChatIDs(raw string) ([]int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var (
		ids      []int64
		seen     = map[int64]bool{}
		problems []error
	)
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		id, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			problems = append(problems, fmt.Errorf(
				"TELEGRAM_ALLOWED_CHAT_IDS contains %q, which is not a numeric chat id", field))
			continue
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return ids, nil
}
