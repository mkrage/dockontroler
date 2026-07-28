package telegram

import (
	"fmt"
	"html"
	"strings"

	"github.com/mkrage/dockontroler/internal/docker"
	"github.com/mkrage/dockontroler/internal/manager"
)

// Callback verbs.
//
// Telegram caps callback_data at 64 bytes, so these are terse and containers are
// addressed by their 12-character short id. The longest payload is a policy
// change — "p:unless-stopped:a1b2c3d4e5f6", 29 bytes — leaving ample headroom.
// callback_test.go asserts that.
const (
	verbOverview     = "l"
	verbShow         = "c"
	verbStart        = "on"
	verbStop         = "off"
	verbRestart      = "rst"
	verbRecreateAsk  = "rc"
	verbRecreateDo   = "rc!"
	verbPolicy       = "p"
	callbackSizeLimit = 64
)

// maxOverviewButtons caps the button grid. Telegram tolerates large keyboards but
// they become unusable, and a host with this many containers is better served by
// the web UI. Anything beyond the cap is reported in the message text.
const maxOverviewButtons = 60

// callback is a decoded button payload.
type callback struct {
	Verb string
	// ID is the short container id, empty for verbs that take no container.
	ID string
	// Policy is set for verbPolicy.
	Policy string
}

func encodeCallback(verb, shortID string) string {
	if shortID == "" {
		return verb
	}
	return verb + ":" + shortID
}

func encodePolicyCallback(policy, shortID string) string {
	return verbPolicy + ":" + policy + ":" + shortID
}

// decodeCallback parses button data. ok is false for anything unrecognised, which
// is treated as a no-op — stale buttons from an older version of the bot should
// not cause an action.
func decodeCallback(data string) (callback, bool) {
	parts := strings.Split(data, ":")
	switch parts[0] {
	case verbOverview:
		return callback{Verb: verbOverview}, true

	case verbShow, verbStart, verbStop, verbRestart, verbRecreateAsk, verbRecreateDo:
		if len(parts) != 2 || parts[1] == "" {
			return callback{}, false
		}
		return callback{Verb: parts[0], ID: parts[1]}, true

	case verbPolicy:
		if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
			return callback{}, false
		}
		switch parts[1] {
		case docker.PolicyNo, docker.PolicyAlways, docker.PolicyUnlessStopped:
		default:
			return callback{}, false
		}
		return callback{Verb: verbPolicy, Policy: parts[1], ID: parts[2]}, true

	default:
		return callback{}, false
	}
}

// renderOverview builds the container list message and its keyboard.
func renderOverview(overview manager.Overview) (string, *InlineKeyboardMarkup) {
	var text strings.Builder
	fmt.Fprintf(&text, "<b>Dockontroler</b>\n%d of %d running · %s\n",
		overview.Running, overview.Total, overview.GeneratedAt.Format("15:04:05"))

	if overview.Warning != "" {
		fmt.Fprintf(&text, "\n⚠️ %s\n", esc(overview.Warning))
	}
	if overview.Total == 0 {
		text.WriteString("\nNo containers on this host yet.")
		return text.String(), nil
	}

	var buttons []InlineKeyboardButton
	truncated := 0

	for _, group := range overview.Groups {
		title := group.Project
		if title == "" {
			title = "Standalone"
		}
		fmt.Fprintf(&text, "\n<b>%s</b>\n", esc(title))

		for _, container := range group.Containers {
			fmt.Fprintf(&text, "%s %s · %s\n",
				container.StateIcon(), esc(displayName(container)), esc(container.Status))

			if len(buttons) < maxOverviewButtons {
				buttons = append(buttons, InlineKeyboardButton{
					Text:         container.StateIcon() + " " + displayName(container),
					CallbackData: encodeCallback(verbShow, container.ShortID),
				})
			} else {
				truncated++
			}
		}
	}

	if truncated > 0 {
		fmt.Fprintf(&text, "\n<i>%d more container(s) not shown as buttons — use the web UI.</i>\n", truncated)
	}
	text.WriteString("\nPick a container to control it.")

	return text.String(), &InlineKeyboardMarkup{InlineKeyboard: chunk(buttons, 2)}
}

// renderContainer builds the per-container control message.
func renderContainer(container manager.Container) (string, *InlineKeyboardMarkup) {
	var text strings.Builder
	fmt.Fprintf(&text, "%s <b>%s</b>\n", container.StateIcon(), esc(container.Name))
	if container.ComposeProject != "" {
		fmt.Fprintf(&text, "project: %s\n", esc(container.ComposeProject))
	}
	fmt.Fprintf(&text, "image: <code>%s</code>\n", esc(container.Image))
	fmt.Fprintf(&text, "status: %s\n", esc(container.Status))
	fmt.Fprintf(&text, "restart: <code>%s</code>\n", esc(container.Policy))
	if container.Note != "" {
		fmt.Fprintf(&text, "\n<i>%s</i>\n", esc(container.Note))
	}
	if container.IsSelf {
		text.WriteString("\n⚠️ This is Dockontroler itself, so stop, restart and recreate are disabled.\n")
	}

	var rows [][]InlineKeyboardButton

	// Run controls. Recreating or stopping Dockontroler through Dockontroler would
	// kill the process mid-request, so those buttons are simply absent.
	if !container.IsSelf {
		var runRow []InlineKeyboardButton
		if container.Running {
			runRow = append(runRow,
				InlineKeyboardButton{Text: "⏹ Stop", CallbackData: encodeCallback(verbStop, container.ShortID)},
				InlineKeyboardButton{Text: "↻ Restart", CallbackData: encodeCallback(verbRestart, container.ShortID)},
			)
		} else {
			runRow = append(runRow,
				InlineKeyboardButton{Text: "▶️ Start", CallbackData: encodeCallback(verbStart, container.ShortID)},
			)
		}
		rows = append(rows, runRow)

		if container.CanRecreate {
			rows = append(rows, []InlineKeyboardButton{{
				Text:         "⬆️ Recreate",
				CallbackData: encodeCallback(verbRecreateAsk, container.ShortID),
			}})
		}
	}

	rows = append(rows, policyRow(container))
	rows = append(rows, []InlineKeyboardButton{{
		Text:         "‹ Back",
		CallbackData: verbOverview,
	}})

	return text.String(), &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// renderRecreateConfirm asks before replacing a container, since recreate is the
// one action here that deletes something.
func renderRecreateConfirm(container manager.Container) (string, *InlineKeyboardMarkup) {
	text := fmt.Sprintf(
		"⬆️ <b>Recreate %s?</b>\n\n"+
			"It will be replaced by a new container built from <code>%s</code> as it stands right now.\n\n"+
			"Configuration, volumes and networks are carried over. If anything fails, the original is restored.",
		esc(container.Name), esc(container.Image))

	return text, &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{
		{Text: "✅ Yes, recreate", CallbackData: encodeCallback(verbRecreateDo, container.ShortID)},
		{Text: "✖ Cancel", CallbackData: encodeCallback(verbShow, container.ShortID)},
	}}}
}

// policyRow renders the three restart policies, marking the active one.
func policyRow(container manager.Container) []InlineKeyboardButton {
	options := []struct {
		policy string
		label  string
	}{
		{docker.PolicyNo, "Never"},
		{docker.PolicyUnlessStopped, "Unless stopped"},
		{docker.PolicyAlways, "Always"},
	}

	row := make([]InlineKeyboardButton, 0, len(options))
	for _, option := range options {
		label := option.label
		if container.PolicyIs(option.policy) {
			label = "✓ " + label
		}
		row = append(row, InlineKeyboardButton{
			Text:         label,
			CallbackData: encodePolicyCallback(option.policy, container.ShortID),
		})
	}
	return row
}

// displayName prefers the Compose service name, which is shorter and what people
// actually call the thing.
func displayName(container manager.Container) string {
	if container.ComposeService != "" {
		return container.ComposeService
	}
	return container.Name
}

// chunk splits buttons into rows of at most size.
func chunk(buttons []InlineKeyboardButton, size int) [][]InlineKeyboardButton {
	var rows [][]InlineKeyboardButton
	for start := 0; start < len(buttons); start += size {
		end := start + size
		if end > len(buttons) {
			end = len(buttons)
		}
		rows = append(rows, buttons[start:end])
	}
	return rows
}

// esc escapes text for Telegram's HTML parse mode. Container names, image
// references and Docker error messages all reach the user this way, and any of
// them may contain characters that would otherwise break the markup.
func esc(text string) string {
	return html.EscapeString(text)
}
