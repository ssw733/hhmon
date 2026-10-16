package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type LLMConfig struct {
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	BaseURL     string  `json:"base_url"`
	APIKey      string  `json:"api_key"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
}

// Search describes a single monitoring query, with its own filters,
// profile and (optionally) its own LLM override.
type Search struct {
	Name             string      `json:"name"`
	Query            string      `json:"query"`
	Areas            []int       `json:"areas"`
	SearchPeriodDays int         `json:"search_period_days"`
	IncludeKeywords  []string    `json:"include_keywords"`
	ExcludeKeywords  []string    `json:"exclude_keywords"`
	Profile          string      `json:"profile"`
	ProfilePath      string      `json:"profile_path"`
	ProfilePaths     []string    `json:"profile_paths"`
	LLM              *LLMConfig  `json:"llm,omitempty"`
	LLMChain         []LLMConfig `json:"llm_chain,omitempty"`
}

// ApplyConfig controls automatic responses to vacancies.
type ApplyConfig struct {
	Enabled             bool   `json:"enabled"`
	DryRun              bool   `json:"dry_run"`
	MaxPerDay           int    `json:"max_per_day"`
	MinDelaySeconds     int    `json:"min_delay_seconds"`
	SessionPath         string `json:"session_path"`
	ResumeTitleContains string `json:"resume_title_contains"`
	QuestionsViaLLM     bool   `json:"questions_via_llm"`
	SkipWithQuestions   bool   `json:"skip_with_questions"`
	BaseURL             string `json:"base_url"`
	Engine              string `json:"engine"`
}

type Config struct {
	PollIntervalSec int         `json:"poll_interval_seconds"`
	RequestJitterMS int         `json:"request_jitter_ms"`
	LLMTimeoutSec   int         `json:"llm_timeout_seconds"`
	SeedOnFirstRun  bool        `json:"seed_on_first_run"`
	DBPath          string      `json:"db_path"`
	LogPath         string      `json:"log_path"`
	LLM             LLMConfig   `json:"llm"`
	LLMChain        []LLMConfig `json:"llm_chain"`
	Apply           ApplyConfig `json:"apply"`
	Searches        []Search    `json:"searches"`
}

func defaultLLM() LLMConfig {
	return LLMConfig{
		Provider:    "ollama",
		Model:       "qwen2.5:14b",
		BaseURL:     "http://localhost:11434",
		Temperature: 0.6,
		MaxTokens:   1200,
	}
}

func loadConfig(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	if cfg.PollIntervalSec < 5 {
		cfg.PollIntervalSec = 5
	}
	if cfg.LLMTimeoutSec <= 0 {
		cfg.LLMTimeoutSec = 180
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "hhmon.db"
	}
	if cfg.LogPath == "" {
		cfg.LogPath = "hhmon.log"
	}
	if cfg.LLM.Provider == "" {
		cfg.LLM = defaultLLM()
	}
	if cfg.Apply.SessionPath == "" {
		cfg.Apply.SessionPath = "session.json"
	}
	if cfg.Apply.MaxPerDay <= 0 {
		cfg.Apply.MaxPerDay = 20
	}
	if cfg.Apply.MinDelaySeconds <= 0 {
		cfg.Apply.MinDelaySeconds = 120
	}
	if cfg.Apply.ResumeTitleContains == "" {
		cfg.Apply.ResumeTitleContains = "backend"
	}
	if cfg.Apply.BaseURL == "" {
		cfg.Apply.BaseURL = "https://hh.ru"
	}
	if cfg.Apply.Engine == "" {
		cfg.Apply.Engine = "browser"
	}
	if len(cfg.Searches) == 0 {
		return cfg, fmt.Errorf("config: no searches defined")
	}
	for i := range cfg.Searches {
		s := &cfg.Searches[i]
		if s.Name == "" {
			s.Name = s.Query
		}
		if s.Query == "" {
			return cfg, fmt.Errorf("config: searches[%d] has empty query", i)
		}
		if s.SearchPeriodDays == 0 {
			s.SearchPeriodDays = 1
		}
		if s.Profile == "" && s.ProfilePath == "" && len(s.ProfilePaths) == 0 {
			s.ProfilePath = "profile.md"
		}
	}
	return cfg, nil
}

func mergeLLM(base LLMConfig, override *LLMConfig) LLMConfig {
	if override == nil {
		return base
	}
	out := base
	if override.Provider != "" {
		out.Provider = override.Provider
	}
	if override.Model != "" {
		out.Model = override.Model
	}
	if override.BaseURL != "" {
		out.BaseURL = override.BaseURL
	}
	if override.APIKey != "" {
		out.APIKey = override.APIKey
	}
	if override.Temperature != 0 {
		out.Temperature = override.Temperature
	}
	if override.MaxTokens != 0 {
		out.MaxTokens = override.MaxTokens
	}
	return out
}
