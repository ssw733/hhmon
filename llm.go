package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

func resolveKey(k string) string {
	if strings.HasPrefix(k, "env:") {
		return os.Getenv(strings.TrimPrefix(k, "env:"))
	}
	return k
}

// isLocalURL reports whether base is a localhost address (keyless local gateway).
func isLocalURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "::1" || strings.HasPrefix(h, "127.")
}

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
	case "openai", "groq", "openrouter", "deepseek", "opencode", "yandex",
		"cerebras", "mistral", "nvidia", "together", "xai", "fireworks":
		return &openaiGen{client: client, cfg: cfg}, nil
	case "gigachat":
		return &gigachatGen{client: gigachatClient(), cfg: cfg}, nil
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

// checkLLMReachability pings configured Ollama endpoints and logs a clear
// warning if they are unreachable (letters/applies won't work without an LLM).
func checkLLMReachability(cfg Config, logger *log.Logger) {
	seen := map[string]bool{}
	var urls []string
	collect := func(c LLMConfig) {
		if !strings.EqualFold(c.Provider, "ollama") {
			return
		}
		base := c.BaseURL
		if base == "" {
			base = "http://localhost:11434"
		}
		if !seen[base] {
			seen[base] = true
			urls = append(urls, base)
		}
	}
	collect(cfg.LLM)
	for _, c := range cfg.LLMChain {
		collect(c)
	}
	for _, s := range cfg.Searches {
		for _, c := range s.LLMChain {
			collect(c)
		}
		if s.LLM != nil {
			collect(*s.LLM)
		}
	}
	client := newHTTPClient(4 * time.Second)
	for _, base := range urls {
		resp, err := client.Get(strings.TrimRight(base, "/") + "/api/tags")
		if err != nil {
			logger.Printf("LLM: Ollama НЕ доступна (%s): %v", base, err)
			logger.Printf("LLM: запусти `ollama serve` и `ollama pull <модель>` — иначе письма и отклики работать не будут")
			continue
		}
		code := resp.StatusCode
		resp.Body.Close()
		if code == http.StatusOK {
			logger.Printf("LLM: Ollama доступна (%s)", base)
		} else {
			logger.Printf("LLM: Ollama ответила HTTP %d (%s)", code, base)
		}
	}
}

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
		if !strings.EqualFold(cfg.Provider, "ollama") && resolveKey(cfg.APIKey) == "" && !isLocalURL(cfg.BaseURL) {
			logger.Printf("LLM: %s/%s пропущен — не задан api_key (используй \"env:VAR\" или локальный base_url)", cfg.Provider, cfg.Model)
			continue
		}
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
	if len(c.entries) == 0 {
		logger.Printf("LLM: ни один провайдер не настроен (нет api_key) — мониторинг будет без писем")
	}
	return c, nil
}

func (c *chainGen) Generate(ctx context.Context, system, user string) (string, error) {
	c.mu.Lock()
	start := c.cur
	c.mu.Unlock()

	n := len(c.entries)
	if n == 0 {
		return "", fmt.Errorf("нет настроенных LLM: задай api_key (например \"env:OPENROUTER_API_KEY\") или подними Ollama")
	}
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
		case "cerebras":
			base = "https://api.cerebras.ai/v1"
		case "mistral":
			base = "https://api.mistral.ai/v1"
		case "nvidia":
			base = "https://integrate.api.nvidia.com/v1"
		case "together":
			base = "https://api.together.xyz/v1"
		case "xai":
			base = "https://api.x.ai/v1"
		case "fireworks":
			base = "https://api.fireworks.ai/inference/v1"
		case "yandex":
			base = "https://llm.api.cloud.yandex.net/v1"
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
	auth := "Bearer " + resolveKey(g.cfg.APIKey)
	if strings.EqualFold(g.cfg.Provider, "yandex") {
		auth = "Api-Key " + resolveKey(g.cfg.APIKey)
	}
	headers := map[string]string{"Authorization": auth}
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

// ---- GigaChat (Sber) ----

func gigachatClient() *http.Client {
	tlsCfg := &tls.Config{}
	if ca := os.Getenv("GIGACHAT_CA"); ca != "" {
		if pem, err := os.ReadFile(ca); err == nil {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pem)
			tlsCfg.RootCAs = pool
		}
	}
	if tlsCfg.RootCAs == nil {
		// Российский Trusted Root CA отсутствует в стандартном хранилище Go.
		tlsCfg.InsecureSkipVerify = true //nolint:gosec
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
}

type gigachatGen struct {
	client *http.Client
	cfg    LLMConfig
	mu     sync.Mutex
	token  string
	exp    time.Time
}

func (g *gigachatGen) accessToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token != "" && time.Now().Before(g.exp.Add(-time.Minute)) {
		return g.token, nil
	}
	authURL := os.Getenv("GIGACHAT_OAUTH_URL")
	if authURL == "" {
		authURL = "https://ngw.devices.sberbank.ru:9443/api/v2/oauth"
	}
	scope := os.Getenv("GIGACHAT_SCOPE")
	if scope == "" {
		scope = "GIGACHAT_API_PERS"
	}
	form := url.Values{"scope": {scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("RqUID", uuid4())
	req.Header.Set("Authorization", "Basic "+resolveKey(g.cfg.APIKey))

	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gigachat oauth %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresAt   int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("gigachat: пустой access_token")
	}
	g.token = out.AccessToken
	if out.ExpiresAt > 0 {
		g.exp = time.UnixMilli(out.ExpiresAt)
	} else {
		g.exp = time.Now().Add(25 * time.Minute)
	}
	return g.token, nil
}

func (g *gigachatGen) Generate(ctx context.Context, system, user string) (string, error) {
	tok, err := g.accessToken(ctx)
	if err != nil {
		return "", err
	}
	base := g.cfg.BaseURL
	if base == "" {
		base = "https://gigachat.devices.sberbank.ru/api/v1"
	}
	model := g.cfg.Model
	if model == "" {
		model = "GigaChat"
	}
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature": g.cfg.Temperature,
	}
	if g.cfg.MaxTokens > 0 {
		payload["max_tokens"] = g.cfg.MaxTokens
	}
	headers := map[string]string{"Authorization": "Bearer " + tok}
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
		return "", fmt.Errorf("gigachat: empty response")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
