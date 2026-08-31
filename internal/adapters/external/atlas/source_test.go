package atlas

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type catalogArgs struct {
	Domain string `json:"domain"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
	System string `json:"system"`
}

type documentArgs struct {
	DocumentID string `json:"document_id"`
	SourceID   string `json:"source_id,omitempty"`
	System     string `json:"system"`
}

func TestSourceEnumeratesAndFetchesWithoutChangingIdentity(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "atlas-test", Version: "1"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: atlasCatalogTool}, func(_ context.Context, _ *sdk.CallToolRequest, args catalogArgs) (*sdk.CallToolResult, any, error) {
		if args.Domain != atlasCatalogDomain || args.System != "shared-docs" || args.Offset != 5 || args.Limit != 2 {
			t.Fatalf("unexpected catalog args: %+v", args)
		}
		return nil, map[string]any{
			"domain": atlasCatalogDomain, "total": 6, "limit": 2, "offset": 5,
			"items": []map[string]any{validDocument("doc-6", "source-1", "body omitted from catalog")},
		}, nil
	})
	sdk.AddTool(server, &sdk.Tool{Name: atlasDocumentTool}, func(_ context.Context, _ *sdk.CallToolRequest, args documentArgs) (*sdk.CallToolResult, any, error) {
		if args.DocumentID != "doc-6" || args.SourceID != "source-1" || args.System != "shared-docs" {
			t.Fatalf("unexpected document args: %+v", args)
		}
		return nil, map[string]any{"source_document": validDocument("doc-6", "source-1", "authoritative body")}, nil
	})

	endpoint := serveMCP(t, server, "service-token")
	source, err := New(Config{Endpoint: endpoint, System: "shared-docs", Token: "service-token", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	page, err := source.ListDocuments(context.Background(), 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 6 || len(page.Items) != 1 || page.Items[0].DocumentID != "doc-6" || page.Items[0].Revision != "sha256:doc-6" {
		t.Fatalf("unexpected page: %+v", page)
	}
	if page.Items[0].Title == "" || page.Items[0].SourceRef == "" {
		t.Fatalf("enumeration must retain user-facing provenance: %+v", page.Items[0])
	}

	doc, err := source.GetDocument(context.Background(), "doc-6", "source-1")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Content != "authoritative body" || doc.DocumentID != page.Items[0].DocumentID || doc.Revision != page.Items[0].Revision {
		t.Fatalf("unexpected document: %+v", doc)
	}
}

func TestSourceAcceptsAtlasJSONTextResults(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "atlas-test", Version: "1"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: atlasCatalogTool}, func(context.Context, *sdk.CallToolRequest, catalogArgs) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: `{"domain":"source_documents","items":[],"total":0,"limit":1,"offset":0}`}}}, nil, nil
	})
	source := mustSource(t, serveMCP(t, server, ""), Config{System: "shared-docs"})
	page, err := source.ListDocuments(context.Background(), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("unexpected page: %+v", page)
	}
}

func TestSourceRejectsBoundaryAndContractViolations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{name: "missing stable id", mutate: func(d map[string]any) { d["item_id"] = "" }, want: "metadata is incomplete"},
		{name: "wrong system", mutate: func(d map[string]any) { d["system"] = "private-docs" }, want: "system boundary"},
		{name: "invalid source ref", mutate: func(d map[string]any) { d["source_ref"] = "/relative" }, want: "source reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := sdk.NewServer(&sdk.Implementation{Name: "atlas-test", Version: "1"}, nil)
			sdk.AddTool(server, &sdk.Tool{Name: atlasCatalogTool}, func(context.Context, *sdk.CallToolRequest, catalogArgs) (*sdk.CallToolResult, any, error) {
				doc := validDocument("doc-1", "source-1", "")
				tc.mutate(doc)
				return nil, map[string]any{"domain": atlasCatalogDomain, "total": 1, "limit": 1, "offset": 0, "items": []map[string]any{doc}}, nil
			})
			source := mustSource(t, serveMCP(t, server, ""), Config{System: "shared-docs"})
			_, err := source.ListDocuments(context.Background(), 0, 1)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestSourceRejectsOversizedBodyAndMismatchedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  map[string]any
		want string
	}{
		{name: "oversized", doc: validDocument("doc-1", "source-1", strings.Repeat("x", 2048)), want: "body exceeded"},
		{name: "identity mismatch", doc: validDocument("doc-other", "source-1", "body"), want: "identity did not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := sdk.NewServer(&sdk.Implementation{Name: "atlas-test", Version: "1"}, nil)
			sdk.AddTool(server, &sdk.Tool{Name: atlasDocumentTool}, func(context.Context, *sdk.CallToolRequest, documentArgs) (*sdk.CallToolResult, any, error) {
				return nil, map[string]any{"source_document": tc.doc}, nil
			})
			source := mustSource(t, serveMCP(t, server, ""), Config{System: "shared-docs", MaxBodyBytes: 1024})
			_, err := source.GetDocument(context.Background(), "doc-1", "source-1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestNewRejectsUnsafeOrUnboundedConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{name: "relative endpoint", cfg: Config{Endpoint: "/mcp", System: "docs"}},
		{name: "credential in endpoint", cfg: Config{Endpoint: "https://user:pass@atlas.example/mcp", System: "docs"}},
		{name: "missing system", cfg: Config{Endpoint: "https://atlas.example/mcp"}},
		{name: "tiny wire limit", cfg: Config{Endpoint: "https://atlas.example/mcp", System: "docs", MaxWireBytes: 1024}},
		{name: "body exceeds wire", cfg: Config{Endpoint: "https://atlas.example/mcp", System: "docs", MaxWireBytes: 64 << 10, MaxBodyBytes: 128 << 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("invalid config must fail")
			}
		})
	}
}

func TestServiceCredentialNeverFollowsRedirect(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		if r.Header.Get("Authorization") != "" {
			t.Error("service credential leaked to redirect target")
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	source, err := New(Config{Endpoint: redirect.URL, System: "docs", Token: "service-token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.ListDocuments(context.Background(), 0, 1); err == nil {
		t.Fatal("redirected MCP endpoint must fail closed")
	}
	if redirected {
		t.Fatal("client followed a redirect despite a service credential")
	}
}

func validDocument(documentID, sourceID, content string) map[string]any {
	return map[string]any{
		"source_id": sourceID, "item_id": documentID, "item_kind": "yuque_document",
		"system": "shared-docs", "title": "Runbook", "source_ref": "https://docs.example/" + documentID,
		"content": content, "content_hash": "sha256:" + documentID, "updated_at": "2026-08-29T00:00:00Z",
		"source_type": "yuque", "captured_at": "2026-08-29T00:01:00Z",
	}
}

func serveMCP(t *testing.T, server *sdk.Server, token string) string {
	t.Helper()
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{JSONResponse: true})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	return httpServer.URL
}

func mustSource(t *testing.T, endpoint string, cfg Config) *Source {
	t.Helper()
	cfg.Endpoint = endpoint
	source, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return source
}
