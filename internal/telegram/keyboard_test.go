package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/mkrage/dockontroler/internal/docker"
	"github.com/mkrage/dockontroler/internal/manager"
)

func sampleContainer() manager.Container {
	return manager.Container{
		ID:             strings.Repeat("a", 64),
		ShortID:        "aaaaaaaaaaaa",
		Name:           "blog-web-1",
		Image:          "ghcr.io/me/blog:latest",
		State:          docker.StateRunning,
		Status:         "Up 3 hours",
		Policy:         docker.PolicyUnlessStopped,
		Running:        true,
		ComposeProject: "blog",
		ComposeService: "web",
		CanRecreate:    true,
	}
}

// TestCallbackDataFitsTelegramLimit is the constraint that shapes the whole
// callback format: Telegram rejects callback_data over 64 bytes, so containers are
// addressed by short id rather than by their full 64-character one.
func TestCallbackDataFitsTelegramLimit(t *testing.T) {
	shortID := "a1b2c3d4e5f6"

	payloads := []string{
		verbOverview,
		encodeCallback(verbShow, shortID),
		encodeCallback(verbStart, shortID),
		encodeCallback(verbStop, shortID),
		encodeCallback(verbRestart, shortID),
		encodeCallback(verbRecreateAsk, shortID),
		encodeCallback(verbRecreateDo, shortID),
		encodePolicyCallback(docker.PolicyNo, shortID),
		encodePolicyCallback(docker.PolicyAlways, shortID),
		encodePolicyCallback(docker.PolicyUnlessStopped, shortID),
	}
	for _, payload := range payloads {
		if len(payload) > callbackSizeLimit {
			t.Errorf("callback_data %q is %d bytes, over the %d-byte limit",
				payload, len(payload), callbackSizeLimit)
		}
	}
}

func TestCallbackRoundTrip(t *testing.T) {
	shortID := "a1b2c3d4e5f6"

	for _, verb := range []string{verbShow, verbStart, verbStop, verbRestart, verbRecreateAsk, verbRecreateDo} {
		decoded, ok := decodeCallback(encodeCallback(verb, shortID))
		if !ok {
			t.Fatalf("decodeCallback rejected its own encoding of %q", verb)
		}
		if decoded.Verb != verb || decoded.ID != shortID {
			t.Errorf("decoded %+v, want verb %q and id %q", decoded, verb, shortID)
		}
	}

	decoded, ok := decodeCallback(encodePolicyCallback(docker.PolicyUnlessStopped, shortID))
	if !ok {
		t.Fatal("decodeCallback rejected a policy payload")
	}
	if decoded.Verb != verbPolicy || decoded.Policy != docker.PolicyUnlessStopped || decoded.ID != shortID {
		t.Errorf("decoded %+v, want the policy payload", decoded)
	}
}

// TestCallbackRejectsUnknownData: a stale button from an older version of the bot
// must do nothing rather than be guessed at.
func TestCallbackRejectsUnknownData(t *testing.T) {
	for _, data := range []string{
		"",
		"nonsense",
		"c",                  // container verb with no id
		"c:",                 // empty id
		"p:on-failure:abc12", // a real Docker policy, but outside this tool
		"p:always",           // missing id
		"rm:abc123def456",    // a verb that does not exist
	} {
		if decoded, ok := decodeCallback(data); ok {
			t.Errorf("decodeCallback(%q) accepted it as %+v, want rejection", data, decoded)
		}
	}
}

func TestRenderContainerKeyboard(t *testing.T) {
	text, markup := renderContainer(sampleContainer())

	if !strings.Contains(text, "blog-web-1") {
		t.Errorf("text does not name the container:\n%s", text)
	}
	if !strings.Contains(text, docker.PolicyUnlessStopped) {
		t.Errorf("text does not show the current policy:\n%s", text)
	}

	labels := buttonLabels(markup)
	for _, want := range []string{"⏹ Stop", "↻ Restart", "⬆️ Recreate", "‹ Back"} {
		if !containsLabel(labels, want) {
			t.Errorf("keyboard is missing %q, has %q", want, labels)
		}
	}
	// The active policy is marked so the current state is readable at a glance.
	if !containsLabel(labels, "✓ Unless stopped") {
		t.Errorf("the active policy is not marked, labels were %q", labels)
	}
}

func TestRenderContainerStoppedShowsStart(t *testing.T) {
	container := sampleContainer()
	container.Running = false
	container.State = docker.StateExited

	_, markup := renderContainer(container)

	labels := buttonLabels(markup)
	if !containsLabel(labels, "▶️ Start") {
		t.Errorf("stopped container has no start button, labels were %q", labels)
	}
	if containsLabel(labels, "⏹ Stop") {
		t.Errorf("stopped container offers stop, labels were %q", labels)
	}
}

// TestRenderContainerSelfHasNoDestructiveButtons mirrors the web UI: acting on
// Dockontroler's own container would kill the process mid-request.
func TestRenderContainerSelfHasNoDestructiveButtons(t *testing.T) {
	container := sampleContainer()
	container.IsSelf = true
	container.CanRecreate = false

	text, markup := renderContainer(container)

	labels := buttonLabels(markup)
	for _, forbidden := range []string{"⏹ Stop", "↻ Restart", "⬆️ Recreate"} {
		if containsLabel(labels, forbidden) {
			t.Errorf("own container offers %q, labels were %q", forbidden, labels)
		}
	}
	// The policy control stays: it changes no running state.
	if !containsLabel(labels, "✓ Unless stopped") {
		t.Errorf("own container lost its policy buttons, labels were %q", labels)
	}
	if !strings.Contains(text, "Dockontroler itself") {
		t.Errorf("text does not explain why the buttons are missing:\n%s", text)
	}
}

func TestRenderContainerWithoutRecreateSupport(t *testing.T) {
	container := sampleContainer()
	container.CanRecreate = false
	container.Note = "pinned to an image id, not a tag"

	text, markup := renderContainer(container)

	if containsLabel(buttonLabels(markup), "⬆️ Recreate") {
		t.Error("recreate offered for a container that cannot be recreated")
	}
	if !strings.Contains(text, "pinned to an image id") {
		t.Errorf("the reason is not shown to the user:\n%s", text)
	}
}

// TestRenderEscapesHTML matters because the bot uses Telegram's HTML parse mode,
// and container names and Docker error messages both reach the user through it.
func TestRenderEscapesHTML(t *testing.T) {
	container := sampleContainer()
	container.Name = "evil<b>name</b>"
	container.ComposeService = ""

	text, _ := renderContainer(container)

	if strings.Contains(text, "<b>name</b>") {
		t.Errorf("container name was not escaped:\n%s", text)
	}
	if !strings.Contains(text, "&lt;b&gt;name&lt;/b&gt;") {
		t.Errorf("expected the escaped form in:\n%s", text)
	}
}

func TestRenderOverview(t *testing.T) {
	overview := manager.Overview{
		Total:       2,
		Running:     1,
		GeneratedAt: time.Date(2026, 7, 28, 14, 3, 22, 0, time.UTC),
		Groups: []manager.Group{
			{Project: "blog", Containers: []manager.Container{sampleContainer()}},
			{Project: "", Containers: []manager.Container{{
				ShortID: "bbbbbbbbbbbb",
				Name:    "pihole",
				State:   docker.StateExited,
				Status:  "Exited (0) 1 day ago",
			}}},
		},
	}

	text, markup := renderOverview(overview)

	if !strings.Contains(text, "1 of 2 running") {
		t.Errorf("summary missing from:\n%s", text)
	}
	if !strings.Contains(text, "Standalone") {
		t.Errorf("containers without a project need their own heading:\n%s", text)
	}
	if !strings.Contains(text, "14:03:22") {
		t.Errorf("timestamp missing from:\n%s", text)
	}

	labels := buttonLabels(markup)
	if len(labels) != 2 {
		t.Fatalf("got %d buttons (%q), want one per container", len(labels), labels)
	}
	// Buttons show the Compose service name where there is one, since that is
	// what people actually call the thing.
	if !containsLabel(labels, "🟢 web") || !containsLabel(labels, "⚪ pihole") {
		t.Errorf("button labels = %q", labels)
	}
}

func TestRenderOverviewEmptyHost(t *testing.T) {
	text, markup := renderOverview(manager.Overview{})

	if !strings.Contains(text, "No containers") {
		t.Errorf("empty host message missing from:\n%s", text)
	}
	if markup != nil {
		t.Error("an empty host should not get a keyboard")
	}
}

func TestCommandParsing(t *testing.T) {
	cases := map[string]string{
		"/list":                    "/list",
		"/list@dockontroler_bot":   "/list",
		"  /LIST  ":                "/list",
		"/start extra args":        "/start",
		"hello there":              "",
		"":                         "",
		"not/a/command":            "",
	}
	for input, want := range cases {
		if got := command(input); got != want {
			t.Errorf("command(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestTruncateCountsRunes(t *testing.T) {
	// Container names are not necessarily ASCII, and cutting mid-rune would
	// produce invalid UTF-8 that Telegram rejects.
	got := truncate(strings.Repeat("ü", 10), 5)
	if runes := len([]rune(got)); runes != 5 {
		t.Errorf("truncate produced %d runes (%q), want 5", runes, got)
	}
	if unchanged := truncate("short", 20); unchanged != "short" {
		t.Errorf("truncate shortened a string that fits: %q", unchanged)
	}
}

func buttonLabels(markup *InlineKeyboardMarkup) []string {
	if markup == nil {
		return nil
	}
	var labels []string
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			labels = append(labels, button.Text)
		}
	}
	return labels
}

func containsLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}
