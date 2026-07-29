package telegram

import (
	"strings"
	"testing"
)

func TestTruncateLeavesShortTextAlone(t *testing.T) {
	text := "<b>Dockontroler</b>\n🟢 blog · Up 3 hours"

	if got := truncate(text, maxMessageUnits); got != text {
		t.Errorf("truncate rewrote text that already fits:\n%s", got)
	}
}

// TestTruncateCountsUTF16Units: Telegram measures its 4096 limit in UTF-16 code
// units, so every state emoji counts twice. Counting runes instead would let a
// message through that the API then rejects as too long.
func TestTruncateCountsUTF16Units(t *testing.T) {
	// 10 emoji are 10 runes but 20 UTF-16 units.
	text := strings.Repeat("🟢", 10)

	if got := visibleUnits(text); got != 20 {
		t.Errorf("visibleUnits = %d, want 20", got)
	}
	if got := truncate(text, 20); got != text {
		t.Errorf("truncate cut text that fits exactly: %q", got)
	}
	got := truncate(text, 19)
	if got == text {
		t.Fatal("truncate left a text that exceeds the limit untouched")
	}
	if visibleUnits(got) > 19 {
		t.Errorf("truncate returned %d units, want at most 19: %q", visibleUnits(got), got)
	}
}

// TestTruncateKeepsMarkupValid is the point of the whole exercise: a cut landing
// inside a tag, inside an entity, or before a closing tag makes Telegram reject the
// entire message, so the user gets nothing at all in exactly the case where the
// truncation was supposed to save the reply.
func TestTruncateKeepsMarkupValid(t *testing.T) {
	cases := map[string]string{
		"cut inside an element":  "<b>" + strings.Repeat("a", 50) + "</b>",
		"cut between two":        "<b>aaaa</b>\n<i>" + strings.Repeat("b", 50) + "</i>",
		"cut around an entity":   strings.Repeat("a", 18) + "&amp;&lt;&gt;" + strings.Repeat("b", 30),
		"cut inside nested tags": "<b><code>" + strings.Repeat("a", 50) + "</code></b>",
	}

	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			got := truncate(text, 20)

			if visibleUnits(got) > 20 {
				t.Errorf("result is %d units, want at most 20: %q", visibleUnits(got), got)
			}
			if !strings.Contains(got, "…") {
				t.Errorf("result does not mark the cut: %q", got)
			}
			if opened := openTags(got); len(opened) > 0 {
				t.Errorf("result leaves %v unclosed, which Telegram rejects: %q", opened, got)
			}
			if bad := strings.LastIndexByte(got, '<'); bad > strings.LastIndexByte(got, '>') {
				t.Errorf("result ends inside a tag: %q", got)
			}
			if amp := strings.LastIndexByte(got, '&'); amp >= 0 &&
				!strings.Contains(got[amp:], ";") {
				t.Errorf("result ends inside an entity: %q", got)
			}
		})
	}
}

// openTags lists the elements a fragment leaves open, innermost last.
func openTags(text string) []string {
	var open []string
	for i := 0; i < len(text); {
		tag, width := leadingTag(text[i:])
		if width == 0 {
			_, size := leadingChar(text[i:])
			i += size
			continue
		}
		i += width

		name, closing := tagName(tag)
		switch {
		case name == "":
		case closing:
			if last := len(open) - 1; last >= 0 && open[last] == name {
				open = open[:last]
			} else {
				open = append(open, "unexpected </"+name+">")
			}
		default:
			open = append(open, name)
		}
	}
	return open
}
