package main

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
)

type fakeGen struct {
	name  string
	calls *int
	fail  bool
	out   string
}

func (f *fakeGen) Generate(ctx context.Context, system, user string) (string, error) {
	*f.calls++
	if f.fail {
		return "", errors.New("limit exceeded")
	}
	return f.out, nil
}

func newTestChain(entries ...chainEntry) *chainGen {
	return &chainGen{entries: entries, log: log.New(io.Discard, "", 0)}
}

func TestChainFailoverAndSticky(t *testing.T) {
	var a, b int
	c := newTestChain(
		chainEntry{name: "a", gen: &fakeGen{name: "a", calls: &a, fail: true}},
		chainEntry{name: "b", gen: &fakeGen{name: "b", calls: &b, out: "ok-b"}},
	)

	for i := 0; i < 2; i++ {
		out, err := c.Generate(context.Background(), "s", "u")
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if out != "ok-b" {
			t.Fatalf("call %d: got %q, want ok-b", i, out)
		}
	}

	if a != 1 {
		t.Fatalf("provider a should be tried only once (sticky), got %d", a)
	}
	if b != 2 {
		t.Fatalf("provider b should be used twice, got %d", b)
	}
	if c.cur != 1 {
		t.Fatalf("current provider index = %d, want 1", c.cur)
	}
}

func TestChainAllFail(t *testing.T) {
	var a, b int
	c := newTestChain(
		chainEntry{name: "a", gen: &fakeGen{name: "a", calls: &a, fail: true}},
		chainEntry{name: "b", gen: &fakeGen{name: "b", calls: &b, fail: true}},
	)
	if _, err := c.Generate(context.Background(), "s", "u"); err == nil {
		t.Fatal("expected error when all providers fail")
	}
}
