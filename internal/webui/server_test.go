package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacrawl/internal/store"
)

const (
	testHost  = "127.0.0.1:43210"
	testToken = "test-private-token"
)

func TestHandlerServesAuthenticatedArchiveViews(t *testing.T) {
	handler := testHandler(t)

	root := request(t, handler, "/", "")
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), "wacrawl archive") {
		t.Fatalf("root status=%d body=%q", root.Code, root.Body.String())
	}
	if !strings.Contains(root.Body.String(), `rel="icon" href="/favicon.svg"`) {
		t.Fatalf("root missing favicon link: %q", root.Body.String())
	}
	if got := root.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") || !strings.Contains(got, "frame-ancestors 'none'") {
		t.Fatalf("content security policy = %q", got)
	}
	if got := root.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache control = %q", got)
	}
	for _, path := range []string{"/favicon.svg", "/favicon.ico"} {
		favicon := request(t, handler, path, "")
		if favicon.Code != http.StatusOK || favicon.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(favicon.Body.String(), "<svg") {
			t.Fatalf("%s status=%d content-type=%q body=%q", path, favicon.Code, favicon.Header().Get("Content-Type"), favicon.Body.String())
		}
	}

	unauthorized := request(t, handler, "/api/status", "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	status := request(t, handler, "/api/status", testToken)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"messages":3`) || strings.Contains(status.Body.String(), "db_path") {
		t.Fatalf("status code=%d body=%s", status.Code, status.Body.String())
	}

	chats := request(t, handler, "/api/chats?limit=10", testToken)
	if chats.Code != http.StatusOK || !strings.Contains(chats.Body.String(), `"name":"Launch Group"`) {
		t.Fatalf("chats code=%d body=%s", chats.Code, chats.Body.String())
	}

	messages := request(t, handler, "/api/messages?chat=123%40g.us&limit=10", testToken)
	if messages.Code != http.StatusOK || !strings.Contains(messages.Body.String(), `"text":"launch now"`) {
		t.Fatalf("messages code=%d body=%s", messages.Code, messages.Body.String())
	}
	if strings.Contains(messages.Body.String(), "media_path") || strings.Contains(messages.Body.String(), "private.jpg") || strings.Contains(messages.Body.String(), "media_url") {
		t.Fatalf("messages exposed private media location: %s", messages.Body.String())
	}

	search := request(t, handler, "/api/search?q=launch&limit=10", testToken)
	if search.Code != http.StatusOK || !strings.Contains(search.Body.String(), `"message_id":"m1"`) {
		t.Fatalf("search code=%d body=%s", search.Code, search.Body.String())
	}
	var results []map[string]any
	if err := json.Unmarshal(search.Body.Bytes(), &results); err != nil || len(results) == 0 {
		t.Fatalf("search json results=%#v err=%v", results, err)
	}
	snippet, _ := results[0]["snippet"].(string)
	if !strings.Contains(snippet, snippetStartMarker+"launch"+snippetEndMarker) {
		t.Fatalf("search snippet markers missing: %q", snippet)
	}
}

func TestHandlerMessagesBeforePagination(t *testing.T) {
	handler := testHandler(t)
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	cutoff := strconv.FormatInt(base.Add(-30*time.Second).Unix(), 10)
	page := request(t, handler, "/api/messages?chat=123%40g.us&limit=10&before="+cutoff, testToken)
	if page.Code != http.StatusOK {
		t.Fatalf("before page status=%d body=%s", page.Code, page.Body.String())
	}
	var messages []map[string]any
	if err := json.Unmarshal(page.Body.Bytes(), &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0]["message_id"] != "m1" || messages[1]["message_id"] != "m3" {
		t.Fatalf("before page messages=%#v", messages)
	}
	if messages[0]["source_pk"] != float64(1) {
		t.Fatalf("before page source_pk=%#v", messages[0]["source_pk"])
	}

	// A composite cursor advances within the shared second: m1 and m3 have the
	// same timestamp, so paging from m3 must return only m1.
	sameSecond := strconv.FormatInt(base.Add(-time.Minute).Unix(), 10)
	page = request(t, handler, "/api/messages?chat=123%40g.us&limit=10&before="+sameSecond+"&before_pk=3", testToken)
	if page.Code != http.StatusOK {
		t.Fatalf("before_pk page status=%d body=%s", page.Code, page.Body.String())
	}
	messages = nil
	if err := json.Unmarshal(page.Body.Bytes(), &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0]["message_id"] != "m1" {
		t.Fatalf("before_pk page messages=%#v", messages)
	}

	for _, invalid := range []string{"abc", "-5", "0"} {
		response := request(t, handler, "/api/messages?chat=123%40g.us&before="+invalid, testToken)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("before=%q status=%d", invalid, response.Code)
		}
	}
	response := request(t, handler, "/api/messages?chat=123%40g.us&before="+cutoff+"&before_pk=0", testToken)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("before_pk=0 status=%d", response.Code)
	}
	response = request(t, handler, "/api/messages?chat=123%40g.us&before_pk=3", testToken)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("before_pk without before status=%d", response.Code)
	}
}

func TestHandlerServesInlineMedia(t *testing.T) {
	ctx := context.Background()
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })

	dir := filepath.Join(filepath.Dir(archive.Path()), "media")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(dir, "photo.png")
	imageBytes := writeTestPNG(t, imagePath)
	textPath := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(textPath, []byte("plain text, definitely not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	sourceImagePath := filepath.Join(sourceRoot, "source-photo.png")
	sourceImageBytes := writeTestPNG(t, sourceImagePath)

	const jid = "555@s.whatsapp.net"
	now := time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)
	err = archive.ReplaceAll(ctx, store.ImportStats{FinishedAt: now}, nil, []store.Chat{
		{JID: jid, Kind: "dm", Name: "Media Tester", LastMessageAt: now},
	}, nil, nil, []store.Message{
		{SourcePK: 1, ChatJID: jid, MessageID: "p1", Timestamp: now, MediaType: "image", MediaPath: imagePath},
		{SourcePK: 2, ChatJID: jid, MessageID: "p2", Timestamp: now, Text: "no media"},
		{SourcePK: 3, ChatJID: jid, MessageID: "p3", Timestamp: now, MediaType: "image", MediaPath: textPath},
		{SourcePK: 4, ChatJID: jid, MessageID: "p4", Timestamp: now, MediaType: "image", MediaPath: filepath.Join(dir, "missing.jpg")},
		{SourcePK: 5, ChatJID: jid, MessageID: "p5", Timestamp: now, MediaType: "image", MediaPath: sourceImagePath},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(archive, testToken, testHost, sourceRoot)
	t.Cleanup(handler.close)

	unauthorized := request(t, handler, "/api/media?pk=1", "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized media status = %d", unauthorized.Code)
	}

	image := request(t, handler, "/api/media?pk=1", testToken)
	if image.Code != http.StatusOK || image.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("media status=%d content-type=%q", image.Code, image.Header().Get("Content-Type"))
	}
	if !bytes.Equal(image.Body.Bytes(), imageBytes) {
		t.Fatalf("media bytes mismatch: got %d want %d", image.Body.Len(), len(imageBytes))
	}
	sourceImage := request(t, handler, "/api/media?pk=5", testToken)
	if sourceImage.Code != http.StatusOK || !bytes.Equal(sourceImage.Body.Bytes(), sourceImageBytes) {
		t.Fatalf("source media status=%d bytes=%d want %d", sourceImage.Code, sourceImage.Body.Len(), len(sourceImageBytes))
	}

	for _, tc := range []struct {
		query string
		code  int
	}{
		{"pk=2", http.StatusNotFound},             // message without media
		{"pk=3", http.StatusUnsupportedMediaType}, // bytes are not an image
		{"pk=4", http.StatusNotFound},             // file missing on disk
		{"pk=99", http.StatusNotFound},            // unknown message
		{"pk=abc", http.StatusBadRequest},
		{"pk=0", http.StatusBadRequest},
		{"pk=", http.StatusBadRequest},
	} {
		response := request(t, handler, "/api/media?"+tc.query, testToken)
		if response.Code != tc.code {
			t.Fatalf("media %s status=%d want %d", tc.query, response.Code, tc.code)
		}
	}
}

func TestHandlerRejectsMediaOutsideAllowedRoots(t *testing.T) {
	ctx := context.Background()
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })

	mediaRoot := filepath.Join(filepath.Dir(archive.Path()), "media")
	if err := os.MkdirAll(mediaRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(filepath.Dir(mediaRoot), "secret.png")
	writeTestPNG(t, secretPath)
	outsidePath := filepath.Join(t.TempDir(), "outside.png")
	writeTestPNG(t, outsidePath)
	const jid = "555@s.whatsapp.net"
	now := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	messages := []store.Message{
		{SourcePK: 1, ChatJID: jid, MessageID: "outside", Timestamp: now, MediaType: "image", MediaPath: outsidePath},
		{SourcePK: 2, ChatJID: jid, MessageID: "traversal", Timestamp: now, MediaType: "image", MediaPath: mediaRoot + string(os.PathSeparator) + ".." + string(os.PathSeparator) + filepath.Base(secretPath)},
		{SourcePK: 4, ChatJID: jid, MessageID: "relative", Timestamp: now, MediaType: "image", MediaPath: "media/photo.png"},
	}
	sourcePKs := []int{1, 2, 4}
	symlinkPath := filepath.Join(mediaRoot, "linked.png")
	if err := os.Symlink(outsidePath, symlinkPath); err != nil {
		t.Logf("symlink escape case unavailable: %v", err)
	} else {
		messages = append(messages, store.Message{SourcePK: 3, ChatJID: jid, MessageID: "symlink", Timestamp: now, MediaType: "image", MediaPath: symlinkPath})
		sourcePKs = append(sourcePKs, 3)
	}
	if err := archive.ReplaceAll(ctx, store.ImportStats{FinishedAt: now}, nil, []store.Chat{
		{JID: jid, Kind: "dm", Name: "Media Tester", LastMessageAt: now},
	}, nil, nil, messages); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(archive, testToken, testHost)
	t.Cleanup(handler.close)
	for _, sourcePK := range sourcePKs {
		response := request(t, handler, "/api/media?pk="+strconv.Itoa(sourcePK), testToken)
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "media unavailable") {
			t.Fatalf("media pk=%d status=%d body=%q", sourcePK, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "outside") {
			t.Fatalf("media pk=%d exposed path in body %q", sourcePK, response.Body.String())
		}
	}
}

func writeTestPNG(t *testing.T, path string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, color.RGBA{R: uint8(60 * x), G: 168, B: 132, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestHandlerRejectsHostMethodsAndInvalidQueries(t *testing.T) {
	handler := testHandler(t)

	req := httptest.NewRequest(http.MethodGet, "http://malicious.invalid/api/status", nil)
	req.Host = "malicious.invalid"
	req.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("wrong host status = %d", response.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://"+testHost+"/api/status", nil)
	req.Host = testHost
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("post status=%d allow=%q", response.Code, response.Header().Get("Allow"))
	}

	invalidLimit := request(t, handler, "/api/chats?limit=501", testToken)
	if invalidLimit.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d", invalidLimit.Code)
	}

	emptySearch := request(t, handler, "/api/search?q=", testToken)
	if emptySearch.Code != http.StatusBadRequest {
		t.Fatalf("empty search status = %d", emptySearch.Code)
	}

	notFound := request(t, handler, "/api/missing", testToken)
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("api missing status = %d", notFound.Code)
	}
	notFound = request(t, handler, "/missing", "")
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("static missing status = %d", notFound.Code)
	}

	wrongToken := request(t, handler, "/api/status", "test-private-tokem")
	if wrongToken.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", wrongToken.Code)
	}
}

func TestServeLifecycleAndPrivateURL(t *testing.T) {
	archive := testArchive(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &captureWriter{written: make(chan string, 1)}
	result := make(chan error, 1)
	go func() {
		result <- Serve(ctx, archive, Config{Output: output})
	}()

	var printed string
	select {
	case printed = <-output.written:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not print its private URL")
	}
	var privateURL string
	for _, field := range strings.Fields(printed) {
		if strings.HasPrefix(field, "http://") {
			privateURL = field
			break
		}
	}
	parsed, err := url.Parse(privateURL)
	if err != nil || parsed.Host == "" || parsed.Fragment == "" {
		t.Fatalf("private URL = %q parsed=%#v err=%v", privateURL, parsed, err)
	}
	if parsed.Hostname() != "127.0.0.1" {
		t.Fatalf("viewer host = %q", parsed.Hostname())
	}

	rootURL := *parsed
	rootURL.Fragment = ""
	response, err := http.Get(rootURL.String()) // #nosec G107 -- test URL is a fresh loopback listener.
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("root status = %d", response.StatusCode)
	}

	apiURL := rootURL
	apiURL.Path = "/api/status"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, apiURL.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+parsed.Fragment)
	response, err = http.DefaultClient.Do(req) // #nosec G704 -- test URL is derived from Serve's fresh loopback listener.
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("api status = %d", response.StatusCode)
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop after context cancellation")
	}
}

func TestServeRejectsInvalidOrOccupiedPorts(t *testing.T) {
	if err := Serve(context.Background(), nil, Config{Port: -1}); err == nil || !strings.Contains(err.Error(), "between 0 and 65535") {
		t.Fatalf("invalid port error = %v", err)
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port
	err = Serve(context.Background(), nil, Config{Port: port})
	if err == nil || !strings.Contains(err.Error(), "listen for web viewer") {
		t.Fatalf("occupied port error = %v", err)
	}
}

func TestHandlerArchiveAndAssetFailures(t *testing.T) {
	archive := testArchive(t)
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(archive, testToken, testHost)
	t.Cleanup(h.close)
	for _, path := range []string{"/api/status", "/api/chats", "/api/messages"} {
		response := request(t, h, path, testToken)
		if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "archive unavailable") {
			t.Fatalf("%s status=%d body=%q", path, response.Code, response.Body.String())
		}
	}

	response := httptest.NewRecorder()
	(&handler{allowedHost: testHost}).serveAsset(response, "static/missing", "text/plain")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("missing asset status = %d", response.Code)
	}

	failing := &failingResponseWriter{header: make(http.Header)}
	writeJSON(failing, map[string]string{"ok": "yes"})
	if failing.writes < 2 {
		t.Fatalf("writeJSON failure writes = %d", failing.writes)
	}
}

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	h := NewHandler(testArchive(t), testToken, testHost)
	t.Cleanup(h.close)
	return h
}

func testArchive(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	err = archive.ReplaceAll(ctx, store.ImportStats{FinishedAt: now}, nil, []store.Chat{
		{JID: "123@g.us", Kind: "group", Name: "Launch Group", LastMessageAt: now, UnreadCount: 1},
	}, nil, nil, []store.Message{
		{SourcePK: 1, ChatJID: "123@g.us", ChatName: "Launch Group", MessageID: "m1", SenderName: "Alice", Timestamp: now.Add(-time.Minute), Text: "launch now", MediaType: "image", MediaPath: "/private/media/private.jpg", MediaURL: "https://example.invalid/private.jpg"},
		{SourcePK: 2, ChatJID: "123@g.us", ChatName: "Launch Group", MessageID: "m2", Timestamp: now, FromMe: true, Text: "ship later"},
		{SourcePK: 3, ChatJID: "123@g.us", ChatName: "Launch Group", MessageID: "m3", SenderName: "Alice", Timestamp: now.Add(-time.Minute), Text: "launch encore"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return archive
}

func request(t *testing.T, handler http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+path, nil)
	req.Host = testHost
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

type captureWriter struct {
	written chan string
}

func (w *captureWriter) Write(body []byte) (int, error) {
	select {
	case w.written <- string(body):
	default:
	}
	return len(body), nil
}

type failingResponseWriter struct {
	header http.Header
	writes int
}

func (w *failingResponseWriter) Header() http.Header {
	return w.header
}

func (w *failingResponseWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("write failed")
}

func (w *failingResponseWriter) WriteHeader(int) {}
