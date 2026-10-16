package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"
)

// playwrightState is the storageState file produced by `tools/hh-login.mjs`.
type playwrightState struct {
	Cookies []struct {
		Name     string  `json:"name"`
		Value    string  `json:"value"`
		Domain   string  `json:"domain"`
		Path     string  `json:"path"`
		Expires  float64 `json:"expires"`
		HTTPOnly bool    `json:"httpOnly"`
		Secure   bool    `json:"secure"`
	} `json:"cookies"`
}

func loadSession(path string) (*http.Client, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read session %q: %w (сначала выполни: node tools/hh-login.mjs)", path, err)
	}
	var st playwrightState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parse session: %w", err)
	}
	if len(st.Cookies) == 0 {
		return nil, fmt.Errorf("session %q не содержит cookies", path)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, c := range st.Cookies {
		host := strings.TrimPrefix(c.Domain, ".")
		u := &url.URL{Scheme: "https", Host: host}
		cookie := &http.Cookie{
			Name:     c.Name,
			Value:    c.Value,
			Path:     orDefault(c.Path, "/"),
			Domain:   c.Domain,
			Secure:   c.Secure,
			HttpOnly: c.HTTPOnly,
		}
		if c.Expires > 0 {
			cookie.Expires = time.Unix(int64(c.Expires), 0)
			if cookie.Expires.Before(now) {
				continue
			}
		}
		jar.SetCookies(u, []*http.Cookie{cookie})
	}
	return &http.Client{Jar: jar, Timeout: 30 * time.Second}, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (a *Applier) xsrf() string {
	u, err := url.Parse(a.base())
	if err != nil {
		return ""
	}
	for _, c := range a.client.Jar.Cookies(u) {
		if c.Name == "_xsrf" {
			return c.Value
		}
	}
	return ""
}
