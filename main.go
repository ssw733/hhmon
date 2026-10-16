package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type searchRun struct {
	name    string
	search  Search
	profile string
	gen     Generator
}

type Monitor struct {
	cfg         Config
	store       *Store
	client      *http.Client
	searches    []searchRun
	log         *log.Logger
	seenThisRun map[string]bool
	applier     *Applier
	mu          sync.Mutex
	statusMu    sync.Mutex
	lastRun     time.Time
	lastNew     int
}

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	once := flag.Bool("once", false, "run a single monitoring cycle and exit")
	seedOnly := flag.Bool("seed", false, "only record current feed without generating letters")
	limit := flag.Int("limit", 0, "max new vacancies to process per cycle (0 = unlimited)")
	listN := flag.Int("list", 0, "print the N most recent saved vacancies (with cover letters) and exit")
	web := flag.Bool("web", false, "serve a local web UI")
	addr := flag.String("addr", "127.0.0.1:8080", "web UI listen address")
	exportPath := flag.String("export", "", "export a static HTML snapshot of the UI to this file and exit")
	applyID := flag.String("apply", "", "откликнуться на сохранённую вакансию по ID и выйти")
	applyPending := flag.Bool("apply-pending", false, "откликнуться на все сохранённые вакансии с письмами и выйти")
	flag.Parse()

	loadDotEnv(".env")

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	logger, logBuf, closeLog, err := setupLogger(cfg.LogPath)
	if err != nil {
		log.Fatalf("logger: %v", err)
	}
	defer closeLog()

	store, err := openStore(cfg.DBPath)
	if err != nil {
		logger.Fatalf("db: %v", err)
	}
	defer store.Close()

	if *listN > 0 {
		printRecent(store, logger, *listN)
		return
	}

	client := newHTTPClient(30 * time.Second)

	genCache := map[string]Generator{}
	runs := make([]searchRun, 0, len(cfg.Searches))
	for i := range cfg.Searches {
		s := cfg.Searches[i]
		profile, err := resolveProfile(s)
		if err != nil {
			logger.Fatalf("search %q profile: %v", s.Name, err)
		}
		gen, err := cachedGenerator(genCache, s, cfg, logger)
		if err != nil {
			logger.Fatalf("search %q llm: %v", s.Name, err)
		}
		logger.Printf("search %q: query=%q areas=%v llm=%s", s.Name, s.Query, s.Areas, describeLLM(s, cfg))
		runs = append(runs, searchRun{name: s.Name, search: s, profile: profile, gen: gen})
	}

	var applier *Applier
	if cfg.Apply.Enabled || *applyID != "" || *applyPending {
		ap, err := newApplier(cfg.Apply, store, runs[0].gen, runs[0].profile, logger)
		if err != nil {
			logger.Fatalf("apply: %v", err)
		}
		applier = ap
		if cfg.Apply.Enabled {
			logger.Printf("apply: включён (dry_run=%v, resume~%q, max/день=%d, пауза=%ds)",
				cfg.Apply.DryRun, cfg.Apply.ResumeTitleContains, cfg.Apply.MaxPerDay, cfg.Apply.MinDelaySeconds)
		}
	}

	m := &Monitor{
		cfg:         cfg,
		store:       store,
		client:      client,
		searches:    runs,
		log:         logger,
		seenThisRun: map[string]bool{},
		applier:     applier,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *exportPath != "" {
		ws := &webServer{cfg: cfg, store: store, mon: m, buf: logBuf}
		if err := ws.exportStatic(*exportPath); err != nil {
			logger.Fatalf("export: %v", err)
		}
		logger.Printf("preview exported: %s", *exportPath)
		return
	}

	if *applyID != "" {
		v, err := store.Get(*applyID)
		if err != nil {
			logger.Fatalf("apply: вакансия %s не найдена в базе (сначала прогони мониторинг): %v", *applyID, err)
		}
		if err := m.applyVacancy(ctx, v); err != nil {
			logger.Fatalf("apply: %v", err)
		}
		return
	}
	if *applyPending {
		n, err := m.applyPending(ctx)
		if err != nil {
			logger.Fatalf("apply-pending: %v", err)
		}
		logger.Printf("apply-pending: отправлено %d", n)
		return
	}

	if cfg.Apply.Enabled && !*once {
		m.startApplyWorker(ctx)
	}

	total, _ := store.Count()
	logger.Printf("=== hhmon started === searches=%d interval=%ds db_vacancies=%d",
		len(runs), cfg.PollIntervalSec, total)
	checkLLMReachability(cfg, logger)

	seed := *seedOnly
	if cfg.SeedOnFirstRun && total == 0 {
		logger.Printf("first run: seeding current feed (no letters); set seed_on_first_run=false to disable")
		seed = true
	}

	if *web {
		startWeb(*addr, cfg, store, m, logBuf, logger)
		logger.Printf("web UI: http://%s", webDisplayAddr(*addr))
		if *once {
			m.Run(ctx, seed, *limit, true)
			return
		}
		go m.Run(ctx, seed, *limit, false)
		<-ctx.Done()
		logger.Printf("shutting down")
		return
	}

	m.Run(ctx, seed, *limit, *once)
}

func startWeb(addr string, cfg Config, store *Store, m *Monitor, buf *lineBuffer, logger *log.Logger) {
	srv := &webServer{cfg: cfg, store: store, mon: m, buf: buf}
	httpSrv := &http.Server{Addr: addr, Handler: srv.routes()}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Printf("web server error: %v", err)
		}
	}()
}

func webDisplayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

func (m *Monitor) Run(ctx context.Context, seed bool, limit int, once bool) {
	for {
		m.mu.Lock()
		newCount, err := m.cycle(ctx, seed, limit)
		m.setStatus(newCount)
		m.mu.Unlock()
		if err != nil {
			m.log.Printf("cycle error: %v", err)
		} else {
			m.log.Printf("cycle done: %d new vacancy(ies)", newCount)
		}
		seed = false
		if once {
			return
		}
		select {
		case <-ctx.Done():
			m.log.Printf("shutting down")
			return
		case <-time.After(time.Duration(m.cfg.PollIntervalSec) * time.Second):
		}
	}
}

// ScanNow runs a single cycle on demand (web UI button).
func (m *Monitor) ScanNow(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.cycle(ctx, false, 0)
	m.setStatus(n)
	return n, err
}

func (m *Monitor) setStatus(newCount int) {
	m.statusMu.Lock()
	m.lastRun = time.Now()
	m.lastNew = newCount
	m.statusMu.Unlock()
}

func (m *Monitor) LastRun() time.Time {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	return m.lastRun
}

func (m *Monitor) LastNew() int {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	return m.lastNew
}

func (m *Monitor) cycle(ctx context.Context, seed bool, limit int) (int, error) {
	newCount := 0
	for _, run := range m.searches {
		items, err := fetchRSS(m.client, run.search.Query, run.search.Areas, run.search.SearchPeriodDays)
		if err != nil {
			m.log.Printf("[%s] rss error: %v", run.name, err)
			continue
		}
		sort.Slice(items, func(i, j int) bool { return items[i].PublishedAt.After(items[j].PublishedAt) })

		for _, it := range items {
			if ctx.Err() != nil {
				return newCount, ctx.Err()
			}
			if limit > 0 && newCount >= limit {
				return newCount, nil
			}
			if m.seenThisRun[it.ID] {
				continue
			}
			m.seenThisRun[it.ID] = true
			seen, err := m.store.Seen(it.ID)
			if err != nil {
				m.log.Printf("[%s] db check %s: %v", run.name, it.ID, err)
				continue
			}
			if seen {
				continue
			}
			if !matches(it.Title, run.search) {
				continue
			}
			if seed {
				_ = m.store.Save(Vacancy{ID: it.ID, Title: it.Title, Company: it.Company, URL: it.URL,
					Region: it.Region, Salary: it.Salary, FoundAt: time.Now(), PublishedAt: it.PublishedAt})
				continue
			}
			if err := m.handle(ctx, run, it); err != nil {
				m.log.Printf("[%s] handle %s: %v", run.name, it.ID, err)
				continue
			}
			newCount++
		}
		m.jitter()
	}
	return newCount, nil
}

func (m *Monitor) handle(ctx context.Context, run searchRun, it RSSItem) error {
	age := time.Since(it.PublishedAt).Round(time.Second)
	m.log.Printf("[%s] NEW [%s] %s | %s | %s | %s | опубликовано %s назад",
		run.name, it.ID, it.Title, it.Company, it.Region, it.Salary, age)

	v, err := fetchVacancy(m.client, it)
	if err != nil {
		m.log.Printf("  fetch full description failed: %v", err)
		v = Vacancy{ID: it.ID, Title: it.Title, Company: it.Company, URL: it.URL,
			Region: it.Region, Salary: it.Salary, FoundAt: time.Now(), PublishedAt: it.PublishedAt}
	}

	system, user := buildPrompt(run.profile, v)
	genCtx, cancel := context.WithTimeout(ctx, time.Duration(m.cfg.LLMTimeoutSec)*time.Second)
	letter, err := run.gen.Generate(genCtx, system, user)
	cancel()
	if err != nil {
		m.log.Printf("  llm error: %v", err)
	} else {
		v.CoverLetter = letter
		m.log.Printf("  --- сопроводительное [%s] ---\n%s\n  -----------------------------", run.name, letter)
	}

	if err := m.store.Save(v); err != nil {
		return err
	}
	m.log.Printf("  saved. url=%s", it.URL)
	return nil
}

func (m *Monitor) nextPending() (Vacancy, bool) {
	vs, err := m.store.PendingApplications(1)
	if err != nil {
		m.log.Printf("apply-worker: db error %v", err)
		return Vacancy{}, false
	}
	if len(vs) == 0 {
		return Vacancy{}, false
	}
	return vs[0], true
}

func (m *Monitor) dailyLimitReached() bool {
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	cnt, err := m.store.SubmittedSince(startOfDay)
	if err != nil {
		return false
	}
	return cnt >= m.cfg.Apply.MaxPerDay
}

// startApplyWorker applies to the oldest pending vacancy every
// min_delay_seconds, respecting the daily cap.
func (m *Monitor) startApplyWorker(ctx context.Context) {
	delay := time.Duration(m.cfg.Apply.MinDelaySeconds) * time.Second
	go func() {
		m.log.Printf("apply-worker: запущен, интервал %s, лимит %d/день", delay, m.cfg.Apply.MaxPerDay)
		for ctx.Err() == nil {
			if m.dailyLimitReached() {
				m.log.Printf("apply-worker: дневной лимит %d достигнут — пауза 10 мин", m.cfg.Apply.MaxPerDay)
				if !sleepCtx(ctx, 10*time.Minute) {
					return
				}
				continue
			}
			v, ok := m.nextPending()
			if !ok {
				if !sleepCtx(ctx, 10*time.Second) {
					return
				}
				continue
			}
			if err := m.applyVacancy(ctx, v); err != nil {
				m.log.Printf("apply-worker: %s ОШИБКА %v", v.ID, err)
			}
			if !sleepCtx(ctx, delay) {
				return
			}
		}
	}()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// applyVacancy generates a letter if missing, then performs the response.
func (m *Monitor) applyVacancy(ctx context.Context, v Vacancy) error {
	if v.CoverLetter == "" {
		system, user := buildPrompt(m.searches[0].profile, v)
		letter, err := m.searches[0].gen.Generate(ctx, system, user)
		if err != nil {
			return fmt.Errorf("сгенерировать письмо: %w", err)
		}
		v.CoverLetter = letter
		_ = m.store.Save(v)
	}
	status, err := m.applier.Apply(ctx, v)
	if err != nil {
		_ = m.store.RecordApplication(v.ID, "", "failed", v.CoverLetter, err.Error())
		return err
	}
	_ = m.store.RecordApplication(v.ID, "", status, v.CoverLetter, "")
	m.log.Printf("apply %s: %s", v.ID, status)
	return nil
}

func (m *Monitor) applyPending(ctx context.Context) (int, error) {
	delay := time.Duration(m.cfg.Apply.MinDelaySeconds) * time.Second
	sent := 0
	for ctx.Err() == nil {
		if m.cfg.Apply.MaxPerDay > 0 && sent >= m.cfg.Apply.MaxPerDay {
			m.log.Printf("apply-pending: достигнут лимит за прогон (%d)", m.cfg.Apply.MaxPerDay)
			break
		}
		v, ok := m.nextPending()
		if !ok {
			break
		}
		if err := m.applyVacancy(ctx, v); err != nil {
			m.log.Printf("apply %s: ОШИБКА %v", v.ID, err)
		} else {
			sent++
		}
		if delay > 0 && !sleepCtx(ctx, delay) {
			break
		}
	}
	return sent, nil
}

func (m *Monitor) jitter() {
	if m.cfg.RequestJitterMS <= 0 {
		return
	}
	time.Sleep(time.Duration(rand.Intn(m.cfg.RequestJitterMS)) * time.Millisecond)
}

func matches(title string, s Search) bool {
	low := strings.ToLower(title)
	for _, ex := range s.ExcludeKeywords {
		if ex != "" && strings.Contains(low, strings.ToLower(ex)) {
			return false
		}
	}
	if len(s.IncludeKeywords) == 0 {
		return true
	}
	for _, in := range s.IncludeKeywords {
		if in != "" && strings.Contains(low, strings.ToLower(in)) {
			return true
		}
	}
	return false
}

func chainFor(s Search, cfg Config) []LLMConfig {
	if len(s.LLMChain) > 0 {
		return s.LLMChain
	}
	return cfg.LLMChain
}

func buildGenerator(s Search, cfg Config, logger *log.Logger) (Generator, error) {
	if chain := chainFor(s, cfg); len(chain) > 0 {
		return newChainGenerator(chain, logger)
	}
	return newGenerator(mergeLLM(cfg.LLM, s.LLM))
}

// cachedGenerator builds each unique LLM chain once and reuses it,
// so startup logs are not duplicated per search.
func cachedGenerator(cache map[string]Generator, s Search, cfg Config, logger *log.Logger) (Generator, error) {
	var key string
	if chain := chainFor(s, cfg); len(chain) > 0 {
		b, _ := json.Marshal(chain)
		key = "chain:" + string(b)
	} else {
		b, _ := json.Marshal(mergeLLM(cfg.LLM, s.LLM))
		key = "single:" + string(b)
	}
	if g, ok := cache[key]; ok {
		return g, nil
	}
	g, err := buildGenerator(s, cfg, logger)
	if err != nil {
		return nil, err
	}
	cache[key] = g
	return g, nil
}

func describeLLM(s Search, cfg Config) string {
	if chain := chainFor(s, cfg); len(chain) > 0 {
		names := make([]string, len(chain))
		for i, c := range chain {
			names[i] = c.Provider + "/" + c.Model
		}
		return "chain[" + strings.Join(names, " -> ") + "]"
	}
	llm := mergeLLM(cfg.LLM, s.LLM)
	return llm.Provider + "/" + llm.Model
}

func resolveProfile(s Search) (string, error) {
	if strings.TrimSpace(s.Profile) != "" {
		return strings.TrimSpace(s.Profile), nil
	}
	if len(s.ProfilePaths) > 0 {
		var parts []string
		for _, p := range s.ProfilePaths {
			b, err := loadProfile(p)
			if err != nil {
				return "", err
			}
			parts = append(parts, b)
		}
		return strings.Join(parts, "\n\n"), nil
	}
	return loadProfile(s.ProfilePath)
}
func setupLogger(path string) (*log.Logger, *lineBuffer, func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, nil, err
	}
	buf := newLineBuffer(1000)
	logger := log.New(io.MultiWriter(os.Stdout, f, buf), "", log.LstdFlags)
	return logger, buf, func() { f.Close() }, nil
}

// loadDotEnv reads simple KEY=VALUE lines from a .env file (if present)
// and sets them as environment variables (without overriding existing ones),
// so API keys can live in .env instead of config.json.
func loadDotEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k != "" && os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func printRecent(store *Store, logger *log.Logger, n int) {
	vs, err := store.Recent(n)
	if err != nil {
		logger.Fatalf("recent: %v", err)
	}
	for _, v := range vs {
		fmt.Printf("\n=== #%s %s\n%s | %s | %s\n%s\n",
			v.ID, v.Title, v.Company, v.Region, v.Salary, v.URL)
		if v.CoverLetter == "" {
			fmt.Println("(сопроводительное не сгенерировано)")
		} else {
			fmt.Println(v.CoverLetter)
		}
	}
}
