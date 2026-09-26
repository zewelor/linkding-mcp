package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	pageSize         = 20
	maxResponseBytes = 2 * 1024 * 1024
)

type bookmark struct {
	ID          int64    `json:"id"`
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	TagNames    []string `json:"tag_names"`
}

type bookmarkDetails struct {
	bookmark
	Notes string `json:"notes"`
}

type bookmarksPage struct {
	Count   int64      `json:"count"`
	Results []bookmark `json:"results"`
}

type tag struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type tagsPage struct {
	Count   int64 `json:"count"`
	Results []tag `json:"results"`
}

type saveResponse struct {
	bookmark
	Title *string `json:"title"`
}

func (r *saveResponse) validate() error {
	if r.Title == nil {
		return errors.New("invalid bookmark response from linkding: missing title")
	}
	return validateBookmark(&r.bookmark)
}

type saveResult struct {
	Status string `json:"status"`
	ID     int64  `json:"id"`
	URL    string `json:"url"`
	Title  string `json:"title"`
}

type searchInput struct {
	Query  string `json:"query,omitempty" jsonschema:"Optional Linkding search phrase, including #tag syntax; omit or leave empty to list all bookmarks"`
	Offset int64  `json:"offset,omitempty" jsonschema:"Index of the first result; default 0"`
}

type tagsInput struct {
	Offset int64 `json:"offset,omitempty" jsonschema:"Index of the first result; default 0"`
}

type bookmarkInput struct {
	ID int64 `json:"id" jsonschema:"Positive Linkding bookmark ID"`
}

type saveInput struct {
	URL string `json:"url" jsonschema:"Absolute HTTP or HTTPS URL to save"`
}

type linkdingClient struct {
	base  *url.URL
	token string
	http  *http.Client
}

func newLinkdingClient(base, token string) (*linkdingClient, error) {
	u, err := parseHTTPURL(base)
	if err != nil {
		return nil, errors.New("linkding_url must be an absolute http or https URL")
	}
	hasQuery := u.RawQuery != "" || u.ForceQuery
	if u.User != nil || hasQuery || strings.Contains(base, "#") {
		return nil, errors.New("linkding_url must not contain credentials, query or fragment")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("linkding_token is required")
	}
	for _, char := range []byte(token) {
		if char < 0x20 || char == 0x7f {
			return nil, errors.New("linkding_token contains invalid HTTP header characters")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// The two configured values are sufficient; do not route API tokens via ambient proxies.
	transport.Proxy = nil
	// Fresh HTTP/1 connections prevent the transport from replaying failed requests.
	transport.DisableKeepAlives = true
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	// DefaultTransport.Clone may retain h2 in TLS ALPN despite Protocols above.
	transport.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	return &linkdingClient{
		base: u, token: token,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *linkdingClient) get(ctx context.Context, path string, query url.Values, output any) error {
	u := c.base.JoinPath("api", path)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.New("could not construct linkding request")
	}
	return c.do(req, output)
}

func (c *linkdingClient) do(req *http.Request, output any) error {
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return errors.New("linkding request canceled")
		case errors.Is(err, context.DeadlineExceeded):
			return errors.New("linkding request timed out")
		default:
			// Transport errors may contain URLs or header values. Do not expose them.
			return errors.New("linkding request failed")
		}
	}
	defer resp.Body.Close()
	expectedStatus := http.StatusOK
	if req.Method == http.MethodPost {
		expectedStatus = http.StatusCreated
	}
	if resp.StatusCode != expectedStatus {
		return fmt.Errorf("linkding http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return errors.New("could not read linkding response")
	}
	if len(body) > maxResponseBytes {
		return errors.New("linkding response exceeds 2 MiB")
	}
	if err := json.Unmarshal(body, output); err != nil {
		return errors.New("linkding returned invalid json")
	}
	return nil
}

func (c *linkdingClient) listBookmarks(ctx context.Context, input searchInput) (bookmarksPage, error) {
	if input.Offset < 0 {
		return bookmarksPage{}, errors.New("offset must be nonnegative")
	}
	query := pagination(input.Offset)
	if strings.TrimSpace(input.Query) != "" {
		query.Set("q", input.Query)
	}
	var response struct {
		Count   *int64     `json:"count"`
		Results []bookmark `json:"results"`
	}
	if err := c.get(ctx, "bookmarks/", query, &response); err != nil {
		return bookmarksPage{}, err
	}
	if response.Count == nil || response.Results == nil {
		return bookmarksPage{}, errors.New("invalid list response from linkding")
	}
	results := response.Results[:min(len(response.Results), pageSize)]
	for i := range results {
		if err := validateBookmark(&results[i]); err != nil {
			return bookmarksPage{}, err
		}
	}
	return bookmarksPage{Count: *response.Count, Results: results}, nil
}

func (c *linkdingClient) listTags(ctx context.Context, input tagsInput) (tagsPage, error) {
	if input.Offset < 0 {
		return tagsPage{}, errors.New("offset must be nonnegative")
	}
	var response struct {
		Count   *int64 `json:"count"`
		Results []tag  `json:"results"`
	}
	if err := c.get(ctx, "tags/", pagination(input.Offset), &response); err != nil {
		return tagsPage{}, err
	}
	if response.Count == nil || response.Results == nil {
		return tagsPage{}, errors.New("invalid list response from linkding")
	}
	return tagsPage{Count: *response.Count, Results: response.Results[:min(len(response.Results), pageSize)]}, nil
}

func (c *linkdingClient) getBookmark(ctx context.Context, input bookmarkInput) (bookmarkDetails, error) {
	if input.ID <= 0 {
		return bookmarkDetails{}, errors.New("id must be positive")
	}
	var result bookmarkDetails
	if err := c.get(ctx, "bookmarks/"+strconv.FormatInt(input.ID, 10)+"/", nil, &result); err != nil {
		return bookmarkDetails{}, err
	}
	if err := validateBookmark(&result.bookmark); err != nil {
		return bookmarkDetails{}, err
	}
	return result, nil
}

func (c *linkdingClient) saveBookmark(ctx context.Context, input saveInput) (saveResult, error) {
	if _, err := parseHTTPURL(input.URL); err != nil {
		return saveResult{}, errors.New("url must be an absolute http or https URL")
	}
	var check struct {
		Bookmark json.RawMessage `json:"bookmark"`
	}
	if err := c.get(ctx, "bookmarks/check/", url.Values{"url": {input.URL}}, &check); err != nil {
		return saveResult{}, err
	}
	if len(check.Bookmark) == 0 {
		return saveResult{}, errors.New("invalid check response from linkding")
	}
	if string(check.Bookmark) != "null" {
		var existing saveResponse
		if err := json.Unmarshal(check.Bookmark, &existing); err != nil {
			return saveResult{}, errors.New("invalid bookmark in check response")
		}
		if err := existing.validate(); err != nil {
			return saveResult{}, err
		}
		return saveResult{Status: "already_exists", ID: existing.ID, URL: existing.URL, Title: *existing.Title}, nil
	}
	body, err := json.Marshal(input)
	if err != nil {
		return saveResult{}, fmt.Errorf("encode bookmark: %w", err)
	}
	u := c.base.JoinPath("api", "bookmarks/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return saveResult{}, errors.New("could not construct bookmark save request")
	}
	req.Header.Set("Content-Type", "application/json")
	var saved saveResponse
	if err := c.do(req, &saved); err != nil {
		return saveResult{}, fmt.Errorf("save failed; outcome may be unknown: %w", err)
	}
	if err := saved.validate(); err != nil {
		return saveResult{}, fmt.Errorf("save failed; outcome may be unknown: %w", err)
	}
	return saveResult{Status: "created", ID: saved.ID, URL: saved.URL, Title: *saved.Title}, nil
}

func validateBookmark(b *bookmark) error {
	if b.ID <= 0 || b.URL == "" {
		return errors.New("invalid bookmark response from linkding")
	}
	if b.TagNames == nil {
		b.TagNames = []string{}
	}
	return nil
}

func pagination(offset int64) url.Values {
	return url.Values{"limit": {strconv.Itoa(pageSize)}, "offset": {strconv.FormatInt(offset, 10)}}
}

func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid url")
	}
	isHTTP := u.Scheme == "http" || u.Scheme == "https"
	if !isHTTP || u.Hostname() == "" {
		return nil, errors.New("invalid http url")
	}
	return u, nil
}

func run(ctx context.Context) error {
	c, err := newLinkdingClient(os.Getenv("LINKDING_URL"), os.Getenv("LINKDING_TOKEN"))
	if err != nil {
		return err
	}
	defer c.http.CloseIdleConnections()
	server := mcp.NewServer(&mcp.Implementation{Name: "linkding-mcp", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name: "list_bookmarks", Description: "Search Linkding bookmarks using its search syntax (including #tag). " +
			"Returns up to 20 results; use offset to fetch further pages.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input searchInput) (*mcp.CallToolResult, bookmarksPage, error) {
		out, err := c.listBookmarks(ctx, input)
		return nil, out, err
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: "list_tags", Description: "List up to 20 Linkding tags; use offset for further pages and #name in bookmark searches.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input tagsInput) (*mcp.CallToolResult, tagsPage, error) {
		out, err := c.listTags(ctx, input)
		return nil, out, err
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_bookmark", Description: "Retrieve a Linkding bookmark by ID, including its notes.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input bookmarkInput) (*mcp.CallToolResult, bookmarkDetails, error) {
		out, err := c.getBookmark(ctx, input)
		return nil, out, err
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: "save_bookmark", Description: "Save an HTTP/HTTPS URL using Linkding metadata and automatic tags. " +
			"Existing URLs are returned without POST. The check and save are not atomic: " +
			"a concurrent save can update an existing bookmark. An interrupted POST is never retried.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input saveInput) (*mcp.CallToolResult, saveResult, error) {
		out, err := c.saveBookmark(ctx, input)
		return nil, out, err
	})
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("mcp server: %w", err)
	}
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("linkding-mcp stopped", "error", err)
		os.Exit(1)
	}
}
