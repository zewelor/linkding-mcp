package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const version = "0.2.0"

//go:embed docs/cli-core.md
var coreSkill string

type usageError string

func (e usageError) Error() string { return string(e) }

const rootHelp = `Usage: linkding <command>

Search, read, and save bookmarks in one Linkding instance.

Commands:
  bookmarks list [--query QUERY] [--offset N] [--archived] [--json]
  bookmarks get ID [--json]
  bookmarks save --input - [--json]    Read one save object from stdin
  tags list [--offset N] [--json]
  mcp                                Run the four MCP tools over stdio
  schema [bookmarks list|get|save | tags list] [--json]
  skills list
  skills get core                    Version-matched agent instructions
  version

Use linkding <command> --help for details. Flags belong to their command.
LINKDING_URL and LINKDING_TOKEN are required only for API operations and MCP.
Exit codes: 0 success (including empty results), 1 runtime/configuration error,
2 invalid arguments/input, 130 interrupted. Diagnostics use stderr.
`

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		_, err := fmt.Fprint(os.Stdout, rootHelp)
		return err
	}
	switch args[0] {
	case "--help", "-h":
		_, err := fmt.Fprint(os.Stdout, rootHelp)
		return err
	case "version", "--version":
		if len(args) != 1 {
			return usageError("version takes no arguments")
		}
		_, err := fmt.Fprintln(os.Stdout, "linkding", version)
		return err
	case "mcp":
		if len(args) == 2 && isHelp(args[1]) {
			_, err := fmt.Fprintln(os.Stdout, "Usage: linkding mcp\nRun the four Linkding MCP tools over stdio. Requires LINKDING_URL and LINKDING_TOKEN.")
			return err
		}
		if len(args) != 1 {
			return usageError("mcp takes no arguments; run linkding mcp --help")
		}
		return runMCP(ctx)
	case "skills":
		return runSkills(args[1:])
	case "schema":
		return runSchema(ctx, args[1:])
	case "bookmarks", "tags":
		return runOperation(ctx, args)
	default:
		return usageError("unknown command; run linkding --help")
	}
}

func isHelp(arg string) bool { return arg == "--help" || arg == "-h" }

func runSkills(args []string) error {
	if len(args) == 0 || (len(args) == 1 && args[0] == "list") {
		_, err := fmt.Fprintln(os.Stdout, "core  Search, read, and save Linkding bookmarks with this CLI version")
		return err
	}
	if len(args) == 1 && isHelp(args[0]) {
		_, err := fmt.Fprintln(os.Stdout, "Usage: linkding skills list\n       linkding skills get core")
		return err
	}
	if len(args) == 2 && args[0] == "get" && args[1] == "core" {
		_, err := fmt.Fprint(os.Stdout, coreSkill)
		return err
	}
	return usageError("unknown skill command; run linkding skills --help")
}

func runOperation(ctx context.Context, args []string) error {
	if len(args) < 2 || isHelp(args[1]) {
		_, err := fmt.Fprint(os.Stdout, rootHelp)
		return err
	}
	path := strings.Join(args[:2], " ")
	usage := ""
	switch path {
	case "bookmarks list":
		usage = "Usage: linkding bookmarks list [--query QUERY] [--offset N] [--archived] [--json]\nSearch active bookmarks, or only the archive with --archived. Returns up to 20 items; count is the total."
	case "tags list":
		usage = "Usage: linkding tags list [--offset N] [--json]\nList up to 20 tags. Use offset for further pages."
	case "bookmarks get":
		usage = "Usage: linkding bookmarks get ID [--json]\nRead one bookmark, including stored notes. ID must be positive."
	case "bookmarks save":
		usage = "Usage: linkding bookmarks save --input - [--json]\nRead exactly one JSON object from stdin: url, optional title, description, notes, tag_names.\nOmitted/null fields preserve values; empty strings or [] clear fields. tag_names replaces the complete tag list.\nInterrupted writes are not retried; check the bookmark before trying again."
	default:
		return usageError("unknown operation; run linkding --help")
	}
	flags := flag.NewFlagSet(path, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	asJSON := flags.Bool("json", false, "output JSON")
	var help bool
	flags.BoolVar(&help, "help", false, "show help")
	flags.BoolVar(&help, "h", false, "show help")
	var search searchInput
	var tags tagsInput
	var detail bookmarkInput
	var save saveInput
	var inputFile string
	var id string
	remaining := args[2:]
	switch path {
	case "bookmarks list":
		flags.StringVar(&search.Query, "query", "", "Linkding search query")
		flags.Int64Var(&search.Offset, "offset", 0, "first result index")
		flags.BoolVar(&search.Archived, "archived", false, "search only the archive")
	case "tags list":
		flags.Int64Var(&tags.Offset, "offset", 0, "first result index")
	case "bookmarks get":
		// Accept the documented ID-before-flags form as well as flags-before-ID.
		if len(remaining) > 0 && !strings.HasPrefix(remaining[0], "-") {
			id, remaining = remaining[0], remaining[1:]
		}
	case "bookmarks save":
		flags.StringVar(&inputFile, "input", "", "use - for JSON stdin")
	}
	if err := parseFlags(flags, remaining); err != nil {
		return err
	}
	if help {
		_, err := fmt.Fprintln(os.Stdout, usage)
		return err
	}
	if path == "bookmarks get" && id == "" && flags.NArg() == 1 {
		id = flags.Arg(0)
	} else if flags.NArg() != 0 {
		return usageError("unexpected argument; run linkding " + path + " --help")
	}
	switch path {
	case "bookmarks list":
		if search.Offset < 0 {
			return usageError("offset must be nonnegative")
		}
	case "tags list":
		if tags.Offset < 0 {
			return usageError("offset must be nonnegative")
		}
	case "bookmarks get":
		value, err := strconv.ParseInt(id, 10, 64)
		if err != nil || value <= 0 {
			return usageError("id must be a positive integer")
		}
		detail.ID = value
	case "bookmarks save":
		if inputFile != "-" {
			return usageError("save requires --input - for JSON stdin")
		}
		type inputResult struct {
			input saveInput
			err   error
		}
		result := make(chan inputResult, 1)
		// Inherited fd 0 can stay blocked even after Close. Cancellation exits this
		// one-shot CLI process; its teardown also terminates the blocked reader.
		go func() {
			input, err := readSaveInput(os.Stdin)
			result <- inputResult{input: input, err: err}
		}()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case parsed := <-result:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if parsed.err != nil {
				return parsed.err
			}
			save = parsed.input
		}
	}
	c, err := newLinkdingClient(os.Getenv("LINKDING_URL"), os.Getenv("LINKDING_TOKEN"))
	if err != nil {
		return err
	}
	defer c.http.CloseIdleConnections()
	var result any
	switch path {
	case "bookmarks list":
		result, err = c.listBookmarks(ctx, search)
	case "tags list":
		result, err = c.listTags(ctx, tags)
	case "bookmarks get":
		result, err = c.getBookmark(ctx, detail)
	case "bookmarks save":
		result, err = c.saveBookmark(ctx, save)
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	if !*asJSON {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(result)
}

func parseFlags(flags *flag.FlagSet, args []string) error {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			break
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		f := flags.Lookup(name)
		if f == nil || seen[name] {
			return usageError("unknown or repeated flag; run linkding " + flags.Name() + " --help")
		}
		seen[name] = true
		isBool, ok := f.Value.(interface{ IsBoolFlag() bool })
		if !hasValue && (!ok || !isBool.IsBoolFlag()) {
			i++
		}
	}
	if err := flags.Parse(args); err != nil {
		return usageError("invalid arguments; run linkding " + flags.Name() + " --help")
	}
	return nil
}

func readSaveInput(reader io.Reader) (saveInput, error) {
	var input saveInput
	data, err := io.ReadAll(io.LimitReader(reader, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return input, usageError("could not read save input or input exceeds 2 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return input, usageError("save input must be one JSON object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return input, usageError("invalid save input JSON")
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return input, usageError("invalid or repeated save field")
		}
		switch name {
		case "url", "title", "description", "notes", "tag_names":
		default:
			return input, usageError("unknown save field; run linkding bookmarks save --help")
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return input, usageError("invalid save input JSON")
		}
		if name == "tag_names" {
			// encoding/json otherwise converts null string elements into empty strings.
			var names []*string
			if err := json.Unmarshal(value, &names); err != nil {
				return input, usageError("tag_names must be null or an array of strings")
			}
			for _, name := range names {
				if name == nil {
					return input, usageError("tag_names items must be strings")
				}
			}
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return input, usageError("invalid save input JSON")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return input, usageError("save input must contain exactly one JSON object")
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return input, usageError("invalid save field type; run linkding schema bookmarks save --json")
	}
	if _, err := parseHTTPURL(input.URL); err != nil {
		return input, usageError("url must be an absolute http or https URL")
	}
	return input, nil
}
