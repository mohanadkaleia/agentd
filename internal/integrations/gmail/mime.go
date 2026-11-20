package gmail

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"
)

type Outgoing struct {
	From       string
	To         []string
	Subject    string
	Body       string
	InReplyTo  string
	References string
	MessageID  string
	Date       time.Time
}

func MessageIDFor(idempotencyKey string) string {
	sum := sha256.Sum256([]byte(idempotencyKey))
	return hex.EncodeToString(sum[:16]) + "@agentd.local"
}

func (o Outgoing) Build() ([]byte, error) {
	if len(o.To) == 0 {
		return nil, fmt.Errorf("at least one recipient is required")
	}
	to := make([]string, 0, len(o.To))
	for _, addr := range o.To {
		a, err := mail.ParseAddress(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid recipient %q: %w", addr, err)
		}
		to = append(to, a.String())
	}
	for name, v := range map[string]string{"subject": o.Subject, "in-reply-to": o.InReplyTo, "references": o.References, "from": o.From} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("%s must not contain line breaks", name)
		}
	}
	var b strings.Builder
	header := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	header("From", o.From)
	header("To", strings.Join(to, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", o.Subject))
	header("Date", o.Date.Format(time.RFC1123Z))
	header("Message-ID", "<"+o.MessageID+">")
	header("In-Reply-To", o.InReplyTo)
	header("References", o.References)
	header("MIME-Version", "1.0")
	header("Content-Type", `text/plain; charset="UTF-8"`)
	header("Content-Transfer-Encoding", "8bit")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(o.Body, "\r\n", "\n"), "\n", "\r\n"))
	return []byte(b.String()), nil
}
