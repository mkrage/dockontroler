package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mkrage/dockontroler/internal/manager"
)

// Concurrency and retry limits for the poll loop.
const (
	// maxConcurrentUpdates lets a slow action — a recreate takes minutes — run
	// without freezing the bot for everyone else. The manager serialises work per
	// container, so overlapping updates are safe.
	maxConcurrentUpdates = 4
	minBackoff           = 1 * time.Second
	maxBackoff           = 30 * time.Second
)

// Bot controls containers from a Telegram chat.
type Bot struct {
	api     *api
	manager *manager.Manager
	log     *slog.Logger
	allowed map[int64]bool
	// slots bounds how many updates are handled at once; see
	// maxConcurrentUpdates.
	slots    chan struct{}
	inFlight sync.WaitGroup
}

// New builds a Bot. allowedChatIDs must not be empty — config.Load refuses to
// start otherwise, because a bot without an allow-list hands every container on
// the host to whoever finds it.
func New(token string, allowedChatIDs []int64, containers *manager.Manager, log *slog.Logger) *Bot {
	allowed := make(map[int64]bool, len(allowedChatIDs))
	for _, id := range allowedChatIDs {
		allowed[id] = true
	}
	return &Bot{
		api:     newAPI(token),
		manager: containers,
		log:     log,
		allowed: allowed,
		slots:   make(chan struct{}, maxConcurrentUpdates),
	}
}

// Run polls for updates until ctx is cancelled, then waits for in-flight actions
// to finish.
//
// A bad token is reported as an error so startup can fail loudly. Everything
// after that is retried with backoff: the bot losing its connection should not
// take the web UI down with it.
func (b *Bot) Run(ctx context.Context) error {
	me, err := b.api.getMe(ctx)
	if err != nil {
		return fmt.Errorf("telegram token rejected: %w", err)
	}
	b.log.Info("telegram bot connected",
		"username", me.Username, "allowed_chats", len(b.allowed))

	defer b.inFlight.Wait()

	var (
		offset  int64
		backoff = minBackoff
	)
	for {
		if ctx.Err() != nil {
			b.log.Info("telegram bot stopping")
			return nil
		}

		updates, err := b.api.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				b.log.Info("telegram bot stopping")
				return nil
			}
			b.logPollError(err, backoff)
			if !sleepContext(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = minBackoff

		for _, update := range updates {
			// Advance the offset before handling, so a handler that panics or
			// hangs cannot make the same update repeat forever.
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			b.dispatch(ctx, update)
		}
	}
}

// logPollError explains the failures that have a specific cause worth naming.
func (b *Bot) logPollError(err error, retryIn time.Duration) {
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.Code == 409 {
		// Telegram refuses long polling while a webhook is registered for the
		// token. docKontroler does not delete it: the webhook may belong to
		// another service the user is running on purpose.
		b.log.Error("telegram rejects polling because a webhook is registered for this token; "+
			"delete the webhook or use a separate bot for dockontroler",
			"error", err, "retry_in", retryIn)
		return
	}
	b.log.Warn("telegram poll failed, retrying", "error", err, "retry_in", retryIn)
}

// dispatch handles one update, concurrently but bounded.
//
// The wait for a slot happens here rather than inside the goroutine, so that a
// saturated bot stops polling instead of piling up handlers: without that, anyone
// who knows the bot's username could spawn goroutines by spamming it, since the
// allow-list applies to what is answered, not to what arrives.
func (b *Bot) dispatch(ctx context.Context, update Update) {
	select {
	case b.slots <- struct{}{}:
	case <-ctx.Done():
		return
	}

	b.inFlight.Add(1)
	go func() {
		defer b.inFlight.Done()
		defer func() { <-b.slots }()
		defer func() {
			if recovered := recover(); recovered != nil {
				b.log.Error("telegram handler panicked", "panic", recovered)
			}
		}()

		switch {
		case update.Message != nil:
			b.handleMessage(ctx, update.Message)
		case update.CallbackQuery != nil:
			b.handleCallback(ctx, update.CallbackQuery)
		}
	}()
}

// ---------- messages ----------

func (b *Bot) handleMessage(ctx context.Context, message *Message) {
	chatID := message.Chat.ID
	if !b.allowed[chatID] {
		b.rejectChat(ctx, chatID, message.From)
		return
	}

	switch command(message.Text) {
	case "/start", "/help":
		b.sendOrEdit(ctx, chatID, 0, helpText(), nil)
	case "/list", "/ps", "/status", "/containers":
		b.showOverview(ctx, chatID, 0)
	case "":
		// Plain chatter, not a command. Staying quiet keeps the bot usable in a
		// group chat that is also used for other things.
	default:
		b.sendOrEdit(ctx, chatID, 0,
			"Unknown command. Send /list to see your containers, or /help.", nil)
	}
}

// rejectChat answers an unauthorised chat.
//
// The reply includes the chat id on purpose: it is what the operator needs for
// TELEGRAM_ALLOWED_CHAT_IDS, and telling a stranger their own chat id reveals
// nothing they could not read from any other bot.
func (b *Bot) rejectChat(ctx context.Context, chatID int64, from *User) {
	b.log.Warn("rejected telegram message from a chat that is not on the allow-list",
		"chat_id", chatID, "user", from.Label())

	text := fmt.Sprintf(
		"Not authorised.\n\nThis chat's id is <code>%d</code>. "+
			"Add it to <code>TELEGRAM_ALLOWED_CHAT_IDS</code> and restart dockontroler "+
			"if this chat should be allowed to control containers.", chatID)
	if err := b.api.sendMessage(ctx, chatID, text, nil); err != nil {
		b.log.Debug("could not answer unauthorised chat", "chat_id", chatID, "error", err)
	}
}

// command extracts the command from a message, dropping the "@botname" suffix
// Telegram appends in group chats.
func command(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	word := strings.Fields(text)[0]
	if at := strings.IndexByte(word, '@'); at > 0 {
		word = word[:at]
	}
	return strings.ToLower(word)
}

func helpText() string {
	return "<b>docKontroler</b>\n\n" +
		"/list — show all containers and control them\n" +
		"/help — this message\n\n" +
		"Everything else works through the buttons: start, stop and restart a " +
		"container, choose when it starts again, or recreate it to pick up a " +
		"rebuilt image."
}

// ---------- callbacks ----------

func (b *Bot) handleCallback(ctx context.Context, query *CallbackQuery) {
	if query.Message == nil {
		// Telegram omits the message for very old keyboards, leaving nothing to
		// edit.
		b.answer(ctx, query.ID, "This message is too old. Send /list for a fresh view.", true)
		return
	}

	chatID := query.Message.Chat.ID
	if !b.allowed[chatID] {
		b.log.Warn("rejected telegram button press from a chat that is not on the allow-list",
			"chat_id", chatID, "user", query.From.Label())
		b.answer(ctx, query.ID, "Not authorised. This chat's id is "+strconv.FormatInt(chatID, 10), true)
		return
	}

	action, ok := decodeCallback(query.Data)
	if !ok {
		b.log.Debug("ignoring unrecognised callback data", "data", query.Data)
		b.answer(ctx, query.ID, "That button is no longer valid. Send /list for a fresh view.", true)
		return
	}

	messageID := query.Message.MessageID
	switch action.Verb {
	case verbOverview:
		b.answer(ctx, query.ID, "", false)
		b.showOverview(ctx, chatID, messageID)
	case verbShow:
		b.answer(ctx, query.ID, "", false)
		b.showContainer(ctx, chatID, messageID, action.ID, "")
	case verbRecreateAsk:
		b.answer(ctx, query.ID, "", false)
		b.showRecreateConfirm(ctx, chatID, messageID, action.ID)
	default:
		b.runAction(ctx, query, action)
	}
}

// runAction performs a state-changing button press and shows the result.
func (b *Bot) runAction(ctx context.Context, query *CallbackQuery, action callback) {
	chatID := query.Message.Chat.ID
	messageID := query.Message.MessageID
	user := query.From.Label()

	// The container to display afterwards. A recreate replaces the container, so
	// the id in the button no longer exists once it succeeds.
	nextRef := action.ID

	var (
		pending string
		done    string
		perform func() error
	)
	switch action.Verb {
	case verbStart:
		pending, done = "Starting…", "Started."
		perform = func() error { return b.manager.Start(ctx, action.ID) }
	case verbStop:
		pending, done = "Stopping…", "Stopped."
		perform = func() error { return b.manager.Stop(ctx, action.ID) }
	case verbRestart:
		pending, done = "Restarting…", "Restarted."
		perform = func() error { return b.manager.Restart(ctx, action.ID) }
	case verbRecreateDo:
		pending, done = "Recreating, this can take a while…", "Recreated."
		perform = func() error {
			result, err := b.manager.Recreate(ctx, action.ID)
			if err != nil {
				return err
			}
			nextRef = result.NewID
			if len(result.Notes) > 0 {
				done = "Recreated. " + strings.Join(result.Notes, "; ") + "."
			}
			return nil
		}
	case verbPolicy:
		pending = "Setting restart policy…"
		done = "Restart policy set to " + action.Policy + "."
		perform = func() error { return b.manager.SetPolicy(ctx, action.ID, action.Policy) }
	default:
		b.answer(ctx, query.ID, "Unsupported action.", true)
		return
	}

	// Answer before doing the work: Telegram expects a reply within a few
	// seconds, and a recreate can run for minutes.
	b.answer(ctx, query.ID, pending, false)

	b.log.Info("telegram action requested",
		"action", action.Verb, "container", action.ID, "chat_id", chatID, "user", user)

	if err := perform(); err != nil {
		b.log.Info("telegram action failed",
			"action", action.Verb, "container", action.ID, "error", err)
		b.showContainer(ctx, chatID, messageID, action.ID, "⚠️ "+esc(manager.UserMessage(err)))
		return
	}
	b.showContainer(ctx, chatID, messageID, nextRef, "✅ "+esc(done))
}

// ---------- views ----------

func (b *Bot) showOverview(ctx context.Context, chatID, messageID int64) {
	overview, err := b.manager.List(ctx)
	if err != nil {
		b.log.Error("could not list containers for telegram", "error", err)
		b.sendOrEdit(ctx, chatID, messageID,
			"⚠️ Cannot reach the Docker daemon.\n<code>"+esc(manager.UserMessage(err))+"</code>", nil)
		return
	}
	text, markup := renderOverview(overview)
	b.sendOrEdit(ctx, chatID, messageID, text, markup)
}

// showContainer renders one container, optionally with a banner line above it
// reporting the outcome of the action that led here.
func (b *Bot) showContainer(ctx context.Context, chatID, messageID int64, ref, banner string) {
	container, err := b.manager.Get(ctx, ref)
	if err != nil {
		if errors.Is(err, manager.ErrNotFound) {
			// Expected after a container is removed elsewhere, so fall back to
			// the list rather than showing a dead end.
			b.showOverview(ctx, chatID, messageID)
			return
		}
		b.log.Error("could not load container for telegram", "container", ref, "error", err)
		b.sendOrEdit(ctx, chatID, messageID, "⚠️ "+esc(manager.UserMessage(err)), nil)
		return
	}

	text, markup := renderContainer(container)
	if banner != "" {
		text = banner + "\n\n" + text
	}
	b.sendOrEdit(ctx, chatID, messageID, text, markup)
}

func (b *Bot) showRecreateConfirm(ctx context.Context, chatID, messageID int64, ref string) {
	container, err := b.manager.Get(ctx, ref)
	if err != nil {
		b.showOverview(ctx, chatID, messageID)
		return
	}
	if !container.CanRecreate {
		b.showContainer(ctx, chatID, messageID, ref, "⚠️ This container cannot be recreated.")
		return
	}
	text, markup := renderRecreateConfirm(container)
	b.sendOrEdit(ctx, chatID, messageID, text, markup)
}

// sendOrEdit posts a new message when messageID is 0, and rewrites the existing
// one otherwise. Editing in place is what keeps a control session to a single
// message instead of a growing wall of near-identical ones.
func (b *Bot) sendOrEdit(ctx context.Context, chatID, messageID int64, text string, markup *InlineKeyboardMarkup) {
	var err error
	if messageID == 0 {
		err = b.api.sendMessage(ctx, chatID, text, markup)
	} else {
		err = b.api.editMessage(ctx, chatID, messageID, text, markup)
	}
	if err != nil && ctx.Err() == nil {
		b.log.Error("could not deliver telegram message", "chat_id", chatID, "error", err)
	}
}

func (b *Bot) answer(ctx context.Context, callbackID, text string, alert bool) {
	if err := b.api.answerCallback(ctx, callbackID, text, alert); err != nil && ctx.Err() == nil {
		b.log.Debug("could not answer callback query", "error", err)
	}
}

// sleepContext waits for d, returning false if ctx was cancelled first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
