package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
)

type Generator interface {
	Generate(ctx context.Context, system, user string) (string, error)
}

func newGenerator(cfg LLMConfig) (Generator, error) {
	client := newHTTPClient(0)
	switch strings.ToLower(cfg.Provider) {
	case "ollama", "":
		return &ollamaGen{client: client, cfg: cfg}, nil
	case "gemini":
		return &geminiGen{client: client, cfg: cfg}, nil
	case "openai", "groq", "openrouter", "deepseek", "opencode":
		return &openaiGen{client: client, cfg: cfg}, nil
	default:
		return nil, fmt.Errorf("unknown llm provider %q", cfg.Provider)
	}
}

func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, payload any) ([]byte, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &llmHTTPError{code: resp.StatusCode, body: string(body)}
	}
	return body, nil
}

type llmHTTPError struct {
	code int
	body string
}

func (e *llmHTTPError) Error() string {
	return fmt.Sprintf("llm http %d: %s", e.code, truncate(e.body, 300))
}

// retryable reports whether it makes sense to switch to another provider.
// Any failure (limits, quota, auth, network, timeout, 5xx) is treated as
// switchable; only the last provider's error is surfaced if all fail.
func retryable(err error) bool { return true }

// ---- Provider chain with automatic failover ----

type chainEntry struct {
	name string
	gen  Generator
}

type chainGen struct {
	entries []chainEntry
	log     *log.Logger
	mu      sync.Mutex
	cur     int
}

func newChainGenerator(cfgs []LLMConfig, logger *log.Logger) (Generator, error) {
	if len(cfgs) == 0 {
		return nil, fmt.Errorf("empty llm chain")
	}
	c := &chainGen{log: logger}
	for _, cfg := range cfgs {
		g, err := newGenerator(cfg)
		if err != nil {
			return nil, err
		}
		name := cfg.Provider
		if cfg.Model != "" {
			name += "/" + cfg.Model
		}
		c.entries = append(c.entries, chainEntry{name: name, gen: g})
	}
	return c, nil
}

func (c *chainGen) Generate(ctx context.Context, system, user string) (string, error) {
	c.mu.Lock()
	start := c.cur
	c.mu.Unlock()

	n := len(c.entries)
	var lastErr error
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		idx := (start + i) % n
		entry := c.entries[idx]
		out, err := entry.gen.Generate(ctx, system, user)
		if err == nil {
			c.mu.Lock()
			if c.cur != idx {
				c.log.Printf("llm: using %s", entry.name)
				c.cur = idx
			}
			c.mu.Unlock()
			return out, nil
		}
		lastErr = err
		if !retryable(err) {
			return "", fmt.Errorf("%s: %w", entry.name, err)
		}
		c.log.Printf("llm: %s failed (%v), switching to next", entry.name, err)
	}
	return "", fmt.Errorf("all %d llm providers failed, last: %w", n, lastErr)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---- Ollama (local, unlimited) ----

type ollamaGen struct {
	client *http.Client
	cfg    LLMConfig
}

func (g *ollamaGen) Generate(ctx context.Context, system, user string) (string, error) {
	base := g.cfg.BaseURL
	if base == "" {
		base = "http://localhost:11434"
	}
	payload := map[string]any{
		"model":  g.cfg.Model,
		"stream": false,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"options": map[string]any{"temperature": g.cfg.Temperature},
	}
	body, err := postJSON(ctx, g.client, strings.TrimRight(base, "/")+"/api/chat", nil, payload)
	if err != nil {
		return "", err
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Message.Content), nil
}

// ---- Google Gemini ----

type geminiGen struct {
	client *http.Client
	cfg    LLMConfig
}

func (g *geminiGen) Generate(ctx context.Context, system, user string) (string, error) {
	model := g.cfg.Model
	if model == "" {
		model = "gemini-1.5-flash"
	}
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, g.cfg.APIKey)
	payload := map[string]any{
		"system_instruction": map[string]any{"parts": []map[string]string{{"text": system}}},
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]string{{"text": user}}},
		},
		"generationConfig": map[string]any{
			"temperature":     g.cfg.Temperature,
			"maxOutputTokens": g.cfg.MaxTokens,
		},
	}
	body, err := postJSON(ctx, g.client, url, nil, payload)
	if err != nil {
		return "", err
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini: empty response")
	}
	return strings.TrimSpace(out.Candidates[0].Content.Parts[0].Text), nil
}

// ---- OpenAI-compatible (Groq / OpenRouter / OpenAI) ----

type openaiGen struct {
	client *http.Client
	cfg    LLMConfig
}

func (g *openaiGen) Generate(ctx context.Context, system, user string) (string, error) {
	base := g.cfg.BaseURL
	if base == "" {
		switch strings.ToLower(g.cfg.Provider) {
		case "groq":
			base = "https://api.groq.com/openai/v1"
		case "openrouter":
			base = "https://openrouter.ai/api/v1"
		case "deepseek":
			base = "https://api.deepseek.com"
		case "opencode":
			base = "https://opencode.ai/inference/openai/v1"
		default:
			base = "https://api.openai.com/v1"
		}
	}
	model := g.cfg.Model
	if model == "" && strings.EqualFold(g.cfg.Provider, "deepseek") {
		model = "deepseek-chat"
	}
	payload := map[string]any{
		"model":       model,
		"temperature": g.cfg.Temperature,
		"max_tokens":  g.cfg.MaxTokens,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	headers := map[string]string{"Authorization": "Bearer " + g.cfg.APIKey}
	body, err := postJSON(ctx, g.client, strings.TrimRight(base, "/")+"/chat/completions", headers, payload)
	if err != nil {
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("openai-compatible: empty response")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}
