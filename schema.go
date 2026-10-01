package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type commandSchema struct {
	Command string            `json:"command"`
	Usage   string            `json:"usage"`
	Inputs  map[string]string `json:"input_bindings"`
	Tool    *mcp.Tool         `json:"tool"`
}

func runSchema(ctx context.Context, args []string) error {
	for _, arg := range args {
		if isHelp(arg) {
			_, err := fmt.Fprintln(os.Stdout, "Usage: linkding schema [bookmarks list|get|save | tags list] [--json]\nDescribe CLI input bindings and the exact MCP input/output schemas. Always emits JSON, offline.")
			return err
		}
	}
	// The optional path precedes flags, just like operation paths.
	pathParts := []string{}
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pathParts, args = append(pathParts, args[0]), args[1:]
	}
	path := strings.Join(pathParts, " ")
	bindings := map[string]map[string]string{
		"bookmarks list": {"query": "--query", "offset": "--offset", "archived": "--archived"},
		"tags list":      {"offset": "--offset"},
		"bookmarks get":  {"id": "positional ID"},
		"bookmarks save": {"url": "stdin JSON", "title": "stdin JSON", "description": "stdin JSON", "notes": "stdin JSON", "tag_names": "stdin JSON"},
	}
	if path != "" && bindings[path] == nil {
		return usageError("unknown schema path; run linkding schema --help")
	}
	flags := flag.NewFlagSet("schema", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Bool("json", true, "output JSON")
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return usageError("unexpected schema argument")
	}
	tools, err := toolSchemas(ctx)
	if err != nil {
		return err
	}
	commands := []commandSchema{}
	for _, tool := range tools {
		var command, usage string
		switch tool.Name {
		case "list_bookmarks":
			command, usage = "bookmarks list", "linkding bookmarks list [--query QUERY] [--offset N] [--archived] [--json]"
		case "list_tags":
			command, usage = "tags list", "linkding tags list [--offset N] [--json]"
		case "get_bookmark":
			command, usage = "bookmarks get", "linkding bookmarks get ID [--json]"
		case "save_bookmark":
			command, usage = "bookmarks save", "linkding bookmarks save --input - [--json]"
		}
		if path == "" || path == command {
			commands = append(commands, commandSchema{Command: command, Usage: usage, Inputs: bindings[command], Tool: tool})
		}
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Name       string            `json:"name"`
		Version    string            `json:"version"`
		Commands   []commandSchema   `json:"commands"`
		ExitCodes  map[string]string `json:"exit_codes"`
		Output     string            `json:"output"`
		InputLimit int               `json:"save_input_limit_bytes"`
	}{Name: "linkding", Version: version, Commands: commands,
		ExitCodes: map[string]string{"0": "success, including empty results", "1": "runtime or configuration error",
			"2": "invalid arguments or input", "130": "interrupted"},
		Output: "JSON; indented by default, compact with --json. Diagnostics use stderr.", InputLimit: maxResponseBytes})
}

func toolSchemas(ctx context.Context) ([]*mcp.Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// SDK v1.8.0 copies inferred Tool schemas and has no public offline iterator.
	// An in-memory session retrieves the exact published schemas without an API client.
	server := newMCPServer(nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		return nil, errors.New("could not load tool schemas")
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "linkding-schema", Version: version}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, errors.New("could not load tool schemas")
	}
	defer clientSession.Close()
	result, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		return nil, errors.New("could not load tool schemas")
	}
	return result.Tools, nil
}
