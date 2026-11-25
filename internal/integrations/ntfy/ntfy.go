package ntfy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"agentd/internal/assignments"
	"agentd/internal/notify"
)

const maxBodyChars = 1000

type Channel struct {
	HTTP        *http.Client
	TopicURL    string
	Token       string
	FullContent bool
}

func (c *Channel) Send(ctx context.Context, m notify.Message) (string, error) {
	title, body := "agentd: "+label(m.Severity), m.Assignment+" needs your attention. Open agentd to read it."
	if c.FullContent {
		title, body = m.Title, clip(m.Body)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.TopicURL, strings.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%w: invalid topic URL", notify.ErrPermanent)
	}
	req.Header.Set("Title", headerText(title))
	req.Header.Set("Priority", priority(m.Severity))
	req.Header.Set("Tags", "agentd,"+string(m.Severity))
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return "", fmt.Errorf("ntfy unreachable: %v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode/100 == 2:
		var out struct {
			ID string `json:"id"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		return out.ID, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500:
		return "", fmt.Errorf("ntfy returned %d", resp.StatusCode)
	default:
		return "", fmt.Errorf("%w: ntfy returned %d", notify.ErrPermanent, resp.StatusCode)
	}
}

func label(s assignments.Severity) string {
	switch s {
	case assignments.SeverityAction:
		return "action needed"
	case assignments.SeverityAttention:
		return "worth a look"
	}
	return "update"
}

func priority(s assignments.Severity) string {
	switch s {
	case assignments.SeverityAction:
		return "high"
	case assignments.SeverityAttention:
		return "default"
	}
	return "low"
}

func headerText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	for _, r := range s {
		if r >= utf8.RuneSelf {
			return mime.BEncoding.Encode("UTF-8", s)
		}
	}
	return s
}

func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxBodyChars {
		return s
	}
	return string([]rune(s)[:maxBodyChars]) + "…"
}
