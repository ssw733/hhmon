package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestYandexAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok-yandex"}}]}`)
	}))
	defer srv.Close()

	g := &openaiGen{client: srv.Client(), cfg: LLMConfig{
		Provider: "yandex", Model: "gpt://folder/yandexgpt-lite", APIKey: "KEY", BaseURL: srv.URL,
	}}
	out, err := g.Generate(context.Background(), "s", "u")
	if err != nil {
		t.Fatal(err)
	}
	if out != "ok-yandex" {
		t.Fatalf("out = %q", out)
	}
	if gotAuth != "Api-Key KEY" {
		t.Fatalf("Authorization = %q, want Api-Key KEY", gotAuth)
	}
}

func TestGigaChatOAuthAndChat(t *testing.T) {
	var chatAuth string
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"content":"привет-гига"}}]}`)
	}))
	defer chat.Close()

	var gotRqUID bool
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRqUID = r.Header.Get("RqUID") != ""
		io.WriteString(w, fmt.Sprintf(`{"access_token":"TOK","expires_at":%d}`, time.Now().Add(time.Hour).UnixMilli()))
	}))
	defer oauth.Close()

	t.Setenv("GIGACHAT_OAUTH_URL", oauth.URL)
	g := &gigachatGen{client: chat.Client(), cfg: LLMConfig{Provider: "gigachat", APIKey: "cid:secret", BaseURL: chat.URL}}

	out, err := g.Generate(context.Background(), "s", "u")
	if err != nil {
		t.Fatal(err)
	}
	if out != "привет-гига" {
		t.Fatalf("out = %q", out)
	}
	if !gotRqUID {
		t.Error("ожидался заголовок RqUID в OAuth-запросе")
	}
	if chatAuth != "Bearer TOK" {
		t.Fatalf("chat Authorization = %q, want Bearer TOK", chatAuth)
	}
}

func TestChainSkipsProvidersWithoutKey(t *testing.T) {
	gen, err := newChainGenerator([]LLMConfig{
		{Provider: "groq", Model: "x", APIKey: ""},
		{Provider: "openrouter", Model: "y", APIKey: "env:NOPE_MISSING"},
	}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	chain := gen.(*chainGen)
	if len(chain.entries) != 0 {
		t.Fatalf("ожидалось 0 активных провайдеров, получено %d", len(chain.entries))
	}
	if _, err := chain.Generate(context.Background(), "s", "u"); err == nil {
		t.Fatal("ожидалась ошибка при пустой цепочке")
	}
}
