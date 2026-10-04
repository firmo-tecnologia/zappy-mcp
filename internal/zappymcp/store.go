package zappymcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

type Message struct {
	ID         string          `json:"id"`
	InstanceID string          `json:"instance_id"`
	Chat       string          `json:"chat"`
	Number     string          `json:"number,omitempty"`
	Contact    string          `json:"contact,omitempty"`
	Direction  string          `json:"direction"`
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	Timestamp  time.Time       `json:"timestamp"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}
type Store struct{ Dir string }
type QueryInput struct {
	Number     string `json:"number,omitempty" jsonschema:"Exact phone number with country code; use number or contact"`
	Contact    string `json:"contact,omitempty" jsonschema:"Case-insensitive contact display name; may match multiple people"`
	InstanceID string `json:"instance_id,omitempty" jsonschema:"Optional WhatsApp instance filter"`
	After      string `json:"after,omitempty" jsonschema:"Optional exclusive RFC3339 timestamp"`
	Direction  string `json:"direction,omitempty" jsonschema:"Optional inbound or outbound filter"`
	Limit      int    `json:"limit,omitempty" jsonschema:"Page size; default 50, maximum 200"`
	Offset     int    `json:"offset,omitempty" jsonschema:"Pagination offset; newest messages first"`
}
type QueryOutput struct {
	Messages    []Message       `json:"messages"`
	Total       int             `json:"total"`
	HasMore     bool            `json:"has_more"`
	NextOffset  int             `json:"next_offset"`
	HistoryPath string          `json:"history_path"`
	Listener    json.RawMessage `json:"listener,omitempty"`
}

func NewStore(root, accountID string) (*Store, error) {
	dir, err := accountPath(root, accountID)
	if err != nil {
		return nil, err
	}
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}
func (s *Store) HistoryPath() string { return filepath.Join(s.Dir, "messages.json") }

func (s *Store) read() ([]Message, error) {
	messages := []Message{}
	err := readJSON(s.HistoryPath(), &messages)
	if errors.Is(err, os.ErrNotExist) {
		return messages, nil
	}
	if messages == nil && err == nil {
		return nil, errors.New("history must be a JSON array (file preserved)")
	}
	return messages, err
}
func (s *Store) Save(ctx context.Context, message Message) error {
	if message.ID == "" || !validUUID(message.InstanceID) {
		return errors.New("message ID and valid instance ID are required")
	}
	lock, err := lockFile(ctx, filepath.Join(s.Dir, "messages.lock"))
	if err != nil {
		return err
	}
	defer lock.Unlock()
	messages, err := s.read()
	if err != nil {
		return err
	}
	for i, existing := range messages {
		if existing.ID == message.ID && existing.InstanceID == message.InstanceID {
			// Enrich outgoing events with a later WhatsApp echo; never lose the
			// contact name or timestamp when a less complete duplicate arrives.
			if message.Contact == "" {
				message.Contact = existing.Contact
			}
			if message.Number == "" {
				message.Number = existing.Number
			}
			if message.Text == "" {
				message.Text = existing.Text
			}
			if len(message.Payload) == 0 {
				message.Payload = existing.Payload
			}
			if !existing.Timestamp.IsZero() {
				message.Timestamp = existing.Timestamp
			}
			messages[i] = message
			return writeJSON(s.HistoryPath(), messages)
		}
	}
	messages = append(messages, message)
	return writeJSON(s.HistoryPath(), messages)
}

func (s *Store) Query(ctx context.Context, input QueryInput) (QueryOutput, error) {
	output := QueryOutput{Messages: []Message{}, HistoryPath: s.HistoryPath()}
	if strings.TrimSpace(input.Number) == "" && strings.TrimSpace(input.Contact) == "" {
		return output, fmt.Errorf("number or contact is required")
	}
	if input.Limit == 0 {
		input.Limit = 50
	}
	if input.Limit < 1 || input.Limit > 200 || input.Offset < 0 {
		return output, fmt.Errorf("limit must be 1..200; offset cannot be negative")
	}
	if input.InstanceID != "" && !validUUID(input.InstanceID) {
		return output, fmt.Errorf("invalid instance_id")
	}
	if input.Direction != "" && input.Direction != "inbound" && input.Direction != "outbound" {
		return output, fmt.Errorf("direction must be inbound or outbound")
	}
	var after time.Time
	var err error
	if input.After != "" {
		after, err = time.Parse(time.RFC3339, input.After)
		if err != nil {
			return output, fmt.Errorf("after must be RFC3339")
		}
	}
	number := phone(input.Number)
	if input.Number != "" && number == "" {
		return output, fmt.Errorf("number must contain a valid phone number")
	}
	lock, err := lockFile(ctx, filepath.Join(s.Dir, "messages.lock"))
	if err != nil {
		return output, err
	}
	defer lock.Unlock()
	messages, err := s.read()
	if err != nil {
		return output, err
	}
	contact := strings.ToLower(strings.TrimSpace(input.Contact))
	// A contact query includes outgoing messages to the same number even when
	// outbound events have no push_name. Names are not unique across accounts.
	contactNumbers := map[string]bool{}
	if contact != "" {
		for _, message := range messages {
			if message.Number != "" && strings.Contains(strings.ToLower(message.Contact), contact) {
				contactNumbers[message.InstanceID+":"+message.Number] = true
			}
		}
	}
	matches := []Message{}
	for _, message := range messages {
		if number != "" && message.Number != number {
			continue
		}
		if contact != "" && !strings.Contains(strings.ToLower(message.Contact), contact) && !contactNumbers[message.InstanceID+":"+message.Number] {
			continue
		}
		if input.InstanceID != "" && message.InstanceID != input.InstanceID {
			continue
		}
		if input.Direction != "" && message.Direction != input.Direction {
			continue
		}
		if !after.IsZero() && !message.Timestamp.After(after) {
			continue
		}
		matches = append(matches, message)
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Timestamp.After(matches[j].Timestamp) })
	output.Total = len(matches)
	start := min(input.Offset, len(matches))
	end := start + min(input.Limit, len(matches)-start)
	output.Messages = matches[start:end]
	output.HasMore = end < len(matches)
	output.NextOffset = end
	if state, err := os.ReadFile(filepath.Join(s.Dir, "listener.json")); err == nil && json.Valid(state) {
		output.Listener = state
	}
	return output, nil
}

// LIDs and group identifiers are not phone numbers. Prefer sender_pn or
// recipient_pn from WhatsApp when the chat uses the @lid addressing mode.
func phone(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexByte(raw, '@'); i >= 0 {
		server := raw[i+1:]
		if server != "s.whatsapp.net" && server != "c.us" {
			return ""
		}
		raw = strings.Split(raw[:i], ":")[0]
	}
	var result strings.Builder
	for _, c := range raw {
		if c >= '0' && c <= '9' {
			result.WriteRune(c)
		} else if c != '+' && c != '(' && c != ')' && c != '-' && !unicode.IsSpace(c) {
			return ""
		}
	}
	digits := result.String()
	if len(digits) < 7 || len(digits) > 15 {
		return ""
	}
	return digits
}

func decodeEvent(body []byte, expectedInstance, accountID string) (Message, error) {
	var envelope struct {
		Event     string `json:"event"`
		Timestamp int64  `json:"timestamp"`
		Payload   struct {
			InstanceID string `json:"instance_id"`
			TenantID   string `json:"tenant_id"`
			MessageID  string `json:"message_id"`
			To         string `json:"to"`
			Type       string `json:"type"`
			Content    string `json:"content"`
			Message    struct {
				ID          string    `json:"id"`
				From        string    `json:"from"`
				Chat        string    `json:"chat"`
				Contact     string    `json:"push_name"`
				SenderPN    string    `json:"sender_pn"`
				RecipientPN string    `json:"recipient_pn"`
				Timestamp   time.Time `json:"timestamp"`
				FromMe      bool      `json:"is_from_me"`
				Type        string    `json:"type"`
				Text        string    `json:"text"`
				Caption     string    `json:"caption"`
			} `json:"message"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Message{}, err
	}
	p := envelope.Payload
	if p.InstanceID != expectedInstance || p.TenantID != accountID {
		return Message{}, errors.New("listener account or instance mismatch")
	}
	message := Message{InstanceID: p.InstanceID, Timestamp: time.Unix(envelope.Timestamp, 0).UTC(), Payload: append(json.RawMessage(nil), body...)}
	switch envelope.Event {
	case "message.sent":
		message.ID, message.Chat, message.Number = p.MessageID, p.To, phone(p.To)
		if message.Number != "" {
			message.Chat = message.Number + "@s.whatsapp.net"
		}
		message.Direction, message.Type, message.Text = "outbound", p.Type, p.Content
	case "message.received":
		m := p.Message
		message.ID, message.Chat, message.Type, message.Text = m.ID, m.Chat, m.Type, m.Text
		if message.Text == "" {
			message.Text = m.Caption
		}
		if !m.Timestamp.IsZero() {
			message.Timestamp = m.Timestamp
		}
		if m.FromMe {
			message.Direction = "outbound"
			message.Number = phone(m.RecipientPN)
			if message.Number == "" {
				message.Number = phone(m.Chat)
			}
		} else {
			message.Direction, message.Contact = "inbound", m.Contact
			message.Number = phone(m.SenderPN)
			if message.Number == "" {
				message.Number = phone(m.From)
			}
		}
	default:
		return Message{}, fmt.Errorf("unsupported listener event")
	}
	return message, nil
}
