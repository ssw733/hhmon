package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const sampleResponseForm = `<!doctype html><html><body>
<form action="/applicant/vacancy_response" method="post">
  <input type="hidden" name="_xsrf" value="abc123">
  <input type="hidden" name="vacancy_id" value="999">
  <label>Резюме</label>
  <input type="radio" name="resume_hash" value="1111111111111111111111111111111111111111" checked>
  <input type="radio" name="resume_hash" value="2222222222222222222222222222222222222222">
  <label>Сопроводительное письмо</label>
  <textarea name="letter"></textarea>
  <label>Ваши ожидания по зарплате?</label>
  <textarea name="question_1"></textarea>
</form>
</body></html>`

func TestParseResponseForm(t *testing.T) {
	action, fields := parseResponseForm(sampleResponseForm)
	if action != "/applicant/vacancy_response" {
		t.Fatalf("action = %q", action)
	}
	if len(fields) < 5 {
		t.Fatalf("ожидалось >=5 полей, получено %d", len(fields))
	}
	qs := questionFields(fields)
	if len(qs) != 1 || qs[0].Name != "question_1" {
		t.Fatalf("questionFields = %+v, want [question_1]", qs)
	}
}

func TestBuildFormValues(t *testing.T) {
	_, fields := parseResponseForm(sampleResponseForm)
	resumeHash := "2222222222222222222222222222222222222222"
	vals := buildFormValues(fields, resumeHash, "Моё письмо", map[string]string{"question_1": "от 250000"}, "abc123")

	checks := map[string]string{
		"vacancy_id":  "999",
		"resume_hash": resumeHash,
		"letter":      "Моё письмо",
		"question_1":  "от 250000",
		"_xsrf":       "abc123",
	}
	for k, want := range checks {
		if got := vals.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestParseResumes(t *testing.T) {
	body := []byte(`<div>
	  <a href="/resume/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa">Backend-разработчик</a>
	  <a href="/resume/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb?query=1">Fullstack</a>
	</div>`)
	rs := parseResumes(body)
	if len(rs) != 2 {
		t.Fatalf("ожидалось 2 резюме, получено %d", len(rs))
	}
	if rs[0].Hash != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || rs[0].Title != "Backend-разработчик" {
		t.Fatalf("resume[0] = %+v", rs[0])
	}
}

func TestApplierSubmit(t *testing.T) {
	var mu sync.Mutex
	var posted url.Values

	mux := http.NewServeMux()
	mux.HandleFunc("/applicant/resumes", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<a href="/resume/2222222222222222222222222222222222222222">Backend-разработчик</a>`)
	})
	mux.HandleFunc("/applicant/vacancy_response", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
			mu.Lock()
			posted = r.PostForm
			mu.Unlock()
			io.WriteString(w, `{"ok":true}`)
			return
		}
		io.WriteString(w, sampleResponseForm)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(srv.URL)
	jar.SetCookies(u, []*http.Cookie{{Name: "_xsrf", Value: "abc123", Path: "/"}})

	a := &Applier{
		cfg: ApplyConfig{
			Enabled: true, DryRun: false, ResumeTitleContains: "backend",
			QuestionsViaLLM: true, BaseURL: srv.URL,
		},
		client:  &http.Client{Jar: jar},
		log:     log.New(io.Discard, "", 0),
		gen:     &fakeGen{out: "ответ-на-вопрос"},
		profile: "профиль",
	}

	status, err := a.Apply(context.Background(), Vacancy{
		ID: "999", Title: "Go backend", Company: "X", URL: srv.URL + "/vacancy/999",
		CoverLetter: "Моё сопроводительное",
	})
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if status != "submitted" {
		t.Fatalf("status = %q, want submitted", status)
	}

	mu.Lock()
	defer mu.Unlock()
	checks := map[string]string{
		"vacancy_id":  "999",
		"resume_hash": "2222222222222222222222222222222222222222",
		"letter":      "Моё сопроводительное",
		"question_1":  "ответ-на-вопрос",
		"_xsrf":       "abc123",
	}
	for k, want := range checks {
		if got := posted.Get(k); got != want {
			t.Errorf("form[%s] = %q, want %q", k, got, want)
		}
	}
}

func TestApplierDryRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/applicant/resumes", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<a href="/resume/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa">Backend</a>`)
	})
	mux.HandleFunc("/applicant/vacancy_response", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("dry-run не должен делать POST")
			return
		}
		io.WriteString(w, sampleResponseForm)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(srv.URL)
	jar.SetCookies(u, []*http.Cookie{{Name: "_xsrf", Value: "abc123", Path: "/"}})

	a := &Applier{
		cfg:    ApplyConfig{Enabled: true, DryRun: true, ResumeTitleContains: "backend", BaseURL: srv.URL},
		client: &http.Client{Jar: jar},
		log:    log.New(io.Discard, "", 0),
		gen:    &fakeGen{out: "x"},
	}
	status, err := a.Apply(context.Background(), Vacancy{ID: "1", Title: "T", CoverLetter: "L"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(status, "dry_run") {
		t.Fatalf("status = %q, want dry_run", status)
	}
}
