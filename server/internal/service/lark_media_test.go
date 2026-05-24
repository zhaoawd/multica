package service

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// Pure parser tests for the §14.1.3 message → attachment metadata
// translation. These verify the closed shape of LarkMessageAttachment
// independent of any HTTP plumbing — the network paths are exercised
// by the handler-level integration tests.

func TestParseLarkMessageBody_Text(t *testing.T) {
	text, atts := parseLarkMessageBody("text", `{"text":"hello world"}`)
	if text != "hello world" {
		t.Errorf("text body = %q, want 'hello world'", text)
	}
	if atts != nil {
		t.Errorf("text msg should not produce attachments, got %+v", atts)
	}
}

func TestParseLarkMessageBody_Image(t *testing.T) {
	_, atts := parseLarkMessageBody("image", `{"image_key":"img_abc123"}`)
	if len(atts) != 1 {
		t.Fatalf("image msg should produce 1 attachment, got %d", len(atts))
	}
	want := LarkMessageAttachment{
		FileKey:      "img_abc123",
		ResourceType: "image",
	}
	if !reflect.DeepEqual(atts[0], want) {
		t.Errorf("image attachment = %+v, want %+v", atts[0], want)
	}
}

func TestParseLarkMessageBody_File(t *testing.T) {
	content := `{"file_key":"file_xyz","file_name":"design.pdf","file_size":12345,"mime_type":"application/pdf"}`
	_, atts := parseLarkMessageBody("file", content)
	if len(atts) != 1 {
		t.Fatalf("file msg should produce 1 attachment, got %d", len(atts))
	}
	want := LarkMessageAttachment{
		FileKey:      "file_xyz",
		ResourceType: "file",
		Filename:     "design.pdf",
		MimeType:     "application/pdf",
		SizeHint:     12345,
	}
	if !reflect.DeepEqual(atts[0], want) {
		t.Errorf("file attachment = %+v, want %+v", atts[0], want)
	}
}

func TestParseLarkMessageBody_UnknownMsgTypeIsSilent(t *testing.T) {
	// Per §6.5 design ("avoid misfires"), unknown msg_types must not
	// produce attachment refs the downloader would later choke on.
	cases := []struct{ typ, content string }{
		{"sticker", `{"sticker_key":"s_1"}`},
		{"post", `{"title":"...","content":[]}`},
		{"audio", `{"file_key":"f_audio"}`},
		{"", ""},
	}
	for _, c := range cases {
		text, atts := parseLarkMessageBody(c.typ, c.content)
		if text != "" {
			t.Errorf("type=%q text = %q, want empty", c.typ, text)
		}
		if atts != nil {
			t.Errorf("type=%q atts = %+v, want nil", c.typ, atts)
		}
	}
}

func TestParseLarkMessageBody_MalformedJSONDoesNotPanic(t *testing.T) {
	// Malformed content must degrade to "empty body" rather than
	// crash the message-listing path — a single bad message in a
	// thread shouldn't sink the entire @bot 创建任务 flow.
	cases := []struct{ typ, content string }{
		{"text", `{not json`},
		{"image", `{"image_key":}`},
		{"file", `null`},
	}
	for _, c := range cases {
		text, atts := parseLarkMessageBody(c.typ, c.content)
		if text != "" || atts != nil {
			t.Errorf("type=%q malformed should be silent, got text=%q atts=%v", c.typ, text, atts)
		}
	}
}

func TestPickMime_ContentTypeWinsOverEnvelope(t *testing.T) {
	// The list-endpoint envelope's mime hint is best-effort; the
	// resource download's Content-Type is authoritative because it
	// reflects what we actually received. Cases must prove that.
	tests := []struct {
		envelope, contentType, want string
	}{
		{"", "image/png", "image/png"},
		{"application/pdf", "", "application/pdf"},
		{"application/pdf", "image/png", "image/png"},
		{"", "image/png; charset=utf-8", "image/png"},
		{"", "  image/jpeg  ", "image/jpeg"},
	}
	for _, c := range tests {
		got := pickMime(c.envelope, c.contentType)
		if got != c.want {
			t.Errorf("pickMime(%q,%q) = %q, want %q", c.envelope, c.contentType, got, c.want)
		}
	}
}

func TestLarkMediaAllowedMimes_HasOnlyDesignedTypes(t *testing.T) {
	// Lock the whitelist. Adding a type is a deliberate scope edit
	// per §14.1.3 ("video/audio/long PDF explicitly out") — the
	// test failure makes that edit visible in PR review.
	want := map[string]bool{
		"image/png":       true,
		"image/jpeg":      true,
		"image/gif":       true,
		"image/webp":      true,
		"application/pdf": true,
	}
	if !reflect.DeepEqual(LarkMediaAllowedMimes, want) {
		t.Errorf("LarkMediaAllowedMimes = %+v, want %+v", LarkMediaAllowedMimes, want)
	}
}

func TestFormatByteSize(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KB"},
		{10 * 1024 * 1024, "10.0 MB"},
		{15 * 1024 * 1024, "15.0 MB"},
	}
	for _, c := range cases {
		if got := formatByteSize(c.n); got != c.want {
			t.Errorf("formatByteSize(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestProcessOneAttachment_BudgetExhaustedSkipsZeroSizeHint(t *testing.T) {
	// §14.1.3 P1 #2: image messages have SizeHint == 0, but once
	// the per-issue cap is reached we must NOT download them. Use a
	// stub service whose deps are all nil except the gating logic —
	// the test never reaches storage or HTTP, so nil deps are fine.
	svc := &LarkMediaService{}
	report := &LarkMediaReport{TotalBytes: LarkMediaMaxTotalBytes}
	att := LarkMessageAttachment{
		FileKey:      "img_after_cap",
		ResourceType: "image",
		// MimeType + SizeHint left zero on purpose — exactly the
		// envelope shape for Lark image messages that previously
		// bypassed the budget gate.
	}
	r := svc.processOneAttachment(
		t.Context(),
		pgtype.UUID{},
		"member",
		pgtype.UUID{},
		pgtype.UUID{},
		"chat",
		"msg",
		att,
		report,
	)
	if r.Status != "limit_exhausted" {
		t.Fatalf("expected limit_exhausted, got %q", r.Status)
	}
	if r.Persisted {
		t.Errorf("budget-exhausted attachment must not persist")
	}
	if r.Notice == "" {
		t.Errorf("expected inline notice, got empty")
	}
}

func TestProcessOneAttachment_EnvelopeMimeRejectedPreDownload(t *testing.T) {
	// §14.1.3 P1 #2: file messages whose envelope mime is outside the
	// whitelist must be rejected BEFORE we hit the resource endpoint.
	// Same nil-deps trick — the early-return fires before any I/O.
	svc := &LarkMediaService{}
	att := LarkMessageAttachment{
		FileKey:      "file_disallowed",
		ResourceType: "file",
		Filename:     "movie.mp4",
		MimeType:     "video/mp4",
		SizeHint:     1024,
	}
	r := svc.processOneAttachment(
		t.Context(),
		pgtype.UUID{},
		"member",
		pgtype.UUID{},
		pgtype.UUID{},
		"chat",
		"msg",
		att,
		&LarkMediaReport{},
	)
	if r.Status != "unsupported" {
		t.Fatalf("expected unsupported, got %q", r.Status)
	}
	if !strings.Contains(r.Notice, "video/mp4") {
		t.Errorf("expected notice mentioning mime type, got %q", r.Notice)
	}
}

func TestIsLarkPermissionCode(t *testing.T) {
	// Known codes we treat as permission failures. Anything outside
	// this list is treated as "other" — adding a new code is an
	// explicit, reviewable edit.
	if !isLarkPermissionCode(99991663) {
		t.Error("99991663 (im:resource scope missing) should be a perm code")
	}
	for _, c := range []int{0, 1, 9999, 200} {
		if isLarkPermissionCode(c) {
			t.Errorf("%d should NOT be classified as a perm code", c)
		}
	}
}
