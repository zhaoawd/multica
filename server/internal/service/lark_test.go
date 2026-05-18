package service

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestEncryptDecryptBotToken_Roundtrip(t *testing.T) {
	for _, key := range []string{
		"some-random-text",
		"6f5902ac237024bdd0c176cb93063dc4", // hex
		"YWJjZGVmZ2hpamtsbW5vcA==",         // base64
	} {
		t.Run("key_"+key[:4], func(t *testing.T) {
			ct, err := EncryptBotToken(key, "lark-token-xyz")
			if err != nil {
				t.Fatalf("encrypt: %v", err)
			}
			if len(ct) == 0 {
				t.Fatalf("expected non-empty ciphertext")
			}
			pt, err := DecryptBotToken(key, ct)
			if err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			if pt != "lark-token-xyz" {
				t.Fatalf("plaintext mismatch: %q", pt)
			}
		})
	}
}

func TestEncryptBotToken_EmptyInputEmptyOutput(t *testing.T) {
	ct, err := EncryptBotToken("k", "")
	if err != nil {
		t.Fatalf("expected nil err for empty token, got %v", err)
	}
	if ct != nil {
		t.Fatalf("expected nil output for empty plaintext, got %v", ct)
	}
}

func TestDecryptBotToken_EmptyInputEmptyOutput(t *testing.T) {
	pt, err := DecryptBotToken("k", nil)
	if err != nil {
		t.Fatalf("expected nil err for empty blob, got %v", err)
	}
	if pt != "" {
		t.Fatalf("expected empty plaintext, got %q", pt)
	}
}

func TestDecryptBotToken_TamperFails(t *testing.T) {
	ct, err := EncryptBotToken("k", "secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	ct[len(ct)-1] ^= 0x01
	if _, err := DecryptBotToken("k", ct); err == nil {
		t.Fatalf("expected decryption to fail on tamper")
	}
}

func TestLarkConfig_Configured(t *testing.T) {
	cfg := LarkConfig{AppID: "a", AppSecret: "b", VerificationToken: "c", EncryptKey: "d"}
	if !cfg.Configured() {
		t.Fatalf("expected fully-populated config to be Configured")
	}
	cfg.EncryptKey = ""
	if cfg.Configured() {
		t.Fatalf("expected missing key to disable Configured")
	}
}

func TestBuildCard_ShapeStaysStable(t *testing.T) {
	card := buildCard("Header", "Body **markdown**", []cardButton{
		{Text: "View", URL: "https://example.test/x"},
	})
	js := CardJSON(card)
	// Verify the schema landmarks the front-end / Lark renderer rely on
	// so an accidental shape rewrite shows up here, not in production.
	for _, needle := range []string{
		`"config"`,
		`"wide_screen_mode":true`,
		`"header"`,
		`"plain_text"`,
		`"Header"`,
		`"markdown"`,
		`"Body **markdown**"`,
		`"action"`,
		`"button"`,
		`"https://example.test/x"`,
	} {
		if !strings.Contains(js, needle) {
			t.Fatalf("card JSON missing %q: %s", needle, js)
		}
	}
}

func TestBuildCard_NoButtonsOmitsActionElement(t *testing.T) {
	card := buildCard("Header", "Body", nil)
	js := CardJSON(card)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(js), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	elements, _ := parsed["elements"].([]any)
	if len(elements) != 1 {
		t.Fatalf("expected 1 element (markdown only), got %d", len(elements))
	}
}

func TestSliceContains(t *testing.T) {
	if !sliceContains([]string{"a", "b"}, "b") {
		t.Fatalf("expected b found")
	}
	if sliceContains([]string{"a"}, "z") {
		t.Fatalf("expected z not found")
	}
	if sliceContains(nil, "x") {
		t.Fatalf("expected miss on nil slice")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello world", 5); got != "hell…" {
		t.Fatalf("got %q", got)
	}
	if got := truncate("hi", 5); got != "hi" {
		t.Fatalf("got %q", got)
	}
	if got := truncate("hi", 0); got != "hi" {
		t.Fatalf("got %q", got)
	}
}

func TestLarkNotify_StopWithoutStartIsNoop(t *testing.T) {
	n := newTestLarkNotify(LarkConfig{}) // not Configured
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// Should return immediately without blocking on workers that never started.
	done := make(chan struct{})
	go func() { n.Stop(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Stop blocked despite Start never being called")
	}
}

func TestLarkNotify_StopUnblocksIdleWorkersImmediately(t *testing.T) {
	n := newTestLarkNotify(fullCfg())
	n.Start()
	defer cleanupNotify(n)

	// Workers are idle on the select (jobs empty). Stop should wake them
	// via stopCh close, not wait on the wide ctx deadline.
	deadline := time.Now().Add(2 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	start := time.Now()
	n.Stop(ctx)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Stop took %v, expected idle workers to exit promptly", elapsed)
	}
}

func TestLarkNotify_DispatchDropsAfterStop(t *testing.T) {
	n := newTestLarkNotify(fullCfg())
	n.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	n.Stop(ctx)
	cancel()

	// After Stop, dispatch must NOT block, send on a stale channel, or
	// panic. We don't have a binding row here, so we just rely on the
	// stopping-flag short-circuit — but the relevant assertion is "this
	// call returns".
	done := make(chan struct{})
	go func() {
		n.dispatch(context.Background(), "00000000-0000-0000-0000-000000000000", "issue:created", func() any { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("dispatch did not return after Stop")
	}
}

// ── Test helpers ─────────────────────────────────────────────────────────

func newTestLarkNotify(cfg LarkConfig) *LarkNotify {
	return &LarkNotify{
		cfg:         cfg,
		client:      NewLarkClient(cfg),
		queries:     nil,
		frontend:    "http://localhost:3000",
		log:         testLogger(),
		jobs:        make(chan larkJob, 8),
		stopCh:      make(chan struct{}),
		sendTimeout: 1 * time.Second,
	}
}

func fullCfg() LarkConfig {
	return LarkConfig{AppID: "a", AppSecret: "b", VerificationToken: "c", EncryptKey: "d"}
}

func cleanupNotify(n *LarkNotify) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	n.Stop(ctx)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
