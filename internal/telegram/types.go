package telegram

// Bot API payloads, reduced to the fields Dockontroler reads or sends.

// Update is one entry from getUpdates. Exactly one of the payload fields is set.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// Message is a chat message.
type Message struct {
	MessageID int64  `json:"message_id"`
	Chat      Chat   `json:"chat"`
	From      *User  `json:"from"`
	Text      string `json:"text"`
}

// Chat is where a message was sent. Group and channel ids are negative.
type Chat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"` // private | group | supergroup | channel
	Title string `json:"title"`
}

// User is a Telegram account.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// Label renders a user for log lines.
func (u *User) Label() string {
	if u == nil {
		return "unknown"
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	if u.FirstName != "" {
		return u.FirstName
	}
	return "unknown"
}

// CallbackQuery is a tap on an inline keyboard button.
type CallbackQuery struct {
	ID string `json:"id"`
	// From is the account that tapped. Message is the message the keyboard
	// belongs to, and may be absent for very old messages.
	From    *User    `json:"from"`
	Message *Message `json:"message"`
	// Data is the callback_data of the tapped button, capped by Telegram at
	// 64 bytes — which is why callbacks carry short container ids.
	Data string `json:"data"`
}

// InlineKeyboardMarkup is the button grid attached to a message.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// InlineKeyboardButton is one button.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}
