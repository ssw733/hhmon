package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

type Applier struct {
	cfg     ApplyConfig
	client  *http.Client
	ua      string
	log     *log.Logger
	gen     Generator
	profile string
	store   *Store
	resumes []resume
}

type resume struct {
	Hash  string
	Title string
}

var resumeHashRe = regexp.MustCompile(`/resume/([0-9a-f]{40})`)

func newApplier(cfg ApplyConfig, store *Store, gen Generator, profile string, logger *log.Logger) (*Applier, error) {
	client, ua, err := loadSession(cfg.SessionPath)
	if err != nil {
		return nil, err
	}
	return &Applier{cfg: cfg, client: client, ua: ua, log: logger, gen: gen, profile: profile, store: store}, nil
}

func (a *Applier) base() string { return strings.TrimRight(a.cfg.BaseURL, "/") }

func (a *Applier) userAgent() string {
	if a.ua != "" {
		return a.ua
	}
	return browserUA
}

func (a *Applier) get(ctx context.Context, rawurl string, referer string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", a.userAgent())
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9")
	if referer != "" {
		req.Header.Set("Referer", referer)
	} else {
		req.Header.Set("Referer", a.base()+"/")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp, body, nil
}

// resolveResume fetches /applicant/resumes and picks a resume by title.
func (a *Applier) resolveResume(ctx context.Context) (resume, error) {
	if len(a.resumes) > 0 {
		return a.pickResume(), nil
	}
	resp, body, err := a.get(ctx, a.base()+"/applicant/resumes", "")
	if err != nil {
		return resume{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return resume{}, fmt.Errorf("resumes status %d (сессия не принята — перезайди: node tools/hh-login.mjs). Ответ: %s",
			resp.StatusCode, truncate(strings.TrimSpace(string(body)), 200))
	}
	a.resumes = parseResumes(body)
	if len(a.resumes) == 0 {
		return resume{}, fmt.Errorf("не найдено ни одного резюме на /applicant/resumes")
	}
	for _, r := range a.resumes {
		a.log.Printf("resume: %s | %s", r.Hash[:8]+"…", r.Title)
	}
	return a.pickResume(), nil
}

func (a *Applier) pickResume() resume {
	want := strings.ToLower(a.cfg.ResumeTitleContains)
	for _, r := range a.resumes {
		if want != "" && strings.Contains(strings.ToLower(r.Title), want) {
			return r
		}
	}
	return a.resumes[0]
}

func parseResumes(body []byte) []resume {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []resume
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			href := attr(n, "href")
			if m := resumeHashRe.FindStringSubmatch(href); m != nil && !seen[m[1]] {
				seen[m[1]] = true
				title := strings.TrimSpace(textContent(n))
				out = append(out, resume{Hash: m[1], Title: title})
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

// Apply performs a real (or dry-run) response to a vacancy.
func (a *Applier) Apply(ctx context.Context, v Vacancy) (string, error) {
	if strings.EqualFold(a.cfg.Engine, "browser") {
		return a.applyBrowser(ctx, v)
	}
	res, err := a.resolveResume(ctx)
	if err != nil {
		return "failed", err
	}

	formURL := fmt.Sprintf("%s/applicant/vacancy_response?vacancyId=%s&hhtmFrom=vacancy", a.base(), url.QueryEscape(v.ID))
	resp, body, err := a.get(ctx, formURL, v.URL)
	if err != nil {
		return "failed", err
	}
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusUnauthorized {
		return "failed", fmt.Errorf("сессия не авторизована (status %d)", resp.StatusCode)
	}

	action, fields := parseResponseForm(string(body))
	if action == "" {
		return "failed", fmt.Errorf("не нашёл форму отклика (возможно, вакансия недоступна или нужны вопросы)")
	}

	questions := questionFields(fields)
	if len(questions) > 0 && a.cfg.SkipWithQuestions {
		a.log.Printf("  apply: вакансия %s требует ответов на %d вопрос(ов) — пропускаю (skip_with_questions)", v.ID, len(questions))
		return "skipped", nil
	}

	answers := map[string]string{}
	if len(questions) > 0 && a.cfg.QuestionsViaLLM {
		for _, q := range questions {
			ans, err := a.answerQuestion(ctx, v, q.Question)
			if err != nil {
				a.log.Printf("  apply: не смог сгенерировать ответ на вопрос: %v", err)
				continue
			}
			answers[q.Name] = ans
			a.log.Printf("  apply: вопрос %q -> ответ %q", truncate(q.Question, 80), truncate(ans, 80))
		}
	}

	vals := buildFormValues(fields, res.Hash, v.CoverLetter, answers, a.xsrf())

	if a.cfg.DryRun {
		a.log.Printf("  apply DRY-RUN: POST %s", a.absolute(action))
		a.log.Printf("  apply DRY-RUN: %s", vals.Encode())
		return "dry_run", nil
	}

	status, err := a.submit(ctx, a.absolute(action), vals, formURL)
	if err != nil {
		return "failed", err
	}
	a.log.Printf("  apply: отклик отправлен на %s (status=%s)", v.ID, status)
	return status, nil
}

type applyResult struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

func parseApplyResult(out string) applyResult {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "{") {
			var r applyResult
			if json.Unmarshal([]byte(line), &r) == nil && r.Status != "" {
				return r
			}
		}
	}
	return applyResult{}
}

// applyBrowser performs the response through a real browser (Playwright),
// because hh.ru drops/closes raw non-browser HTTP requests on /applicant/*.
func (a *Applier) applyBrowser(ctx context.Context, v Vacancy) (string, error) {
	if strings.TrimSpace(v.CoverLetter) == "" {
		return "failed", fmt.Errorf("нет сопроводительного письма")
	}
	tmp, err := os.CreateTemp("", "hh-letter-*.txt")
	if err != nil {
		return "failed", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(v.CoverLetter); err != nil {
		tmp.Close()
		return "failed", err
	}
	tmp.Close()

	args := []string{"tools/hh-apply.mjs", "--id", v.ID, "--letter", tmp.Name(), "--session", a.cfg.SessionPath}
	if a.cfg.DryRun {
		args = append(args, "--dry")
	}
	cmd := exec.CommandContext(ctx, "node", args...)
	out, err := cmd.CombinedOutput()
	txt := strings.TrimSpace(string(out))
	for _, line := range strings.Split(txt, "\n") {
		if strings.TrimSpace(line) != "" {
			a.log.Printf("  node: %s", line)
		}
	}
	if err != nil {
		return "failed", fmt.Errorf("браузерный отклик: %v", err)
	}
	res := parseApplyResult(txt)
	if res.Status == "" {
		return "failed", fmt.Errorf("не получил результат от браузера")
	}
	if res.Status == "failed" && res.Error != "" {
		return "failed", fmt.Errorf("%s", res.Error)
	}
	return res.Status, nil
}

func (a *Applier) submit(ctx context.Context, action string, vals url.Values, referer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, action, strings.NewReader(vals.Encode()))
	if err != nil {
		return "failed", err
	}
	req.Header.Set("User-Agent", a.userAgent())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json, text/html, */*")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9")
	req.Header.Set("Referer", referer)
	req.Header.Set("X-Xsrftoken", vals.Get("_xsrf"))
	req.Header.Set("Origin", a.base())

	resp, err := a.client.Do(req)
	if err != nil {
		return "failed", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 400 {
		return "failed", fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	low := strings.ToLower(string(body))
	for _, bad := range []string{"error", "ошиб", "капча", "captcha", "too many"} {
		if strings.Contains(low, bad) {
			return "failed", fmt.Errorf("сервер вернул ошибку: %s", truncate(string(body), 300))
		}
	}
	return "submitted", nil
}

func (a *Applier) answerQuestion(ctx context.Context, v Vacancy, question string) (string, error) {
	system := "Ты помогаешь соискателю отвечать на вопросы работодателя при отклике на hh.ru. " +
		"Отвечай на русском, кратко (1-2 предложения), только на основе фактов из профиля. Не выдумывай."
	user := "Профиль кандидата:\n" + a.profile +
		"\n\nВакансия: " + v.Title + " (" + v.Company + ")" +
		"\n\nВопрос работодателя: " + question + "\n\nНапиши только текст ответа."
	genCtx, cancel := context.WithTimeout(ctx, time.Duration(a.cfgTimeout())*time.Second)
	defer cancel()
	return a.gen.Generate(genCtx, system, user)
}

func (a *Applier) cfgTimeout() int { return 120 }

func (a *Applier) absolute(action string) string {
	if strings.HasPrefix(action, "http") {
		return action
	}
	if strings.HasPrefix(action, "/") {
		return a.base() + action
	}
	return a.base() + "/" + action
}

// ---- form parsing ----

type formField struct {
	Name       string
	Value      string
	Type       string
	Checked    bool
	IsTextarea bool
	Question   string
}

var systemFieldNames = map[string]bool{
	"_xsrf": true, "vacancy_id": true, "vacancyId": true, "employer_id": true,
	"resume_hash": true, "resume_id": true, "letter": true, "template_id": true,
	"hhtmFrom": true, "hhtmFromLabel": true, "after_login": true,
}

func isSystemField(name string) bool {
	if systemFieldNames[name] {
		return true
	}
	switch name {
	case "resume", "resumeHash", "resume_hash", "resumeId", "resume_id":
		return true
	}
	return false
}

func questionFields(fields []formField) []formField {
	var out []formField
	for _, f := range fields {
		if f.Name == "" || isSystemField(f.Name) {
			continue
		}
		isLetter := strings.Contains(strings.ToLower(f.Name), "letter")
		if isLetter {
			continue
		}
		if f.IsTextarea || f.Type == "text" || f.Type == "" {
			out = append(out, f)
		}
	}
	return out
}

func buildFormValues(fields []formField, resumeHash, letter string, answers map[string]string, xsrf string) url.Values {
	vals := url.Values{}
	for _, f := range fields {
		if f.Name == "" {
			continue
		}
		lname := strings.ToLower(f.Name)
		switch {
		case f.IsTextarea && strings.Contains(lname, "letter"):
			vals.Set(f.Name, letter)
		case answers[f.Name] != "":
			vals.Set(f.Name, answers[f.Name])
		case f.IsTextarea:
			vals.Set(f.Name, answers[f.Name])
		case f.Type == "radio":
			if strings.Contains(lname, "resume") && f.Value == resumeHash {
				vals.Set(f.Name, f.Value)
			} else if f.Checked {
				vals.Set(f.Name, f.Value)
			}
		case f.Type == "checkbox":
			if f.Checked {
				vals.Set(f.Name, f.Value)
			}
		case f.Type == "hidden" || f.Type == "text" || f.Type == "":
			vals.Set(f.Name, f.Value)
		}
	}
	if xsrf != "" {
		vals.Set("_xsrf", xsrf)
	}
	if letter != "" {
		if _, ok := vals["letter"]; !ok {
			vals.Set("letter", letter)
		}
	}
	return vals
}

func parseResponseForm(body string) (action string, fields []formField) {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return "", nil
	}
	var form *html.Node
	var findForm func(*html.Node)
	findForm = func(n *html.Node) {
		if form != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "form" {
			if strings.Contains(attr(n, "action"), "vacancy_response") || hasHidden(n, "vacancy") {
				form = n
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findForm(c)
		}
	}
	findForm(doc)
	if form == nil {
		return "", nil
	}
	action = attr(form, "action")

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "input":
				t := attr(n, "type")
				if t == "" {
					t = "text"
				}
				fields = append(fields, formField{
					Name: attr(n, "name"), Value: attr(n, "value"),
					Type: t, Checked: hasAttr(n, "checked"),
				})
			case "textarea":
				fields = append(fields, formField{
					Name: attr(n, "name"), Value: strings.TrimSpace(textContent(n)),
					IsTextarea: true, Question: nearbyQuestion(n),
				})
			case "select":
				fields = append(fields, formField{Name: attr(n, "name"), Value: selectedValue(n), Type: "select"})
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(form)
	return action, fields
}

func hasHidden(n *html.Node, substr string) bool {
	found := false
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if found {
			return
		}
		if x.Type == html.ElementNode && x.Data == "input" && attr(x, "type") == "hidden" {
			if strings.Contains(attr(x, "name"), substr) {
				found = true
			}
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return found
}

func selectedValue(sel *html.Node) string {
	for c := sel.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == "option" && hasAttr(c, "selected") {
			return attr(c, "value")
		}
	}
	return ""
}

func nearbyQuestion(n *html.Node) string {
	for p := n.Parent; p != nil && p.Type == html.ElementNode; p = p.Parent {
		if t := strings.TrimSpace(textContent(p)); t != "" && len(t) < 400 {
			return t
		}
		if p.Data == "form" || p.Data == "body" {
			break
		}
	}
	return ""
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}
