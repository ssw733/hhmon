package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type rssDoc struct {
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
}

var vacancyIDRe = regexp.MustCompile(`/vacancy/(\d+)`)

// RSSItem is the normalized view used by the monitor.
type RSSItem struct {
	ID          string
	Title       string
	URL         string
	Company     string
	Region      string
	Salary      string
	PublishedAt time.Time
}

func rssURL(query string, areas []int, periodDays int) string {
	q := url.Values{}
	q.Set("text", query)
	for _, a := range areas {
		q.Add("area", fmt.Sprintf("%d", a))
	}
	q.Set("order_by", "publication_time")
	if periodDays > 0 {
		q.Set("search_period", fmt.Sprintf("%d", periodDays))
	}
	q.Set("items_on_page", "50")
	return "https://hh.ru/search/vacancy/rss?" + q.Encode()
}

func fetchRSS(client *http.Client, query string, areas []int, periodDays int) ([]RSSItem, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 1500 * time.Millisecond)
		}
		items, err := fetchRSSOnce(client, query, areas, periodDays)
		if err == nil {
			return items, nil
		}
		lastErr = err
		if isPermanentRSSError(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

type rssStatusError struct{ code int }

func (e rssStatusError) Error() string { return fmt.Sprintf("rss status %d", e.code) }

func isPermanentRSSError(err error) bool {
	var se rssStatusError
	if errors.As(err, &se) {
		return se.code != http.StatusTooManyRequests && se.code < 500
	}
	return false
}

func fetchRSSOnce(client *http.Client, query string, areas []int, periodDays int) ([]RSSItem, error) {
	req, err := http.NewRequest(http.MethodGet, rssURL(query, areas, periodDays), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml, */*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, rssStatusError{code: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	var doc rssDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse rss: %w", err)
	}

	items := make([]RSSItem, 0, len(doc.Channel.Items))
	for _, it := range doc.Channel.Items {
		m := vacancyIDRe.FindStringSubmatch(it.Link)
		if m == nil {
			continue
		}
		item := RSSItem{
			ID:      m[1],
			Title:   strings.TrimSpace(it.Title),
			URL:     it.Link,
			Company: extractField(it.Description, "Вакансия компании:"),
			Region:  extractField(it.Description, "Регион:"),
			Salary:  extractField(it.Description, "Предполагаемый уровень месячного дохода:"),
		}
		if t, err := time.Parse(time.RFC3339, it.PubDate); err == nil {
			item.PublishedAt = t
		}
		items = append(items, item)
	}
	return items, nil
}

var fieldRe = map[string]*regexp.Regexp{}

func extractField(htmlDesc, label string) string {
	re, ok := fieldRe[label]
	if !ok {
		re = regexp.MustCompile(regexp.QuoteMeta(label) + `\s*([^<]*)`)
		fieldRe[label] = re
	}
	m := re.FindStringSubmatch(htmlDesc)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(m[1]))
}
