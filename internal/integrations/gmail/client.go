package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://gmail.googleapis.com/gmail/v1/users/me"

var ErrHistoryExpired = errors.New("gmail history id is too old")

type Client struct {
	HTTP    *http.Client
	BaseURL string
}

type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gmail api: %d: %s", e.Status, e.Body)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u := base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

type Profile struct {
	EmailAddress string `json:"emailAddress"`
	HistoryID    string `json:"historyId"`
}

func (c *Client) Profile(ctx context.Context) (Profile, error) {
	var p Profile
	return p, c.do(ctx, "GET", "/profile", nil, nil, &p)
}

type messageRef struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

func (c *Client) Search(ctx context.Context, q string, max int) ([]string, error) {
	var out struct {
		Messages []messageRef `json:"messages"`
	}
	err := c.do(ctx, "GET", "/messages", url.Values{"q": {q}, "maxResults": {strconv.Itoa(max)}}, nil, &out)
	ids := make([]string, 0, len(out.Messages))
	for _, m := range out.Messages {
		ids = append(ids, m.ID)
	}
	return ids, err
}

type rawPart struct {
	MimeType string `json:"mimeType"`
	Headers  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	Body struct {
		Data string `json:"data"`
	} `json:"body"`
	Parts []rawPart `json:"parts"`
}

type rawMessage struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	LabelIDs     []string `json:"labelIds"`
	Snippet      string   `json:"snippet"`
	InternalDate string   `json:"internalDate"`
	Payload      rawPart  `json:"payload"`
}

type Message struct {
	ID        string    `json:"id"`
	ThreadID  string    `json:"thread_id"`
	MessageID string    `json:"message_id,omitempty"`
	From      string    `json:"from"`
	To        string    `json:"to,omitempty"`
	Subject   string    `json:"subject"`
	Date      time.Time `json:"date"`
	Snippet   string    `json:"snippet"`
	Labels    []string  `json:"labels,omitempty"`
	Body      string    `json:"body,omitempty"`
}

func (m Message) HasLabel(label string) bool {
	for _, l := range m.Labels {
		if l == label {
			return true
		}
	}
	return false
}

func (c *Client) Message(ctx context.Context, id string, withBody bool) (Message, error) {
	format := "metadata"
	if withBody {
		format = "full"
	}
	var raw rawMessage
	if err := c.do(ctx, "GET", "/messages/"+url.PathEscape(id), url.Values{"format": {format}}, nil, &raw); err != nil {
		return Message{}, err
	}
	return parse(raw, withBody), nil
}

func (c *Client) Thread(ctx context.Context, id string) ([]Message, error) {
	var raw struct {
		Messages []rawMessage `json:"messages"`
	}
	if err := c.do(ctx, "GET", "/threads/"+url.PathEscape(id), url.Values{"format": {"full"}}, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(raw.Messages))
	for _, m := range raw.Messages {
		out = append(out, parse(m, true))
	}
	return out, nil
}

func parse(raw rawMessage, withBody bool) Message {
	m := Message{ID: raw.ID, ThreadID: raw.ThreadID, Snippet: raw.Snippet, Labels: raw.LabelIDs}
	for _, h := range raw.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			m.From = h.Value
		case "to":
			m.To = h.Value
		case "subject":
			m.Subject = h.Value
		case "message-id":
			m.MessageID = h.Value
		}
	}
	if ms, err := strconv.ParseInt(raw.InternalDate, 10, 64); err == nil {
		m.Date = time.UnixMilli(ms).UTC()
	}
	if withBody {
		m.Body = textBody(raw.Payload)
	}
	return m
}

func textBody(p rawPart) string {
	if strings.HasPrefix(p.MimeType, "text/plain") && p.Body.Data != "" {
		return decode(p.Body.Data)
	}
	for _, part := range p.Parts {
		if body := textBody(part); body != "" {
			return body
		}
	}
	if strings.HasPrefix(p.MimeType, "text/html") && p.Body.Data != "" {
		return "[html only] " + decode(p.Body.Data)
	}
	return ""
}

func decode(data string) string {
	b, err := base64.URLEncoding.DecodeString(data)
	if err != nil {
		b, err = base64.RawURLEncoding.DecodeString(data)
		if err != nil {
			return ""
		}
	}
	return string(b)
}

func (c *Client) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	body := map[string]string{"raw": base64.URLEncoding.EncodeToString(raw)}
	if threadID != "" {
		body["threadId"] = threadID
	}
	var out messageRef
	err := c.do(ctx, "POST", "/messages/send", nil, body, &out)
	return out.ID, err
}

func (c *Client) CreateDraft(ctx context.Context, raw []byte, threadID string) (string, error) {
	msg := map[string]string{"raw": base64.URLEncoding.EncodeToString(raw)}
	if threadID != "" {
		msg["threadId"] = threadID
	}
	var out struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, "POST", "/drafts", nil, map[string]any{"message": msg}, &out)
	return out.ID, err
}

func (c *Client) FindSent(ctx context.Context, messageID string) (string, bool, error) {
	ids, err := c.Search(ctx, "in:sent rfc822msgid:"+messageID, 1)
	if err != nil || len(ids) == 0 {
		return "", false, err
	}
	return ids[0], true, nil
}

func (c *Client) FindDraft(ctx context.Context, messageID string) (string, bool, error) {
	var out struct {
		Drafts []struct {
			ID string `json:"id"`
		} `json:"drafts"`
	}
	if err := c.do(ctx, "GET", "/drafts", url.Values{"q": {"rfc822msgid:" + messageID}, "maxResults": {"1"}}, nil, &out); err != nil {
		return "", false, err
	}
	if len(out.Drafts) == 0 {
		return "", false, nil
	}
	return out.Drafts[0].ID, true, nil
}

func (c *Client) History(ctx context.Context, startID string) ([]string, string, error) {
	var added []string
	latest := startID
	page := ""
	for {
		q := url.Values{"startHistoryId": {startID}, "historyTypes": {"messageAdded"}, "labelId": {"INBOX"}}
		if page != "" {
			q.Set("pageToken", page)
		}
		var out struct {
			History []struct {
				MessagesAdded []struct {
					Message messageRef `json:"message"`
				} `json:"messagesAdded"`
			} `json:"history"`
			HistoryID     string `json:"historyId"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := c.do(ctx, "GET", "/history", q, nil, &out); err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				return nil, "", ErrHistoryExpired
			}
			return nil, "", err
		}
		for _, h := range out.History {
			for _, m := range h.MessagesAdded {
				added = append(added, m.Message.ID)
			}
		}
		if out.HistoryID != "" {
			latest = out.HistoryID
		}
		if out.NextPageToken == "" {
			return added, latest, nil
		}
		page = out.NextPageToken
	}
}
