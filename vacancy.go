package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

type jobPosting struct {
	Type               string `json:"@type"`
	Title              string `json:"title"`
	Description        string `json:"description"`
	DatePosted         string `json:"datePosted"`
	ValidThrough       string `json:"validThrough"`
	HiringOrganization struct {
		Name string `json:"name"`
	} `json:"hiringOrganization"`
	JobLocation struct {
		Address struct {
			AddressLocality string `json:"addressLocality"`
		} `json:"address"`
	} `json:"jobLocation"`
}

var ldJSONRe = regexp.MustCompile(`(?s)<script[^>]*application/ld\+json[^>]*>(.*?)</script>`)

func fetchVacancy(client *http.Client, base RSSItem) (Vacancy, error) {
	req, err := http.NewRequest(http.MethodGet, base.URL, nil)
	if err != nil {
		return Vacancy{}, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9,en;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return Vacancy{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Vacancy{}, fmt.Errorf("vacancy status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Vacancy{}, err
	}

	v := Vacancy{
		ID:          base.ID,
		Title:       base.Title,
		Company:     base.Company,
		URL:         base.URL,
		Region:      base.Region,
		Salary:      base.Salary,
		FoundAt:     time.Now(),
		PublishedAt: base.PublishedAt,
	}

	for _, m := range ldJSONRe.FindAllStringSubmatch(string(body), -1) {
		var jp jobPosting
		if err := json.Unmarshal([]byte(m[1]), &jp); err != nil {
			continue
		}
		if !strings.EqualFold(jp.Type, "JobPosting") {
			continue
		}
		if jp.Title != "" {
			v.Title = jp.Title
		}
		if jp.HiringOrganization.Name != "" {
			v.Company = jp.HiringOrganization.Name
		}
		if jp.JobLocation.Address.AddressLocality != "" {
			v.Region = jp.JobLocation.Address.AddressLocality
		}
		v.Description = htmlToText(jp.Description)
		if t, err := time.Parse(time.RFC3339, jp.DatePosted); err == nil {
			v.PublishedAt = t
		}
		break
	}
	if v.Description == "" {
		v.Description = extractQADescription(string(body))
	}
	return v, nil
}

var (
	tagRe    = regexp.MustCompile(`(?s)<[^>]+>`)
	spaceRe  = regexp.MustCompile(`[ \t\x{00a0}]+`)
	nlRe     = regexp.MustCompile(`\n{3,}`)
	qaDescRe = regexp.MustCompile(`(?s)data-qa="vacancy-description"[^>]*>(.*?)</div>\s*</div>`)
)

func htmlToText(s string) string {
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "<br />", "\n")
	s = strings.ReplaceAll(s, "</p>", "\n\n")
	s = strings.ReplaceAll(s, "</li>", "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r", "")
	s = spaceRe.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	s = nlRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func extractQADescription(page string) string {
	m := qaDescRe.FindStringSubmatch(page)
	if m == nil {
		return ""
	}
	return htmlToText(m[1])
}
