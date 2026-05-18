package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExtractDocURLs_RecognisesAllKinds(t *testing.T) {
	in := strings.Join([]string{
		"plain docx: https://acme.feishu.cn/docx/AbCdEf123_-",
		"markdown link: see [the spec](https://acme.feishu.cn/wiki/WiKi456)",
		"sheets: https://acme.feishu.cn/sheets/Sheet789",
		"base: https://acme.feishu.cn/base/BaseAbc",
		"file: https://acme.feishu.cn/file/FileXyz",
		"larksuite global: https://acme.larksuite.com/docx/GlobalAbc",
		"non-lark: https://github.com/foo/bar should not match",
		"duplicate: https://acme.feishu.cn/docx/AbCdEf123_- again",
	}, "\n")

	got := ExtractDocURLs(in)

	want := []string{
		"https://acme.feishu.cn/docx/AbCdEf123_-",
		"https://acme.feishu.cn/wiki/WiKi456",
		"https://acme.feishu.cn/sheets/Sheet789",
		"https://acme.feishu.cn/base/BaseAbc",
		"https://acme.feishu.cn/file/FileXyz",
		"https://acme.larksuite.com/docx/GlobalAbc",
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d urls, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("url %d: want %q got %q", i, w, got[i])
		}
	}
}

func TestExtractDocURLs_EmptyAndNonMatching(t *testing.T) {
	if got := ExtractDocURLs(""); got != nil {
		t.Fatalf("expected nil for empty input, got %v", got)
	}
	if got := ExtractDocURLs("no urls in this text at all"); got != nil {
		t.Fatalf("expected nil for no-match input, got %v", got)
	}
	// Bare host without the doc-kind path component must not match — we
	// don't want to claim arbitrary feishu URLs as docs.
	if got := ExtractDocURLs("https://www.feishu.cn/why"); got != nil {
		t.Fatalf("expected nil for bare feishu URL, got %v", got)
	}
}

func TestParseDocURL(t *testing.T) {
	cases := []struct {
		url, wantKind, wantToken string
		wantOK                   bool
	}{
		{"https://acme.feishu.cn/docx/AbC123", "docx", "AbC123", true},
		{"https://acme.feishu.cn/wiki/wiki-token_xyz", "wiki", "wiki-token_xyz", true},
		{"https://acme.larksuite.com/sheets/S1", "sheets", "S1", true},
		{"https://github.com/foo/bar", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		gotKind, gotToken, gotOK := parseDocURL(c.url)
		if gotKind != c.wantKind || gotToken != c.wantToken || gotOK != c.wantOK {
			t.Errorf("parseDocURL(%q) = (%q,%q,%v), want (%q,%q,%v)", c.url, gotKind, gotToken, gotOK, c.wantKind, c.wantToken, c.wantOK)
		}
	}
}

func TestLarkDocs_Fetch_UnconfiguredReturnsUnavailable(t *testing.T) {
	d := NewLarkDocs(nil)
	got := d.Fetch(context.Background(), "https://acme.feishu.cn/docx/Abc")
	if got.Error != "unavailable" || got.Content != "" {
		t.Fatalf("expected unavailable, got %+v", got)
	}
	if got.URL != "https://acme.feishu.cn/docx/Abc" {
		t.Fatalf("URL should round-trip even when unconfigured, got %q", got.URL)
	}
}

func TestLarkDocs_Fetch_UnrecognisedURLIsUnavailable(t *testing.T) {
	d := newTestLarkDocs(t, func(http.ResponseWriter, *http.Request) {
		t.Fatalf("HTTP should not be called for an unrecognised URL")
	})
	got := d.Fetch(context.Background(), "https://example.com/not-a-lark-url")
	if got.Error != "unavailable" {
		t.Fatalf("expected unavailable for non-lark URL, got %+v", got)
	}
}

func TestLarkDocs_Fetch_UnsupportedKindIsUnavailable(t *testing.T) {
	// sheets / base / file are recognised but not expanded in P3.A. The
	// fetcher must short-circuit BEFORE hitting Lark — otherwise we'd
	// burn a tenant-token round-trip on a URL we know we can't read.
	d := newTestLarkDocs(t, func(http.ResponseWriter, *http.Request) {
		t.Fatalf("HTTP should not be called for unsupported kinds")
	})
	got := d.Fetch(context.Background(), "https://acme.feishu.cn/sheets/S1")
	if got.Error != "unavailable" {
		t.Fatalf("expected unavailable for sheets, got %+v", got)
	}
}

func TestLarkDocs_Fetch_DocxSuccess(t *testing.T) {
	d := newTestLarkDocs(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/auth/v3/tenant_access_token/internal"):
			writeJSONResp(w, map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
		case strings.Contains(r.URL.Path, "/docx/v1/documents/AbC123/raw_content"):
			if got := r.Header.Get("Authorization"); got != "Bearer tk" {
				t.Errorf("expected Bearer tk, got %q", got)
			}
			writeJSONResp(w, map[string]any{"code": 0, "data": map[string]any{"content": "hello world"}})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(500)
		}
	})

	got := d.Fetch(context.Background(), "https://acme.feishu.cn/docx/AbC123")
	if got.Error != "" {
		t.Fatalf("expected no error, got %q", got.Error)
	}
	if got.Content != "hello world" {
		t.Fatalf("content mismatch: %q", got.Content)
	}
}

func TestLarkDocs_Fetch_DocxForbidden(t *testing.T) {
	d := newTestLarkDocs(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/auth/v3/tenant_access_token") {
			writeJSONResp(w, map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
			return
		}
		// HTTP 200 with a Lark business code for permission denied — common
		// shape from the Open API.
		writeJSONResp(w, map[string]any{"code": 1254100, "msg": "permission denied"})
	})

	got := d.Fetch(context.Background(), "https://acme.feishu.cn/docx/AbC123")
	if got.Error != "forbidden" {
		t.Fatalf("expected forbidden, got %+v", got)
	}
	if got.Content != "" {
		t.Fatalf("content must be empty on error, got %q", got.Content)
	}
}

func TestLarkDocs_Fetch_DocxHTTP403(t *testing.T) {
	d := newTestLarkDocs(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/auth/v3/tenant_access_token") {
			writeJSONResp(w, map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		// Even on a non-2xx, Lark sometimes returns valid JSON — the parser
		// reads body before checking status, so we still emit valid JSON.
		json.NewEncoder(w).Encode(map[string]any{"code": -1, "msg": "blocked"})
	})

	got := d.Fetch(context.Background(), "https://acme.feishu.cn/docx/AbC123")
	if got.Error != "forbidden" {
		t.Fatalf("expected forbidden on HTTP 403, got %+v", got)
	}
}

func TestLarkDocs_Fetch_DocxNotFound(t *testing.T) {
	d := newTestLarkDocs(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/auth/v3/tenant_access_token") {
			writeJSONResp(w, map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
			return
		}
		writeJSONResp(w, map[string]any{"code": 1254404, "msg": "not found"})
	})

	got := d.Fetch(context.Background(), "https://acme.feishu.cn/docx/MissingDoc")
	if got.Error != "not_found" {
		t.Fatalf("expected not_found, got %+v", got)
	}
}

func TestLarkDocs_Fetch_DocxServerError(t *testing.T) {
	d := newTestLarkDocs(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/auth/v3/tenant_access_token") {
			writeJSONResp(w, map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
			return
		}
		w.WriteHeader(500)
		w.Write([]byte("server boom"))
	})

	got := d.Fetch(context.Background(), "https://acme.feishu.cn/docx/AbC123")
	if got.Error != "unavailable" {
		t.Fatalf("expected unavailable on transient 5xx, got %+v", got)
	}
}

func TestLarkDocs_Fetch_WikiResolvesToDocx(t *testing.T) {
	d := newTestLarkDocs(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/auth/v3/tenant_access_token"):
			writeJSONResp(w, map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
		case strings.Contains(r.URL.Path, "/wiki/v2/spaces/get_node"):
			if r.URL.Query().Get("token") != "wikiTok" {
				t.Errorf("missing wiki token in query: %q", r.URL.RawQuery)
			}
			writeJSONResp(w, map[string]any{
				"code": 0,
				"data": map[string]any{
					"node": map[string]any{"obj_token": "docxObj", "obj_type": "docx"},
				},
			})
		case strings.Contains(r.URL.Path, "/docx/v1/documents/docxObj/raw_content"):
			writeJSONResp(w, map[string]any{"code": 0, "data": map[string]any{"content": "wiki body"}})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})

	got := d.Fetch(context.Background(), "https://acme.feishu.cn/wiki/wikiTok")
	if got.Error != "" || got.Content != "wiki body" {
		t.Fatalf("expected wiki body, got %+v", got)
	}
}

func TestLarkDocs_Fetch_WikiNonDocxNodeUnavailable(t *testing.T) {
	// Wiki entries can point at sheets / mindnotes etc. — we surface those
	// as unavailable rather than guessing at a wrong API path.
	d := newTestLarkDocs(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/auth/v3/tenant_access_token") {
			writeJSONResp(w, map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
			return
		}
		writeJSONResp(w, map[string]any{
			"code": 0,
			"data": map[string]any{
				"node": map[string]any{"obj_token": "sheetObj", "obj_type": "sheet"},
			},
		})
	})

	got := d.Fetch(context.Background(), "https://acme.feishu.cn/wiki/wikiTok")
	if got.Error != "unavailable" {
		t.Fatalf("expected unavailable for non-docx wiki node, got %+v", got)
	}
}

func TestFetchErrorCode_MapsAsExpected(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&larkFetchError{httpStatus: 403}, "forbidden"},
		{&larkFetchError{httpStatus: 200, code: 1254100}, "forbidden"},
		{&larkFetchError{httpStatus: 404}, "not_found"},
		{&larkFetchError{httpStatus: 200, code: 1254404}, "not_found"},
		{&larkFetchError{httpStatus: 500}, "unavailable"},
		{&larkFetchError{httpStatus: 200, code: 99999}, "unavailable"},
	}
	for _, c := range cases {
		if got := fetchErrorCode(c.err); got != c.want {
			t.Errorf("fetchErrorCode(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// ── Test helpers ─────────────────────────────────────────────────────────

func newTestLarkDocs(t *testing.T, handler http.HandlerFunc) *LarkDocs {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := LarkConfig{AppID: "a", AppSecret: "b", VerificationToken: "c", EncryptKey: "d"}
	client := &LarkClient{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 2 * time.Second},
		apiBase:    srv.URL,
	}
	return NewLarkDocs(client)
}

func writeJSONResp(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(body)
}
