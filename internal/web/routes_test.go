package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/session"
)

const routeTestID2 = "20261106-234457-abcdef"

func testServer(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	work, _ := projectorFixture(t)
	home := filepath.Join(work, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, "models.json"), `{
		"schemaVersion": 1,
		"providers": {
			"p1": {
				"api": "openai-completions",
				"baseUrl": "http://127.0.0.1:1/v1",
				"apiKey": "sk-secret-value",
				"models": [{"id": "real-1", "name": "alpha", "contextWindow": 100000, "supportsTools": true}]
			}
		}
	}`)
	write(filepath.Join(home, "settings.json"), `{"defaultModel":"alpha"}`)
	write(filepath.Join(home, "prompt.md"), "# home prompt")
	write(filepath.Join(work, "README.md"), "# workspace readme")
	write(filepath.Join(work, ".ant", "settings.json"), `{"defaultModel":"alpha"}`)
	write(filepath.Join(work, ".ant", "project.md"), "# project context")
	write(filepath.Join(work, ".ant", "project.meta.json"), `{"hash":"sha256:abc","generatedAt":"2026-11-06T00:00:00Z"}`)

	// 第二个 session，用于列表与排序。
	writeWebSession(t, work, routeTestID2, session.Meta{
		SchemaVersion: 1, ID: routeTestID2, Workspace: work,
		CreatedAt: time.Now().Add(-2 * time.Hour), UpdatedAt: time.Now().Add(-time.Hour),
		Status: session.StatusIdle, Mode: model.ModeAsk, Title: "second session",
		TurnCount: 1, Usage: model.Usage{Input: 3, Output: 2},
	}, []model.Event{
		{Seq: 1, TS: time.Now().Add(-time.Hour), Type: model.EventSessionCreated, Data: model.MustJSON(map[string]any{"id": routeTestID2})},
	}, nil)

	cfg, err := config.NewLoader().Load(context.Background(), config.Options{WorkDir: work, HomeDir: home})
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	server, err := New(Deps{
		WorkDir: work,
		HomeDir: home,
		Config:  cfg,
		Archive: session.NewArchive(work),
		Clock:   time.Now,
		Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts, work, home
}

func getJSON(t *testing.T, ts *httptest.Server, path string) (int, map[string]any, http.Header) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if len(body) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode %s: %v (%s)", path, err, string(body))
		}
	}
	return resp.StatusCode, payload, resp.Header
}

func TestRoutesHealthAndSecurity(t *testing.T) {
	ts, _, _ := testServer(t)
	status, payload, header := getJSON(t, ts, "/api/health")
	if status != http.StatusOK {
		t.Fatalf("health status = %d", status)
	}
	if payload["ok"] != true || payload["readonly"] != true || payload["version"] != "test" {
		t.Fatalf("health payload = %v", payload)
	}
	if header.Get("X-Content-Type-Options") != "nosniff" ||
		header.Get("Referrer-Policy") != "no-referrer" ||
		!strings.Contains(header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("security headers = %v", header)
	}
	if header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("must not send CORS headers")
	}
	if header.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control = %q", header.Get("Cache-Control"))
	}

	// Host 校验：非回环 Host 一律 403。
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("evil host status = %d, want 403", resp.StatusCode)
	}

	// 非 GET 一律 405（一期没有写接口）。
	postResp, err := http.Post(ts.URL+"/api/sessions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", postResp.StatusCode)
	}
}

func TestRoutesSessionsAndDetail(t *testing.T) {
	ts, _, _ := testServer(t)
	status, payload, _ := getJSON(t, ts, "/api/sessions?limit=10")
	if status != http.StatusOK {
		t.Fatalf("sessions status = %d", status)
	}
	list, _ := payload["sessions"].([]any)
	if len(list) != 2 {
		t.Fatalf("sessions = %d, want 2 (%v)", len(list), payload)
	}
	seen := map[string]bool{}
	for _, raw := range list {
		seen[raw.(map[string]any)["id"].(string)] = true
	}
	if !seen[projectTestID] || !seen[routeTestID2] {
		t.Fatalf("sessions missing ids: %v", seen)
	}

	status, detail, header := getJSON(t, ts, "/api/sessions/"+projectTestID)
	if status != http.StatusOK {
		t.Fatalf("detail status = %d", status)
	}
	if detail["id"] != projectTestID || detail["readonly"] != true {
		t.Fatalf("detail = %v", detail)
	}
	turns, _ := detail["turns"].([]any)
	if len(turns) != 2 {
		t.Fatalf("detail turns = %d", len(turns))
	}
	if detail["plan"] == nil {
		t.Fatal("plan should be included")
	}
	etag := header.Get("ETag")
	if etag == "" {
		t.Fatal("detail must set ETag")
	}
	// If-None-Match → 304。
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/sessions/"+projectTestID, nil)
	req.Header.Set("If-None-Match", etag)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("304 status = %d", resp.StatusCode)
	}

	if status, _, _ := getJSON(t, ts, "/api/sessions/20260101-000000-abcdef"); status != http.StatusNotFound {
		t.Fatalf("missing session status = %d", status)
	}
	if status, _, _ := getJSON(t, ts, "/api/sessions/%2e%2e%2f%2e%2e%2fetc%2fpasswd"); status != http.StatusBadRequest && status != http.StatusNotFound {
		t.Fatalf("invalid session id status = %d", status)
	}
}

func TestRoutesArtifactAndPlan(t *testing.T) {
	ts, _, _ := testServer(t)
	url := ts.URL + "/api/sessions/" + projectTestID + "/artifacts/message-1.txt"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "assistant body" {
		t.Fatalf("artifact = %d %q", resp.StatusCode, body)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("artifact content type = %q", resp.Header.Get("Content-Type"))
	}

	for _, path := range []string{
		"../../meta.json",
		"..%2f..%2fmeta.json",
		"%2e%2e%2fmeta.json",
		"missing.txt",
	} {
		resp, err := http.Get(ts.URL + "/api/sessions/" + projectTestID + "/artifacts/" + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("artifact path %q must not succeed", path)
		}
	}

	status, plan, _ := getJSON(t, ts, "/api/sessions/"+projectTestID+"/plan")
	if status != http.StatusOK || !strings.Contains(str(plan["body"]), "# Plan") {
		t.Fatalf("plan = %d %v", status, plan)
	}
	if status, _, _ := getJSON(t, ts, "/api/sessions/"+routeTestID2+"/plan"); status != http.StatusNotFound {
		t.Fatalf("missing plan status = %d", status)
	}
}

func TestRoutesEventsIncremental(t *testing.T) {
	ts, _, _ := testServer(t)
	status, payload, _ := getJSON(t, ts, "/api/sessions/"+projectTestID+"/events?after=20")
	if status != http.StatusOK {
		t.Fatalf("events status = %d", status)
	}
	events, _ := payload["events"].([]any)
	if len(events) == 0 {
		t.Fatal("events after=20 should not be empty")
	}
	for _, raw := range events {
		event := raw.(map[string]any)
		if seq := event["seq"].(float64); seq <= 20 {
			t.Fatalf("event seq %v <= after", seq)
		}
	}
}

func TestRoutesWorkspaceFile(t *testing.T) {
	ts, _, _ := testServer(t)
	status, payload, _ := getJSON(t, ts, "/api/workspace/file?path=README.md")
	if status != http.StatusOK || !strings.Contains(str(payload["content"]), "workspace readme") {
		t.Fatalf("workspace file = %d %v", status, payload)
	}
	for _, path := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		".ant/settings.json",
		".ant/models.json",
		".ant/sessions",
		".git/config",
	} {
		status, _, _ := getJSON(t, ts, "/api/workspace/file?path="+path)
		if status != http.StatusBadRequest {
			t.Fatalf("workspace path %q status = %d, want 400", path, status)
		}
	}
}

func TestRoutesConfigRedaction(t *testing.T) {
	ts, _, _ := testServer(t)
	status, payload, _ := getJSON(t, ts, "/api/config")
	if status != http.StatusOK || payload["available"] != true {
		t.Fatalf("config = %d %v", status, payload["available"])
	}
	encoded, _ := json.Marshal(payload)
	text := string(encoded)
	for _, forbidden := range []string{"sk-secret-value", "apiKey", "apiKeyEnv", "Authorization"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("/api/config leaks %q: %s", forbidden, text)
		}
	}
	models, _ := payload["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models = %d", len(models))
	}
	providers, _ := payload["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("providers = %d", len(providers))
	}
	provider := providers[0].(map[string]any)
	for _, key := range []string{"apiKey", "apiKeyEnv", "headers"} {
		if _, exists := provider[key]; exists {
			t.Fatalf("provider leaks %s", key)
		}
	}
}

func TestRoutesHomeAndProject(t *testing.T) {
	ts, _, _ := testServer(t)
	status, home, _ := getJSON(t, ts, "/api/home")
	if status != http.StatusOK {
		t.Fatalf("home status = %d", status)
	}
	files, _ := home["files"].([]any)
	found := false
	for _, raw := range files {
		file := raw.(map[string]any)
		if file["name"] == "models.json" {
			found = true
			if _, exists := file["content"]; exists {
				t.Fatal("home metadata must not include content")
			}
		}
	}
	if !found {
		t.Fatalf("home files = %v", files)
	}

	status, prompt, _ := getJSON(t, ts, "/api/home/prompt?name=prompt.md")
	if status != http.StatusOK || !strings.Contains(str(prompt["content"]), "home prompt") {
		t.Fatalf("home prompt = %d %v", status, prompt)
	}
	if status, _, _ := getJSON(t, ts, "/api/home/prompt?name=models.json"); status != http.StatusBadRequest {
		t.Fatalf("models.json prompt status = %d, want 400", status)
	}
	if status, _, _ := getJSON(t, ts, "/api/home/prompt?name=../secret"); status != http.StatusBadRequest {
		t.Fatalf("traversal prompt status = %d, want 400", status)
	}

	status, project, _ := getJSON(t, ts, "/api/project")
	if status != http.StatusOK || project["exists"] != true || !strings.Contains(str(project["body"]), "project context") {
		t.Fatalf("project = %d %v", status, project)
	}
	if project["hash"] != "sha256:abc" {
		t.Fatalf("project hash = %v", project["hash"])
	}
}

func TestRoutesStaticAndNoAccountUI(t *testing.T) {
	ts, _, _ := testServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "/assets/app.js") {
		t.Fatalf("index = %d", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("index cache-control = %q", resp.Header.Get("Cache-Control"))
	}

	resp, err = http.Get(ts.URL + "/assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	assetBody, _ := io.ReadAll(resp.Body)
	etag := resp.Header.Get("ETag")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(assetBody) == 0 || etag == "" {
		t.Fatalf("asset = %d etag=%q", resp.StatusCode, etag)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/assets/app.js", nil)
	req.Header.Set("If-None-Match", etag)
	second, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Fatalf("asset 304 = %d", second.StatusCode)
	}

	// SPA fallback 不拦截未知 API。
	if status, _, _ := getJSON(t, ts, "/api/nope"); status != http.StatusNotFound {
		t.Fatalf("unknown api = %d", status)
	}

	// 一期无账号 UI：静态资源里不允许出现账号/登录相关路由与文案。
	server, err := New(Deps{WorkDir: t.TempDir(), Archive: session.NewArchive(t.TempDir()), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	for name, asset := range server.assets {
		lower := strings.ToLower(strings.ReplaceAll(string(asset.body), "function", "fn"))
		for _, forbidden := range []string{"login", "sign in", "signin", "logout", "oauth", "account", "登录", "账号", "头像", "余额", "配额"} {
			if strings.Contains(lower, strings.ToLower(forbidden)) {
				t.Fatalf("static asset %s contains account-related text %q", name, forbidden)
			}
		}
	}
}

func TestRoutesStreamNotifiesChange(t *testing.T) {
	ts, work, _ := testServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/sessions/"+projectTestID+"/stream", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream content type = %q", resp.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(resp.Body)

	// 追加一条事件，触发 changed。
	go func() {
		time.Sleep(300 * time.Millisecond)
		path := filepath.Join(work, ".ant", "sessions", projectTestID, "events.jsonl")
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(`{"seq":99,"ts":"2026-11-07T00:00:00.000Z","type":"usage","turn":2,"data":{"input":1}}` + "\n")
		_ = f.Sync()
	}()

	deadline := time.Now().Add(4 * time.Second)
	var sawChanged bool
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.TrimSpace(line) == "event: changed" {
			sawChanged = true
			break
		}
	}
	if !sawChanged {
		t.Fatal("stream should emit changed after events.jsonl grows")
	}
}

func str(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	raw, _ := json.Marshal(value)
	return string(raw)
}
