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
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Company     string    `json:"company"`
	URL         string    `json:"url"`
	Region      string    `json:"region"`
	Salary      string    `json:"salary"`
	Description string    `json:"description"`
	CoverLetter string    `json:"cover_letter"`
	FoundAt     time.Time `json:"found_at"`
	PublishedAt time.Time `json:"published_at"`
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
CREATE INDEX IF NOT EXISTS idx_vacancies_found_at ON vacancies(found_at);
CREATE TABLE IF NOT EXISTS applications (
	vacancy_id  TEXT PRIMARY KEY,
	resume_hash TEXT,
	status      TEXT,
	letter      TEXT,
	error       TEXT,
	applied_at  TEXT
);
CREATE INDEX IF NOT EXISTS idx_applications_applied_at ON applications(applied_at);`
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

func (s *Store) CountWithLetters() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(1) FROM vacancies WHERE cover_letter <> ''`).Scan(&n)
	return n, err
}

func (s *Store) Get(id string) (Vacancy, error) {
	var v Vacancy
	var foundAt, publishedAt string
	err := s.db.QueryRow(
		`SELECT id, title, company, url, region, salary, description, cover_letter, found_at, published_at
		 FROM vacancies WHERE id = ?`, id).
		Scan(&v.ID, &v.Title, &v.Company, &v.URL, &v.Region, &v.Salary,
			&v.Description, &v.CoverLetter, &foundAt, &publishedAt)
	if err != nil {
		return v, err
	}
	v.FoundAt, _ = time.Parse(time.RFC3339, foundAt)
	v.PublishedAt, _ = time.Parse(time.RFC3339, publishedAt)
	return v, nil
}

func (s *Store) RecordApplication(vacancyID, resumeHash, status, letter, errMsg string) error {
	_, err := s.db.Exec(
		`INSERT INTO applications (vacancy_id, resume_hash, status, letter, error, applied_at)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(vacancy_id) DO UPDATE SET
		   resume_hash=excluded.resume_hash, status=excluded.status,
		   letter=excluded.letter, error=excluded.error, applied_at=excluded.applied_at`,
		vacancyID, resumeHash, status, letter, errMsg, time.Now().Format(time.RFC3339),
	)
	return err
}

func (s *Store) HasApplied(vacancyID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(1) FROM applications WHERE vacancy_id = ?`, vacancyID).Scan(&n)
	return n > 0, err
}

func (s *Store) AppliedSince(t time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(1) FROM applications WHERE applied_at >= ?`,
		t.Format(time.RFC3339)).Scan(&n)
	return n, err
}

func (s *Store) LastAppliedAt() (time.Time, error) {
	var raw string
	err := s.db.QueryRow(`SELECT applied_at FROM applications ORDER BY applied_at DESC LIMIT 1`).Scan(&raw)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, raw)
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
