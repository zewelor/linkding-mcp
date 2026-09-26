package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var dockerE2E = flag.Bool("e2e-docker", false, "run the same scenarios through linkding-mcp:e2e")

const fixtureToken = "e2e-token-must-not-appear-in-errors"

type exchange struct {
	method, path string
	query        map[string][]string
	body         string
	status       int
	reply        string
	location     string
	disconnect   bool
}

type toolCall struct {
	name      string
	args      map[string]any
	want      string
	wantError string
}

type observation struct {
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Query      map[string][]string `json:"query"`
	Authorized bool                `json:"authorized"`
	Body       string              `json:"body,omitempty"`
}

type evidence struct {
	Name     string        `json:"name"`
	Passed   bool          `json:"passed"`
	Requests []observation `json:"requests"`
}

func TestE2E(t *testing.T) {
	mode := "native"
	if *dockerE2E {
		mode = "docker"
	}
	report := struct {
		Mode  string     `json:"mode"`
		Cases []evidence `json:"cases"`
	}{Mode: mode, Cases: []evidence{}}
	defer func() {
		if err := os.MkdirAll("artifacts", 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		name := "artifacts/e2e.json"
		if *dockerE2E {
			name = "artifacts/e2e-docker.json"
		}
		if err := os.WriteFile(name, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}()
	program := filepath.Join(t.TempDir(), "linkding-mcp")
	if !*dockerE2E {
		build := exec.CommandContext(t.Context(), "go", "build", "-o", program, ".")
		if out, err := build.CombinedOutput(); err != nil {
			report.Cases = append(report.Cases, evidence{Name: "build", Requests: []observation{}})
			t.Fatalf("build: %v\n%s", err, out)
		}
	}
	command := func(base, token string, caFile ...string) *exec.Cmd {
		cmd := exec.Command(program)
		if *dockerE2E {
			cmd = exec.Command("docker", "run", "--rm", "-i", "--network", "host",
				"--read-only", "--user", "65532:65532", "--cap-drop", "ALL",
				"--security-opt", "no-new-privileges:true", "--log-driver", "none",
				"-e", "LINKDING_URL", "-e", "LINKDING_TOKEN", "linkding-mcp:e2e")
		}
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LINKDING_URL=" + base, "LINKDING_TOKEN=" + token}
		if len(caFile) > 0 {
			if *dockerE2E {
				cmd.Args = append(cmd.Args[:len(cmd.Args)-1], "--mount", "type=bind,src="+caFile[0]+",dst=/fixture-ca.pem,readonly",
					"-e", "SSL_CERT_FILE=/fixture-ca.pem", "linkding-mcp:e2e")
			} else {
				cmd.Env = append(cmd.Env, "SSL_CERT_FILE="+caFile[0])
			}
		}
		return cmd
	}
	connect := func(t *testing.T, base string, caFile ...string) *mcp.ClientSession {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		t.Cleanup(cancel)
		cmd := command(base, fixtureToken, caFile...)
		var stderr, protocolLog bytes.Buffer
		cmd.Stderr = &stderr
		client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0.1.0"},
			&mcp.ClientOptions{Logger: slog.New(slog.NewTextHandler(&protocolLog, nil))})
		session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}, nil)
		if err != nil {
			t.Fatalf("stdio initialization: %v", err)
		}
		t.Cleanup(func() {
			if err := session.Close(); err != nil {
				t.Errorf("stdio shutdown: %v", err)
			}
			if strings.Contains(stderr.String(), fixtureToken) {
				t.Error("token leaked to stderr")
			}
			if protocolLog.Len() != 0 {
				t.Errorf("MCP protocol diagnostic: %s", protocolLog.String())
			}
		})
		tools, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, tool := range tools.Tools {
			names = append(names, tool.Name)
			if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != (tool.Name != "save_bookmark") {
				t.Errorf("incorrect read-only hint for %s", tool.Name)
			}
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"get_bookmark", "list_bookmarks", "list_tags", "save_bookmark"}) {
			t.Fatalf("tool inventory: %v", names)
		}
		return session
	}

	bookmark := func(id int) string {
		return fmt.Sprintf(`{"id":%d,"url":"https://example.test/%d","title":"Go café","description":"Article","tag_names":["go"],"notes":"Private notes","unread":true}`, id, id)
	}
	brief := func(id int) string {
		return fmt.Sprintf(`{"id":%d,"url":"https://example.test/%d","title":"Go café","description":"Article","tag_names":["go"]}`, id, id)
	}
	fullPage, briefPage, tags := []string{}, []string{}, []string{}
	for id := 1; id <= 20; id++ {
		fullPage = append(fullPage, bookmark(id))
		briefPage = append(briefPage, brief(id))
		tags = append(tags, fmt.Sprintf(`{"id":%d,"name":"tag%d","date_added":"2026-09-26"}`, id, id))
	}
	query := "#go + notatki &"
	pageQuery := func(offset string) map[string][]string {
		return map[string][]string{"limit": {"20"}, "offset": {offset}}
	}
	searchQuery := func(offset string) map[string][]string {
		q := pageQuery(offset)
		q["q"] = []string{query}
		return q
	}
	newURL := "https://example.test/new?a=1&b=2"
	saveResult := `{"id":22,"url":"` + newURL + `","title":"Saved by Linkding"}`
	newArgs := map[string]any{"url": newURL}
	check := exchange{method: "GET", path: "/api/bookmarks/check/", query: map[string][]string{"url": {newURL}},
		status: 200, reply: `{"bookmark":null,"metadata":{"title":"Not persisted"},"auto_tags":["go"]}`}
	post := exchange{method: "POST", path: "/api/bookmarks/", query: map[string][]string{},
		body: `{"url":"` + newURL + `"}`, status: 201, reply: saveResult}
	scenarios := []struct {
		name  string
		http  []exchange
		calls []toolCall
	}{
		{name: "bookmarks_pagination", http: []exchange{
			{method: "GET", path: "/api/bookmarks/", query: searchQuery("0"), status: 200,
				reply: `{"count":21,"next":"https://untrusted.test/next","results":[` + strings.Join(fullPage, ",") + `]}`},
			{method: "GET", path: "/api/bookmarks/", query: searchQuery("20"), status: 200,
				reply: `{"count":21,"results":[` + bookmark(21) + `]}`},
		}, calls: []toolCall{
			{name: "list_bookmarks", args: map[string]any{"query": query},
				want: `{"count":21,"results":[` + strings.Join(briefPage, ",") + `]}`},
			{name: "list_bookmarks", args: map[string]any{"query": query, "offset": 20},
				want: `{"count":21,"results":[` + brief(21) + `]}`},
		}},
		{name: "tags_and_detail", http: []exchange{
			{method: "GET", path: "/api/tags/", query: pageQuery("0"), status: 200,
				reply: `{"count":21,"results":[` + strings.Join(tags, ",") + `]}`},
			{method: "GET", path: "/api/tags/", query: pageQuery("20"), status: 200,
				reply: `{"count":21,"results":[{"id":21,"name":"go"}]}`},
			{method: "GET", path: "/api/bookmarks/21/", query: map[string][]string{}, status: 200, reply: bookmark(21)},
		}, calls: []toolCall{
			{name: "list_tags", args: map[string]any{}, want: strings.ReplaceAll(
				`{"count":21,"results":[`+strings.Join(tags, ",")+`]}`, `,"date_added":"2026-09-26"`, "")},
			{name: "list_tags", args: map[string]any{"offset": 20}, want: `{"count":21,"results":[{"id":21,"name":"go"}]}`},
			{name: "get_bookmark", args: map[string]any{"id": 21}, want: strings.Replace(bookmark(21), `,"unread":true`, "", 1)},
		}},
		{name: "new_url_saved_once", http: []exchange{check, post}, calls: []toolCall{
			{name: "save_bookmark", args: newArgs, want: `{"status":"created","id":22,"url":"` + newURL + `","title":"Saved by Linkding"}`},
		}},
		{name: "existing_url_without_post", http: []exchange{
			{method: "GET", path: check.path, query: check.query, status: 200, reply: `{"bookmark":` + saveResult + `}`},
		}, calls: []toolCall{
			{name: "save_bookmark", args: newArgs, want: `{"status":"already_exists","id":22,"url":"` + newURL + `","title":"Saved by Linkding"}`},
		}},
		{name: "redirect_not_followed", http: []exchange{
			{method: "GET", path: check.path, query: check.query, status: 302,
				location: "/linkding/api/unexpected/"},
		}, calls: []toolCall{{name: "save_bookmark", args: newArgs, wantError: "http 302"}}},
		{name: "missing_list_results", http: []exchange{
			{method: "GET", path: "/api/bookmarks/", query: searchQuery("0"), status: 200, reply: `{}`},
		}, calls: []toolCall{{name: "list_bookmarks", args: map[string]any{"query": query}, wantError: "invalid list response"}}},
	}
	for _, title := range []struct{ name, field string }{
		{"missing", ""}, {"null", `,"title":null`}, {"empty", `,"title":""`},
	} {
		for _, existing := range []bool{true, false} {
			response := `{"id":22,"url":"` + newURL + `"` + title.field + `}`
			x, y := check, post
			exchanges, status, route := []exchange{x, y}, "created", "post"
			if existing {
				x.reply = `{"bookmark":` + response + `}`
				exchanges, status, route = []exchange{x}, "already_exists", "check"
			} else {
				exchanges[1].reply = response
			}
			call := toolCall{name: "save_bookmark", args: newArgs, wantError: "invalid bookmark"}
			if title.name == "empty" {
				call.wantError = ""
				call.want = `{"status":"` + status + `","id":22,"url":"` + newURL + `","title":""}`
			}
			scenarios = append(scenarios, struct {
				name  string
				http  []exchange
				calls []toolCall
			}{name: route + "_title_" + title.name, http: exchanges, calls: []toolCall{call}})
		}
	}
	for _, tool := range []string{"list_bookmarks", "list_tags"} {
		q := pageQuery("0")
		args := map[string]any{}
		path := "/api/tags/"
		if tool == "list_bookmarks" {
			q, args, path = searchQuery("0"), map[string]any{"query": query}, "/api/bookmarks/"
		}
		scenarios = append(scenarios, struct {
			name  string
			http  []exchange
			calls []toolCall
		}{name: tool + "_empty", http: []exchange{{method: "GET", path: path, query: q, status: 200,
			reply: `{"count":0,"results":[]}`}}, calls: []toolCall{{name: tool, args: args, want: `{"count":0,"results":[]}`}}})
	}
	scenarios = append(scenarios, struct {
		name  string
		http  []exchange
		calls []toolCall
	}{name: "list_bookmarks_without_query", http: []exchange{{method: "GET", path: "/api/bookmarks/", query: pageQuery("0"), status: 200,
		reply: `{"count":0,"results":[]}`}}, calls: []toolCall{{name: "list_bookmarks", args: map[string]any{}, want: `{"count":0,"results":[]}`}}})
	invalid := []toolCall{
		{name: "list_bookmarks", args: map[string]any{"query": 123}, wantError: "validating"},
		{name: "list_bookmarks", args: map[string]any{"query": query, "offset": -1}, wantError: "offset must"},
		{name: "list_tags", args: map[string]any{"offset": -1}, wantError: "offset must"},
		{name: "get_bookmark", args: map[string]any{"id": 0}, wantError: "id must"},
		{name: "get_bookmark", args: map[string]any{}, wantError: "validating"},
		{name: "save_bookmark", args: map[string]any{"url": "file:///tmp/bookmark"}, wantError: "url must"},
		{name: "save_bookmark", args: map[string]any{"url": "relative/path"}, wantError: "url must"},
	}
	for i, call := range invalid {
		scenarios = append(scenarios, struct {
			name  string
			http  []exchange
			calls []toolCall
		}{
			name: fmt.Sprintf("invalid_%s_%d_no_http", call.name, i), calls: []toolCall{call}})
	}
	faults := []struct {
		name, reply, wantError string
		status                 int
	}{
		{"unauthorized", fixtureToken, "http 401", 401},
		{"server_error", fixtureToken, "http 503", 503},
		{"malformed", "{", "invalid json", 200},
		{"missing_bookmark", `{}`, "invalid check response", 200},
		{"bad_bookmark", `{"bookmark":{}}`, "invalid bookmark", 200},
		{"oversized", strings.Repeat("x", 2*1024*1024+1), "response exceeds", 200},
	}
	for _, fault := range faults {
		x := check
		x.status, x.reply = fault.status, fault.reply
		scenarios = append(scenarios, struct {
			name  string
			http  []exchange
			calls []toolCall
		}{
			name: "check_" + fault.name + "_no_post", http: []exchange{x},
			calls: []toolCall{{name: "save_bookmark", args: newArgs, wantError: fault.wantError}}})
	}
	for _, fault := range []struct {
		name, reply, wantError string
		status                 int
		disconnect             bool
	}{
		{"interrupted", "", "outcome may be unknown", 201, true},
		{"invalid_result", `{}`, "invalid bookmark", 201, false},
		{"failure", fixtureToken, "http 500", 500, false},
	} {
		x := post
		x.status, x.reply, x.disconnect = fault.status, fault.reply, fault.disconnect
		scenarios = append(scenarios, struct {
			name  string
			http  []exchange
			calls []toolCall
		}{
			name: "post_" + fault.name + "_without_retry", http: []exchange{check, x},
			calls: []toolCall{{name: "save_bookmark", args: newArgs, wantError: fault.wantError}}})
	}
	scenarios = append(scenarios, struct {
		name  string
		http  []exchange
		calls []toolCall
	}{
		name: "missing_bookmark_404", http: []exchange{{method: "GET", path: "/api/bookmarks/999/",
			query: map[string][]string{}, status: 404, reply: fixtureToken}},
		calls: []toolCall{{name: "get_bookmark", args: map[string]any{"id": 999}, wantError: "http 404"}}})
	scenarios = append(scenarios, struct {
		name  string
		http  []exchange
		calls []toolCall
	}{
		name: "interrupted_get_without_retry",
		http: []exchange{
			{method: "GET", path: "/api/bookmarks/1/", query: map[string][]string{}, status: 200, reply: bookmark(1)},
			{method: "GET", path: "/api/bookmarks/2/", query: map[string][]string{}, disconnect: true},
		},
		calls: []toolCall{
			{name: "get_bookmark", args: map[string]any{"id": 1}, want: strings.Replace(bookmark(1), `,"unread":true`, "", 1)},
			{name: "get_bookmark", args: map[string]any{"id": 2}, wantError: "request failed"},
		}})

	for _, scenario := range scenarios {
		entry := evidence{Name: scenario.name, Requests: []observation{}}
		entry.Passed = t.Run(scenario.name, func(t *testing.T) {
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				mu.Lock()
				defer mu.Unlock()
				index := len(entry.Requests)
				entry.Requests = append(entry.Requests, observation{Method: r.Method, Path: r.URL.Path,
					Query: r.URL.Query(), Authorized: r.Header.Get("Authorization") == "Token "+fixtureToken,
					Body: string(body)})
				if index >= len(scenario.http) {
					t.Error("unexpected HTTP request")
					w.WriteHeader(500)
					return
				}
				x := scenario.http[index]
				if r.Method != x.method || r.URL.Path != "/linkding"+x.path ||
					!reflect.DeepEqual(map[string][]string(r.URL.Query()), x.query) {
					t.Errorf("unexpected HTTP route: %s %s", r.Method, r.URL.String())
				}
				if !entry.Requests[index].Authorized {
					t.Error("missing or wrong API authorization")
				}
				if x.body != "" {
					compareJSON(t, body, []byte(x.body))
				} else if len(body) != 0 {
					t.Error("unexpected body")
				}
				if x.disconnect {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					if err := conn.Close(); err != nil {
						t.Error(err)
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if x.location != "" {
					w.Header().Set("Location", x.location)
				}
				w.WriteHeader(x.status)
				// A canceled connection may reject the oversized response tail.
				if _, err := io.WriteString(w, x.reply); err != nil && x.status != 200 {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			session := connect(t, server.URL+"/linkding/")
			for _, call := range scenario.calls {
				res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: call.name, Arguments: call.args})
				if err != nil {
					t.Fatalf("MCP call: %v", err)
				}
				encoded, err := json.Marshal(res)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(encoded, []byte(fixtureToken)) {
					t.Fatal("token leaked in tool response")
				}
				if call.wantError != "" {
					if !res.IsError || !strings.Contains(string(encoded), call.wantError) {
						t.Errorf("expected tool error containing %q; got %s", call.wantError, encoded)
					}
					continue
				}
				if res.IsError {
					t.Fatalf("tool error: %s", encoded)
				}
				out, err := json.Marshal(res.StructuredContent)
				if err != nil {
					t.Fatal(err)
				}
				compareJSON(t, out, []byte(call.want))
			}
			mu.Lock()
			defer mu.Unlock()
			if len(entry.Requests) != len(scenario.http) {
				t.Errorf("HTTP request count: got %d, want %d", len(entry.Requests), len(scenario.http))
			}
		})
		report.Cases = append(report.Cases, entry)
	}

	for _, config := range []struct{ name, url, token string }{
		{"missing_url", "", fixtureToken},
		{"relative_url", "localhost", fixtureToken},
		{"url_credentials", "http://" + fixtureToken + "@localhost", fixtureToken},
		{"url_query", "http://localhost?token=" + fixtureToken, fixtureToken},
		{"url_fragment", "http://localhost#" + fixtureToken, fixtureToken},
		{"missing_token", "http://localhost", ""},
		{"token_newline", "http://localhost", fixtureToken + "\n"},
	} {
		entry := evidence{Name: "startup_" + config.name, Requests: []observation{}}
		entry.Passed = t.Run(entry.Name, func(t *testing.T) {
			cmd := command(config.url, config.token)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			cmd.WaitDelay = time.Second
			if err := cmd.Run(); err == nil {
				t.Error("bad configuration accepted")
			}
			if stdout.Len() != 0 || stderr.Len() == 0 {
				t.Error("startup diagnostics must use stderr only")
			}
			if strings.Contains(stderr.String(), fixtureToken) {
				t.Error("token leaked in startup error")
			}
		})
		report.Cases = append(report.Cases, entry)
	}

	tlsEntry := evidence{Name: "https_http1_negotiation", Requests: []observation{}}
	tlsEntry.Passed = t.Run(tlsEntry.Name, func(t *testing.T) {
		requests := make(chan observation, 1)
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor != 1 || r.URL.Path != "/api/tags/" {
				t.Error("unexpected HTTPS protocol or path")
			}
			requests <- observation{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(),
				Authorized: r.Header.Get("Authorization") == "Token "+fixtureToken}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"count":0,"results":[]}`)
		}))
		server.EnableHTTP2 = true
		server.StartTLS()
		t.Cleanup(server.Close)
		caFile := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o644); err != nil {
			t.Fatal(err)
		}
		session := connect(t, server.URL, caFile)
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_tags", Arguments: map[string]any{}})
		if err != nil || result.IsError {
			t.Fatalf("HTTPS MCP call failed: %v, %v", err, result)
		}
		got, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		compareJSON(t, got, []byte(`{"count":0,"results":[]}`))
		request := <-requests
		tlsEntry.Requests = append(tlsEntry.Requests, request)
		if !request.Authorized {
			t.Error("missing API authorization")
		}
	})
	report.Cases = append(report.Cases, tlsEntry)

	entry := evidence{Name: "cancellation_reaches_http", Requests: []observation{}}
	entry.Passed = t.Run(entry.Name, func(t *testing.T) {
		started, canceled := make(chan bool, 1), make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started <- r.Header.Get("Authorization") == "Token "+fixtureToken
			select {
			case <-r.Context().Done():
				close(canceled)
			case <-time.After(4 * time.Second):
				w.WriteHeader(500)
			}
		}))
		t.Cleanup(server.Close)
		session := connect(t, server.URL)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_bookmarks", Arguments: map[string]any{"query": "cancel"}})
			result <- err
		}()
		select {
		case authorized := <-started:
			entry.Requests = append(entry.Requests, observation{Method: "GET", Path: "/api/bookmarks/", Authorized: authorized})
			if !authorized {
				t.Error("missing API authorization")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("HTTP request did not start")
		}
		cancel()
		select {
		case err := <-result:
			if err == nil {
				t.Error("cancellation returned success")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("MCP call was not canceled")
		}
		select {
		case <-canceled:
		case <-time.After(3 * time.Second):
			t.Error("cancellation did not reach HTTP")
		}
	})
	report.Cases = append(report.Cases, entry)
}

func compareJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var actual, expected any
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("JSON mismatch\ngot:  %s\nwant: %s", got, want)
	}
}
