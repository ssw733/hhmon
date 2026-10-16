package main

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Vacancy struct {
	ID          string
	Title       string
	Company     string
	URL         string
	Region      string
	Salary      string
	Description string
	CoverLetter string
	FoundAt     time.Time
	PublishedAt time.Time
}

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	schema := `
CREATE TABLE IF NOT EXISTS vacancies (
	id           TEXT PRIMARY KEY,
	title        TEXT,
	company      TEXT,
	url          TEXT,
	region       TEXT,
	salary       TEXT,
	description  TEXT,
	cover_letter TEXT,
	found_at     TEXT,
	published_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_vacancies_found_at ON vacancies(found_at);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Seen(id string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(1) FROM vacancies WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

func (s *Store) Save(v Vacancy) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO vacancies
		 (id, title, company, url, region, salary, description, cover_letter, found_at, published_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		v.ID, v.Title, v.Company, v.URL, v.Region, v.Salary, v.Description,
		v.CoverLetter, v.FoundAt.Format(time.RFC3339), v.PublishedAt.Format(time.RFC3339),
	)
	return err
}

func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(1) FROM vacancies`).Scan(&n)
	return n, err
}

func (s *Store) Recent(limit int) ([]Vacancy, error) {
	rows, err := s.db.Query(
		`SELECT id, title, company, url, region, salary, description, cover_letter, found_at, published_at
		 FROM vacancies ORDER BY found_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Vacancy
	for rows.Next() {
		var v Vacancy
		var foundAt, publishedAt string
		if err := rows.Scan(&v.ID, &v.Title, &v.Company, &v.URL, &v.Region, &v.Salary,
			&v.Description, &v.CoverLetter, &foundAt, &publishedAt); err != nil {
			return nil, err
		}
		v.FoundAt, _ = time.Parse(time.RFC3339, foundAt)
		v.PublishedAt, _ = time.Parse(time.RFC3339, publishedAt)
		out = append(out, v)
	}
	return out, rows.Err()
}
