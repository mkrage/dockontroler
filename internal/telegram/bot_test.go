package telegram

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestDispatchWaitsForAFreeSlot: the cap exists so a flood of messages cannot
// spawn handlers without limit. The allow-list decides what gets answered, not
// what arrives, so anyone who knows the bot's username can send updates.
func TestDispatchWaitsForAFreeSlot(t *testing.T) {
	bot := newTestBot()
	for i := 0; i < maxConcurrentUpdates; i++ {
		bot.slots <- struct{}{}
	}

	dispatched := make(chan struct{})
	go func() {
		// An update with no payload keeps the handler from touching the network.
		bot.dispatch(context.Background(), Update{UpdateID: 1})
		close(dispatched)
	}()

	select {
	case <-dispatched:
		t.Fatal("dispatch accepted an update while all slots were taken")
	case <-time.After(50 * time.Millisecond):
	}

	<-bot.slots // a handler finished

	select {
	case <-dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not proceed after a slot was freed")
	}
	bot.inFlight.Wait()
}

// TestDispatchGivesUpOnShutdown: waiting for a slot must not outlive the context,
// or a shutdown would hang behind a saturated bot.
func TestDispatchGivesUpOnShutdown(t *testing.T) {
	bot := newTestBot()
	for i := 0; i < maxConcurrentUpdates; i++ {
		bot.slots <- struct{}{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	returned := make(chan struct{})
	go func() {
		bot.dispatch(ctx, Update{UpdateID: 1})
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch kept waiting for a slot after the context was cancelled")
	}
}

func newTestBot() *Bot {
	return New("test-token", []int64{1}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}
