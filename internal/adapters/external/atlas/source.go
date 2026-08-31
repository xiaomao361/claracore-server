// Package atlas adapts Atlas's MCP document catalog to the external Source contract.
package atlas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"claracore-server/internal/domain/external"
)

const (
	defaultTimeout      = 30 * time.Second
	defaultMaxWireBytes = 8 << 20
	defaultMaxBodyBytes = 2 << 20
	maximumPageSize     = 500
	atlasCatalogTool    = "atlas_catalog"
	atlasDocumentTool   = "atlas_source_document"
	atlasCatalogDomain  = "source_documents"
)

// Config is deployment-owned. Token is held only in process memory and is never
// included in tool arguments or error messages.
type Config struct {
	Endpoint     string
	System       string
	Token        string
	Timeout      time.Duration
	MaxWireBytes int64
	MaxBodyBytes int
	HTTPClient   *http.Client
}

// Source is a read-only Atlas MCP client.
type Source struct {
	endpoint     string
	system       string
	httpClient   *http.Client
	maxBodyBytes int
}

var _ external.Source = (*Source)(nil)

func New(cfg Config) (*Source, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("atlas external source: endpoint must be a complete http(s) URL")
	}
	if strings.TrimSpace(cfg.System) == "" {
		return nil, errors.New("atlas external source: system is required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Timeout < time.Second || cfg.Timeout > 10*time.Minute {
		return nil, errors.New("atlas external source: timeout must be between 1s and 10m")
	}
	if cfg.MaxWireBytes == 0 {
		cfg.MaxWireBytes = defaultMaxWireBytes
	}
	if cfg.MaxWireBytes < 64<<10 || cfg.MaxWireBytes > 64<<20 {
		return nil, errors.New("atlas external source: wire limit must be between 64 KiB and 64 MiB")
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.MaxBodyBytes < 1024 || int64(cfg.MaxBodyBytes) > cfg.MaxWireBytes {
		return nil, errors.New("atlas external source: body limit must be at least 1 KiB and no larger than wire limit")
	}

	base := cfg.HTTPClient
	if base == nil {
		base = &http.Client{}
	}
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := *base
	client.Timeout = cfg.Timeout
	client.Transport = boundedTransport{base: transport, token: cfg.Token, maxBytes: cfg.MaxWireBytes}
	// A service credential must never follow an HTTP redirect to another host.
	// MCP endpoints are stable configuration, so redirects are treated as a
	// configuration error rather than silently replaying authenticated requests.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return &Source{
		endpoint: endpoint, system: strings.TrimSpace(cfg.System),
		httpClient: &client, maxBodyBytes: cfg.MaxBodyBytes,
	}, nil
}

func (s *Source) ListDocuments(ctx context.Context, offset, limit int) (external.Page, error) {
	if offset < 0 || limit < 1 || limit > maximumPageSize {
		return external.Page{}, fmt.Errorf("atlas external source: pagination requires offset >= 0 and limit 1..%d", maximumPageSize)
	}
	var payload struct {
		Domain string         `json:"domain"`
		Items  []documentWire `json:"items"`
		Total  int            `json:"total"`
		Limit  int            `json:"limit"`
		Offset int            `json:"offset"`
	}
	if err := s.call(ctx, atlasCatalogTool, map[string]any{
		"domain": atlasCatalogDomain, "limit": limit, "offset": offset, "system": s.system,
	}, &payload); err != nil {
		return external.Page{}, err
	}
	if payload.Domain != atlasCatalogDomain || payload.Total < 0 || payload.Limit != limit || payload.Offset != offset || len(payload.Items) > limit {
		return external.Page{}, errors.New("atlas external source: catalog returned an inconsistent page")
	}
	page := external.Page{Total: payload.Total, Limit: payload.Limit, Offset: payload.Offset, Items: make([]external.DocumentMeta, 0, len(payload.Items))}
	for _, item := range payload.Items {
		meta, err := item.meta(s.system)
		if err != nil {
			return external.Page{}, err
		}
		page.Items = append(page.Items, meta)
	}
	return page, nil
}

func (s *Source) GetDocument(ctx context.Context, documentID, sourceID string) (external.Document, error) {
	documentID, sourceID = strings.TrimSpace(documentID), strings.TrimSpace(sourceID)
	if documentID == "" {
		return external.Document{}, errors.New("atlas external source: document ID is required")
	}
	args := map[string]any{"document_id": documentID, "system": s.system}
	if sourceID != "" {
		args["source_id"] = sourceID
	}
	var payload struct {
		Document documentWire `json:"source_document"`
	}
	if err := s.call(ctx, atlasDocumentTool, args, &payload); err != nil {
		return external.Document{}, err
	}
	meta, err := payload.Document.meta(s.system)
	if err != nil {
		return external.Document{}, err
	}
	if meta.DocumentID != documentID || (sourceID != "" && meta.SourceID != sourceID) {
		return external.Document{}, errors.New("atlas external source: document identity did not match the request")
	}
	if len(payload.Document.Content) > s.maxBodyBytes {
		return external.Document{}, errors.New("atlas external source: document body exceeded the configured limit")
	}
	return external.Document{
		DocumentMeta: meta, Content: payload.Document.Content,
		SourceType: payload.Document.SourceType, CapturedAt: payload.Document.CapturedAt,
	}, nil
}

func (s *Source) call(ctx context.Context, tool string, args map[string]any, out any) error {
	client := sdk.NewClient(&sdk.Implementation{Name: "claracore-server", Version: "external-source-v1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint: s.endpoint, HTTPClient: s.httpClient, MaxRetries: -1, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return errors.New("atlas external source: service unavailable or MCP handshake failed")
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return errors.New("atlas external source: MCP tool call failed")
	}
	if result.IsError {
		return errors.New("atlas external source: MCP tool reported an error")
	}
	if result.StructuredContent != nil {
		encoded, err := json.Marshal(result.StructuredContent)
		if err == nil && json.Unmarshal(encoded, out) == nil {
			return nil
		}
		return errors.New("atlas external source: structured result was invalid")
	}
	var text string
	for _, content := range result.Content {
		if item, ok := content.(*sdk.TextContent); ok {
			if text != "" {
				return errors.New("atlas external source: tool returned multiple text results")
			}
			text = item.Text
		}
	}
	if text == "" || json.Unmarshal([]byte(text), out) != nil {
		return errors.New("atlas external source: tool result was not valid JSON")
	}
	return nil
}

type documentWire struct {
	SourceID    string `json:"source_id"`
	ItemID      string `json:"item_id"`
	ItemKind    string `json:"item_kind"`
	System      string `json:"system"`
	Title       string `json:"title"`
	SourceRef   string `json:"source_ref"`
	Content     string `json:"content"`
	ContentHash string `json:"content_hash"`
	UpdatedAt   string `json:"updated_at"`
	SourceType  string `json:"source_type"`
	CapturedAt  string `json:"captured_at"`
}

func (w documentWire) meta(expectedSystem string) (external.DocumentMeta, error) {
	fields := []string{w.SourceID, w.ItemID, w.ItemKind, w.System, w.Title, w.SourceRef, w.ContentHash}
	for _, field := range fields {
		if strings.TrimSpace(field) == "" {
			return external.DocumentMeta{}, errors.New("atlas external source: document metadata is incomplete")
		}
	}
	if w.System != expectedSystem {
		return external.DocumentMeta{}, errors.New("atlas external source: document escaped the configured system boundary")
	}
	ref, err := url.Parse(w.SourceRef)
	if err != nil || ref.Host == "" || ref.User != nil || (ref.Scheme != "http" && ref.Scheme != "https") {
		return external.DocumentMeta{}, errors.New("atlas external source: document source reference is not a complete http(s) URL")
	}
	return external.DocumentMeta{
		SourceID: w.SourceID, DocumentID: w.ItemID, Kind: w.ItemKind, System: w.System,
		Title: w.Title, SourceRef: w.SourceRef, Revision: w.ContentHash, UpdatedAt: w.UpdatedAt,
	}, nil
}

type boundedTransport struct {
	base     http.RoundTripper
	token    string
	maxBytes int64
}

func (t boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	if t.token != "" {
		clone.Header.Set("Authorization", "Bearer "+t.token)
	}
	resp, err := t.base.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > t.maxBytes {
		resp.Body.Close()
		return nil, errors.New("external source response exceeded configured wire limit")
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.LimitReader(resp.Body, t.maxBytes+1), Closer: resp.Body}
	return resp, nil
}
