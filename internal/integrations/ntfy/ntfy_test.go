package ntfy_test

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentd/internal/assignments"
	"agentd/internal/integrations/ntfy"
	"agentd/internal/notify"
)

type received struct {
	title, priority, body string
}

func server(t *testing.T, status int) (*ntfy.Channel, *[]received) {
	var got []received
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		title, _ := new(mime.WordDecoder).DecodeHeader(r.Header.Get("Title"))
		got = append(got, received{title, r.Header.Get("Priority"), string(body)})
		w.WriteHeader(status)
		io.WriteString(w, `{"id":"msg-1"}`)
	}))
	t.Cleanup(srv.Close)
	return &ntfy.Channel{HTTP: srv.Client(), TopicURL: srv.URL + "/agentd-secret-topic"}, &got
}

var alert = notify.Message{ID: "ntf_1", Assignment: "Portfolio", Severity: assignments.SeverityAction,
	Title: "استرداد: Refund approved\r\nX-Injected: 1", Body: "Account 1234 balance is $5,000"}

func TestDefaultPushRevealsNothingButThatAttentionIsNeeded(t *testing.T) {
	ch, got := server(t, 200)
	ref, err := ch.Send(context.Background(), alert)
	if err != nil || ref != "msg-1" {
		t.Fatalf("send = %q, %v", ref, err)
	}
	r := (*got)[0]
	if strings.Contains(r.body+r.title, "1234") || strings.Contains(r.title, "Refund") {
		t.Fatalf("private push leaked content: %+v", r)
	}
	if !strings.Contains(r.body, "Portfolio needs your attention") || r.priority != "high" {
		t.Fatalf("push = %+v", r)
	}
}

func TestFullContentIsHeaderSafe(t *testing.T) {
	ch, got := server(t, 200)
	ch.FullContent = true
	if _, err := ch.Send(context.Background(), alert); err != nil {
		t.Fatal(err)
	}
	r := (*got)[0]
	if r.title != "استرداد: Refund approved  X-Injected: 1" || r.body != alert.Body {
		t.Fatalf("push = %+v", r)
	}
}

func TestErrorsAreClassifiedAndNeverShowTheTopic(t *testing.T) {
	for status, permanent := range map[int]bool{503: false, 429: false, 403: true, 400: true} {
		ch, _ := server(t, status)
		_, err := ch.Send(context.Background(), alert)
		if err == nil || errors.Is(err, notify.ErrPermanent) != permanent {
			t.Fatalf("%d: err = %v, want permanent=%v", status, err, permanent)
		}
	}
	ch := &ntfy.Channel{HTTP: http.DefaultClient, TopicURL: "http://127.0.0.1:1/agentd-secret-topic"}
	_, err := ch.Send(context.Background(), alert)
	if err == nil || strings.Contains(err.Error(), "secret-topic") || errors.Is(err, notify.ErrPermanent) {
		t.Fatalf("unreachable server err = %v, want a transient error without the topic", err)
	}
}
