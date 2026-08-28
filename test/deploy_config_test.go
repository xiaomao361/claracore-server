package test

import (
	"os"
	"strings"
	"testing"
)

// TestMCPReverseProxyKeepsLoopbackHost prevents the deployed reverse proxy from
// tripping the MCP SDK's DNS-rebinding protection. The application listens on
// loopback, so forwarding the public Host header makes every authenticated MCP
// POST fail with 403 before JSON-RPC is reached.
func TestMCPReverseProxyKeepsLoopbackHost(t *testing.T) {
	for _, path := range []string{
		"../deploy/claracore-server.nginx.conf",
		"../deploy/nginx.conf.example",
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s: %v", path, err)
		}
		text := string(body)
		start := strings.Index(text, "location ")
		if start < 0 || !strings.Contains(text[start:], "/mcp") {
			t.Fatalf("%s 缺少 /mcp location", path)
		}
		mcp := text[start:]
		if next := strings.Index(mcp[1:], "location "); next >= 0 {
			mcp = mcp[:next+1]
		}
		if !strings.Contains(mcp, "proxy_set_header Host 127.0.0.1;") {
			t.Fatalf("%s 的 /mcp 必须把上游 Host 固定为 loopback", path)
		}
	}
}
