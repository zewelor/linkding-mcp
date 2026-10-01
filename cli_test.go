package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type cliObservation struct {
	Args     []string `json:"args"`
	Input    string   `json:"stdin,omitempty"`
	ExitCode int      `json:"exit_code"`
	Stdout   string   `json:"stdout"`
	Stderr   string   `json:"stderr"`
}

func TestCLIE2E(t *testing.T) {
	report := struct {
		Mode     string           `json:"mode"`
		Cases    []evidence       `json:"cases"`
		Commands []cliObservation `json:"commands"`
	}{Mode: "native", Cases: []evidence{}, Commands: []cliObservation{}}
	if *dockerE2E {
		report.Mode = "docker"
	}
	t.Cleanup(func() {
		if err := os.MkdirAll("artifacts", 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("artifacts/e2e-cli-"+report.Mode+".json", append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	program := filepath.Join(t.TempDir(), "linkding")
	if !*dockerE2E {
		cmd := exec.CommandContext(t.Context(), "go", "build", "-o", program, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
	}
	command := func(ctx context.Context, base string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, program, args...)
		if *dockerE2E {
			cmd = exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "host",
				"--read-only", "--user", "65532:65532", "--cap-drop", "ALL",
				"--security-opt", "no-new-privileges:true", "--log-driver", "none",
				"-e", "LINKDING_URL", "-e", "LINKDING_TOKEN", "linkding:e2e")
			// No arguments explicitly overrides Docker's default MCP command.
			if len(args) == 0 {
				cmd.Args = append(cmd.Args, "--help")
			} else {
				cmd.Args = append(cmd.Args, args...)
			}
		}
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LINKDING_URL=" + base}
		if base != "" {
			cmd.Env = append(cmd.Env, "LINKDING_TOKEN="+fixtureToken)
		}
		return cmd
	}
	run := func(t *testing.T, base, input string, args ...string) cliObservation {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := command(ctx, base, args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(input), &stdout, &stderr
		err := cmd.Run()
		if cmd.ProcessState == nil || ctx.Err() != nil {
			t.Fatalf("CLI process: %v", err)
		}
		result := cliObservation{Args: args, Input: input, ExitCode: cmd.ProcessState.ExitCode(),
			Stdout: stdout.String(), Stderr: stderr.String()}
		if strings.Contains(result.Stdout+result.Stderr, fixtureToken) {
			t.Fatal("token leaked in CLI output")
		}
		report.Commands = append(report.Commands, result)
		return result
	}

	url := "https://example.test/article?a=1&b=2"
	saved := `{"id":42,"url":"` + url + `","title":"Go café"}`
	q := map[string][]string{"limit": {"20"}, "offset": {"0"}}
	checkQ := map[string][]string{"url": {url}}
	emptyQ := map[string][]string{}
	workflow := []exchange{
		{method: "GET", path: "/api/bookmarks/", query: map[string][]string{
			"limit": {"20"}, "offset": {"0"}, "q": {"--help"}}, status: 200, reply: `{"count":0,"results":[]}`},
		{method: "GET", path: "/api/bookmarks/", query: map[string][]string{
			"limit": {"20"}, "offset": {"20"}, "q": {"#go or #rust"}}, status: 200, reply: `{"count":21,"results":[]}`},
		{method: "GET", path: "/api/bookmarks/archived/", query: q, status: 200, reply: `{"count":0,"results":[]}`},
		{method: "GET", path: "/api/tags/", query: q, status: 200, reply: `{"count":1,"results":[{"id":1,"name":"go"}]}`},
		{method: "GET", path: "/api/bookmarks/42/", query: emptyQ, status: 200,
			reply: `{"id":42,"url":"` + url + `","title":"Go café","notes":"Stored notes"}`},
		{method: "GET", path: "/api/bookmarks/check/", query: checkQ, status: 200, reply: `{"bookmark":null}`},
		{method: "POST", path: "/api/bookmarks/", query: emptyQ, status: 201, reply: saved,
			body: `{"url":"` + url + `","notes":"Line one\nLine two","tag_names":["go","reading"]}`},
		{method: "GET", path: "/api/bookmarks/check/", query: checkQ, status: 200, reply: `{"bookmark":` + saved + `}`},
		{method: "GET", path: "/api/bookmarks/check/", query: checkQ, status: 200, reply: `{"bookmark":` + saved + `}`},
		{method: "PATCH", path: "/api/bookmarks/42/", query: emptyQ, status: 200, reply: saved, body: `{"description":"New description"}`},
		{method: "GET", path: "/api/bookmarks/check/", query: checkQ, status: 200, reply: `{"bookmark":` + saved + `}`},
		{method: "PATCH", path: "/api/bookmarks/42/", query: emptyQ, status: 200,
			reply: `{"id":42,"url":"` + url + `","title":""}`, body: `{"title":"","notes":"","tag_names":[]}`},
		{method: "GET", path: "/api/bookmarks/check/", query: checkQ, status: 200, reply: `{"bookmark":` + saved + `}`},
		{method: "PATCH", path: "/api/bookmarks/42/", query: emptyQ, disconnect: true, body: `{"notes":"Interrupted"}`},
		{method: "GET", path: "/api/tags/", query: q, status: 401, reply: fixtureToken},
	}
	entry := evidence{Name: "bookmark_workflow", Requests: []observation{}}
	entry.Passed = t.Run(entry.Name, func(t *testing.T) {
		var mu sync.Mutex
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			defer mu.Unlock()
			index := len(entry.Requests)
			entry.Requests = append(entry.Requests, observation{Method: r.Method, Path: r.URL.Path,
				Query: r.URL.Query(), Authorized: r.Header.Get("Authorization") == "Token "+fixtureToken,
				Body: string(body)})
			if index >= len(workflow) {
				t.Error("unexpected HTTP request, possibly a retry")
				w.WriteHeader(500)
				return
			}
			x := workflow[index]
			if r.Method != x.method || r.URL.Path != "/linkding"+x.path ||
				!reflect.DeepEqual(map[string][]string(r.URL.Query()), x.query) || !entry.Requests[index].Authorized {
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			}
			if x.body != "" {
				compareJSON(t, body, []byte(x.body))
			} else if len(body) != 0 {
				t.Error("unexpected request body")
			}
			if x.disconnect {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(x.status)
			io.WriteString(w, x.reply)
		}))
		defer server.Close()
		base := server.URL + "/linkding/"
		calls := []struct {
			args  []string
			input string
			want  string
		}{
			{args: []string{"bookmarks", "list", "--query", "--help", "--json"}, want: `{"count":0,"results":[]}`},
			{args: []string{"bookmarks", "list", "--query", "#go or #rust", "--offset", "20", "--json"}, want: `{"count":21,"results":[]}`},
			{args: []string{"bookmarks", "list", "--archived", "--json"}, want: `{"count":0,"results":[]}`},
			{args: []string{"tags", "list", "--json"}, want: `{"count":1,"results":[{"id":1,"name":"go"}]}`},
			{args: []string{"bookmarks", "get", "42", "--json"}, want: `{"id":42,"url":"` + url + `","title":"Go café","description":"","tag_names":[],"date_added":"","date_modified":"","notes":"Stored notes"}`},
			{args: []string{"bookmarks", "save", "--input", "-", "--json"},
				input: `{"url":"` + url + `","notes":"Line one\nLine two","tag_names":["go","reading"]}`,
				want:  `{"status":"created","id":42,"url":"` + url + `","title":"Go café"}`},
			{args: []string{"bookmarks", "save", "--input", "-", "--json"},
				input: `{"url":"` + url + `","title":null,"notes":null,"tag_names":null}`,
				want:  `{"status":"already_exists","id":42,"url":"` + url + `","title":"Go café"}`},
			{args: []string{"bookmarks", "save", "--input", "-", "--json"},
				input: `{"url":"` + url + `","description":"New description"}`,
				want:  `{"status":"updated","id":42,"url":"` + url + `","title":"Go café"}`},
			{args: []string{"bookmarks", "save", "--input", "-", "--json"},
				input: `{"url":"` + url + `","title":"","notes":"","tag_names":[]}`,
				want:  `{"status":"updated","id":42,"url":"` + url + `","title":""}`},
		}
		for _, call := range calls {
			got := run(t, base, call.input, call.args...)
			if got.ExitCode != 0 || got.Stderr != "" {
				t.Fatalf("CLI failed: %+v", got)
			}
			compareJSON(t, []byte(got.Stdout), []byte(call.want))
		}
		got := run(t, base, `{"url":"`+url+`","notes":"Interrupted"}`, "bookmarks", "save", "--input", "-", "--json")
		if got.ExitCode != 1 || got.Stdout != "" || !strings.Contains(got.Stderr, "outcome may be unknown") {
			t.Errorf("interrupted write output: %+v", got)
		}
		got = run(t, base, "", "tags", "list", "--json")
		if got.ExitCode != 1 || got.Stdout != "" || !strings.Contains(got.Stderr, "http 401") {
			t.Errorf("API error output: %+v", got)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(entry.Requests) != len(workflow) {
			t.Errorf("request count: got %d, want %d", len(entry.Requests), len(workflow))
		}
	})
	report.Cases = append(report.Cases, entry)

	for _, args := range [][]string{
		{}, {"--help"}, {"--version"}, {"bookmarks", "--help"}, {"bookmarks", "save", "--help"},
		{"mcp", "--help"}, {"skills", "list"}, {"skills", "get", "core"},
	} {
		entry := evidence{Name: "offline_" + strings.Join(args, "_"), Requests: []observation{}}
		entry.Passed = t.Run(entry.Name, func(t *testing.T) {
			got := run(t, "", "", args...)
			if got.ExitCode != 0 || got.Stdout == "" || got.Stderr != "" {
				t.Errorf("offline discovery failed: %+v", got)
			}
		})
		report.Cases = append(report.Cases, entry)
	}
	for _, invalid := range []struct {
		name  string
		args  []string
		input string
	}{
		{name: "unknown_command", args: []string{"delete"}},
		{name: "invalid_offset", args: []string{"bookmarks", "list", "--offset", "-1"}},
		{name: "invalid_bool", args: []string{"bookmarks", "list", "--archived=yes"}},
		{name: "duplicate_flag", args: []string{"tags", "list", "--offset", "1", "--offset", "2"}},
		{name: "invalid_id", args: []string{"bookmarks", "get", "0"}},
		{name: "extra_argument", args: []string{"tags", "list", "extra"}},
		{name: "mcp_json", args: []string{"mcp", "--json"}},
		{name: "unknown_schema", args: []string{"schema", "bookmarks", "delete", "--json"}},
		{name: "unknown_skill", args: []string{"skills", "get", "missing"}},
		{name: "save_missing_input", args: []string{"bookmarks", "save"}},
		{name: "trailing_json", input: `{"url":"https://example.test"} {}`},
		{name: "unknown_field", input: `{"url":"https://example.test","unread":true}`},
		{name: "duplicate_field", input: `{"url":"https://example.test","url":"https://other.test"}`},
		{name: "wrong_case_field", input: `{"URL":"https://example.test"}`},
		{name: "invalid_type", input: `{"url":"https://example.test","notes":123}`},
		{name: "null_tag_item", input: `{"url":"https://example.test","tag_names":[null]}`},
		{name: "null_object", input: `null`},
		{name: "missing_url", input: `{}`},
		{name: "invalid_url", input: `{"url":"file:///tmp/example"}`},
		{name: "truncated_json", input: `{"url":`},
	} {
		entry := evidence{Name: "invalid_" + invalid.name, Requests: []observation{}}
		entry.Passed = t.Run(entry.Name, func(t *testing.T) {
			args := invalid.args
			if invalid.input != "" {
				args = []string{"bookmarks", "save", "--input", "-", "--json"}
			}
			// No credentials: input errors must win over configuration errors.
			got := run(t, "", invalid.input, args...)
			if got.ExitCode != 2 || got.Stdout != "" || got.Stderr == "" {
				t.Errorf("invalid input output: %+v", got)
			}
		})
		report.Cases = append(report.Cases, entry)
	}
	entry = evidence{Name: "interrupt_open_stdin", Requests: []observation{}}
	entry.Passed = t.Run(entry.Name, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		args := []string{"bookmarks", "save", "--input", "-", "--json"}
		cmd := command(ctx, "", args...)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer stdin.Close()
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// Exceed the pipe buffer to establish that the subprocess is consuming input.
		input := `{"url":"https://example.test"}` + strings.Repeat(" ", 1024*1024)
		if _, err := io.WriteString(stdin, input); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			cancel()
			<-done
			t.Fatal("CLI ignored cancellation while stdin remained open")
		}
		result := cliObservation{Args: args, Input: input, ExitCode: cmd.ProcessState.ExitCode(),
			Stdout: stdout.String(), Stderr: stderr.String()}
		report.Commands = append(report.Commands, result)
		if result.ExitCode != 130 || result.Stdout != "" {
			t.Errorf("stdin cancellation: exit %d, stdout %q, stderr %q", result.ExitCode, result.Stdout, result.Stderr)
		}
	})
	report.Cases = append(report.Cases, entry)
	entry = evidence{Name: "offline_schema_matches_mcp", Requests: []observation{}}
	entry.Passed = t.Run(entry.Name, func(t *testing.T) {
		got := run(t, "", "", "schema", "--json")
		if got.ExitCode != 0 || got.Stderr != "" {
			t.Fatalf("schema failed: %+v", got)
		}
		var schema struct {
			Commands []struct {
				Command string    `json:"command"`
				Tool    *mcp.Tool `json:"tool"`
			} `json:"commands"`
		}
		if err := json.Unmarshal([]byte(got.Stdout), &schema); err != nil {
			t.Fatal(err)
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "cli-e2e", Version: "1"}, nil)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		session, err := client.Connect(ctx, &mcp.CommandTransport{
			Command: command(ctx, "http://127.0.0.1:1", "mcp"), TerminateDuration: time.Second,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		tools, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(schema.Commands) != 4 || len(tools.Tools) != 4 {
			t.Fatalf("command/tool inventory: %d/%d", len(schema.Commands), len(tools.Tools))
		}
		for _, command := range schema.Commands {
			found := false
			for _, tool := range tools.Tools {
				if command.Tool != nil && tool.Name == command.Tool.Name {
					actual, _ := json.Marshal(command.Tool)
					want, _ := json.Marshal(tool)
					compareJSON(t, actual, want)
					found = true
				}
			}
			if !found {
				t.Errorf("missing MCP schema for %s", command.Command)
			}
		}
		got = run(t, "", "", "schema", "bookmarks", "save", "--json")
		if got.ExitCode != 0 || !json.Valid([]byte(got.Stdout)) {
			t.Errorf("targeted schema output: %+v", got)
		}
	})
	report.Cases = append(report.Cases, entry)
}
