package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h) // listens on 127.0.0.1
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewClientRequiresLoopback(t *testing.T) {
	for _, u := range []string{"http://example.com:3000", "http://10.0.0.5:3000", "https://127.0.0.1:3000", "ftp://localhost"} {
		if _, err := NewClient(u, ""); err == nil {
			t.Errorf("NewClient(%q) succeeded, want error", u)
		}
	}
	for _, u := range []string{"http://127.0.0.1:3000", "http://localhost:3000", "http://[::1]:3000"} {
		if _, err := NewClient(u, ""); err != nil {
			t.Errorf("NewClient(%q) = %v", u, err)
		}
	}
}

func TestClientMessagesLongPoll(t *testing.T) {
	var gotAuth, gotTimeout string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotTimeout = r.Header.Get("Authorization"), r.URL.Query().Get("timeout")
		if r.URL.Path != "/messages" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Write([]byte(`[{"messageId":"M1","chatId":"15551234567@s.whatsapp.net","senderId":"15551234567@s.whatsapp.net","body":"hi","fromMe":false,"timestamp":1700000000,"botIds":["1@s.whatsapp.net"],"readReceiptKey":{"remoteJid":"15551234567@s.whatsapp.net","id":"M1"}}]`))
	}))
	evs, err := c.Messages(context.Background(), 25*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok" || gotTimeout != "25" {
		t.Fatalf("auth=%q timeout=%q", gotAuth, gotTimeout)
	}
	if len(evs) != 1 || evs[0].Body != "hi" || evs[0].MessageID != "M1" || evs[0].ReadReceiptKey.ID != "M1" {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Time().Unix() != 1700000000 {
		t.Fatalf("Time = %v", evs[0].Time())
	}
}

func TestClientSend(t *testing.T) {
	var body map[string]string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"success":true,"messageId":"A","messageIds":["A","B"]}`))
	}))
	res, err := c.Send(context.Background(), "1@s.whatsapp.net", "hello", "Q1")
	if err != nil {
		t.Fatal(err)
	}
	if body["chatId"] != "1@s.whatsapp.net" || body["message"] != "hello" || body["replyTo"] != "Q1" {
		t.Fatalf("body = %v", body)
	}
	if res.MessageID != "A" || len(res.MessageIDs) != 2 {
		t.Fatalf("res = %+v", res)
	}
}

func TestClientAPIError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"boom","messageIds":["X"]}`))
	}))
	_, err := c.Send(context.Background(), "1@s.whatsapp.net", "hello", "")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 500 || ae.Message != "boom" || len(ae.MessageIDs) != 1 {
		t.Fatalf("err = %v", err)
	}
	if !IsAPIStatus(err, 500) || IsAPIStatus(err, 503) {
		t.Fatal("IsAPIStatus wrong")
	}
}

func TestClientHealthTypingRead(t *testing.T) {
	var paths []string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"connected","user":{"id":"1@s.whatsapp.net"}}`))
			return
		}
		w.Write([]byte(`{"success":true}`))
	}))
	h, err := c.Health(context.Background())
	if err != nil || !h.Connected() || h.User.ID != "1@s.whatsapp.net" {
		t.Fatalf("health = %+v, %v", h, err)
	}
	if err := c.Typing(context.Background(), "1@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}
	if err := c.MarkRead(context.Background(), ReadKey{RemoteJID: "1@s.whatsapp.net", ID: "M"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /health", "POST /typing", "POST /read"}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths = %v, want %v", paths, want)
		}
	}
}

func TestClientTransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, _ := NewClient(url, "")
	if _, err := c.Health(context.Background()); err == nil {
		t.Fatal("want error from closed server")
	}
}
