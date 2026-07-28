package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// clearEnv empties every variable Load reads, so a test never picks up something
// from the developer's own shell.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"LISTEN_ADDR", "DOCKER_SOCKET", "REFRESH_INTERVAL", "STOP_TIMEOUT",
		"LOG_LEVEL", "TELEGRAM_BOT_TOKEN", "TELEGRAM_ALLOWED_CHAT_IDS",
		"DOCKONTROLER_SELF_ID",
	} {
		t.Setenv(name, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.DockerSocket != DefaultDockerSocket {
		t.Errorf("DockerSocket = %q, want %q", cfg.DockerSocket, DefaultDockerSocket)
	}
	if cfg.RefreshInterval != DefaultRefreshInterval {
		t.Errorf("RefreshInterval = %s, want %s", cfg.RefreshInterval, DefaultRefreshInterval)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.BotEnabled() {
		t.Error("BotEnabled = true with no token set")
	}
}

// TestBotRequiresAnAllowList is the security guard: a bot with no allow-list
// hands every container on the host to whoever finds it.
func TestBotRequiresAnAllowList(t *testing.T) {
	clearEnv(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456:secret")

	_, err := Load()

	if err == nil {
		t.Fatal("Load succeeded with a token but no allowed chat ids")
	}
	if !strings.Contains(err.Error(), "TELEGRAM_ALLOWED_CHAT_IDS") {
		t.Errorf("error = %q, want it to name the missing variable", err)
	}
}

func TestChatIDParsing(t *testing.T) {
	clearEnv(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456:secret")
	// Group and channel ids are negative; whitespace and duplicates are the kind
	// of thing that survives copy-paste.
	t.Setenv("TELEGRAM_ALLOWED_CHAT_IDS", " 42, -1001234567890 ,42")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []int64{42, -1001234567890}
	if len(cfg.TelegramChatIDs) != len(want) {
		t.Fatalf("TelegramChatIDs = %v, want %v (duplicates removed)", cfg.TelegramChatIDs, want)
	}
	for i, id := range want {
		if cfg.TelegramChatIDs[i] != id {
			t.Errorf("TelegramChatIDs[%d] = %d, want %d", i, cfg.TelegramChatIDs[i], id)
		}
	}
	if !cfg.BotEnabled() {
		t.Error("BotEnabled = false with a token set")
	}
}

func TestChatIDRejectsNonNumeric(t *testing.T) {
	clearEnv(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456:secret")
	// A username is the obvious mistake here, and it must not silently become an
	// empty allow-list.
	t.Setenv("TELEGRAM_ALLOWED_CHAT_IDS", "@myusername")

	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a non-numeric chat id")
	}
}

func TestDockerSocketAcceptsUnixURL(t *testing.T) {
	clearEnv(t)
	// People copy the value of DOCKER_HOST into this variable, so the scheme is
	// tolerated rather than rejected.
	t.Setenv("DOCKER_SOCKET", "unix:///run/docker.sock")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DockerSocket != "/run/docker.sock" {
		t.Errorf("DockerSocket = %q, want /run/docker.sock", cfg.DockerSocket)
	}
}

func TestDockerSocketRejectsTCP(t *testing.T) {
	clearEnv(t)
	t.Setenv("DOCKER_SOCKET", "tcp://192.168.1.10:2375")

	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a tcp:// docker host, which is not supported")
	}
}

func TestDurationsAcceptBareSeconds(t *testing.T) {
	clearEnv(t)
	t.Setenv("REFRESH_INTERVAL", "10")
	t.Setenv("STOP_TIMEOUT", "45s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RefreshInterval != 10*time.Second {
		t.Errorf("RefreshInterval = %s, want 10s", cfg.RefreshInterval)
	}
	if cfg.StopTimeout != 45*time.Second {
		t.Errorf("StopTimeout = %s, want 45s", cfg.StopTimeout)
	}
}

func TestInvalidValuesAreRejectedNotDefaulted(t *testing.T) {
	cases := []struct {
		label, name, value string
	}{
		{"listen address", "LISTEN_ADDR", "8080"},
		{"refresh too small", "REFRESH_INTERVAL", "100ms"},
		{"refresh nonsense", "REFRESH_INTERVAL", "soon"},
		{"negative stop", "STOP_TIMEOUT", "-5s"},
		{"stop far too large", "STOP_TIMEOUT", "3h"},
		{"unknown log level", "LOG_LEVEL", "verbose"},
		{"chat id list without a token is fine, but not this", "TELEGRAM_ALLOWED_CHAT_IDS", "abc"},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(testCase.name, testCase.value)

			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted %s=%q instead of failing loudly",
					testCase.name, testCase.value)
			}
		})
	}
}

// TestAllProblemsReportedTogether: a fresh deployment usually has more than one
// thing wrong, and fixing them one restart at a time is miserable.
func TestAllProblemsReportedTogether(t *testing.T) {
	clearEnv(t)
	t.Setenv("LISTEN_ADDR", "nonsense")
	t.Setenv("LOG_LEVEL", "loud")

	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded")
	}
	message := err.Error()
	if !strings.Contains(message, "LISTEN_ADDR") || !strings.Contains(message, "LOG_LEVEL") {
		t.Errorf("error = %q, want both problems mentioned", message)
	}
}
