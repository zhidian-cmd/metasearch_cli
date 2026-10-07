package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 用 httptest 假端点走完整 Search 路径：请求形态（GET + query 参数 + X-API-Key）
// 与响应映射（title/snippet/url/date/site_name）一次锁死。
func TestTinyfishSearch(t *testing.T) {
	var gotPath, gotQuery, gotKey, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("query")
		gotKey = r.Header.Get("X-API-Key")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"query":         gotQuery,
			"total_results": 2,
			"page":          0,
			"results": []map[string]any{
				{"position": 1, "site_name": "www.cas.cn", "title": " 可降解塑料：环境友好却存健康隐患 ",
					"snippet": "目前常见的可降解原材料有淀粉基塑料、PLA。", "url": "https://www.cas.cn/cm/202506/a.shtml",
					"date": "Jun 4, 2025"},
				{"position": 0, "site_name": "xinhuanet.com", "title": "禁塑令落地",
					"snippet": "", "url": "http://www.xinhuanet.com/a.html", "date": ""},
			},
		})
	}))
	defer srv.Close()

	old := tinyfishEndpoint
	tinyfishEndpoint = srv.URL
	defer func() { tinyfishEndpoint = old }()

	items, err := tinyfishProvider{}.Search(context.Background(), Options{
		Query:  "食品包装 可降解塑料",
		Limit:  15,
		APIKey: "sk-test",
	})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/" {
		t.Fatalf("应 GET 根路径，得 %s %s", gotMethod, gotPath)
	}
	if gotQuery != "食品包装 可降解塑料" {
		t.Fatalf("query 参数丢失: %q", gotQuery)
	}
	if gotKey != "sk-test" {
		t.Fatalf("X-API-Key 头丢失: %q", gotKey)
	}
	if len(items) != 2 {
		t.Fatalf("应 2 条，得 %d", len(items))
	}
	if items[0].Rank != 1 || items[0].Date != "Jun 4, 2025" {
		t.Fatalf("position/date 映射错: %+v", items[0])
	}
	if items[0].Signals == nil || items[0].Signals.Source != "www.cas.cn" {
		t.Fatalf("site_name 应进 Signals.Source: %+v", items[0].Signals)
	}
	// position<=0 的兜底口径：rank 顺延
	if items[1].Rank != 2 {
		t.Fatalf("position=0 应兜底为 len(out)+1=2，得 %d", items[1].Rank)
	}
	if strings.TrimSpace(items[1].Date) != "" {
		t.Fatalf("空 date 应保持空串")
	}
}

func TestTinyfishMissingKey(t *testing.T) {
	if _, err := (tinyfishProvider{}).Search(context.Background(), Options{Query: "x"}); err == nil {
		t.Fatal("无 key 应报错")
	}
	if _, err := (tinyfishProvider{}).Search(context.Background(), Options{Query: "  ", APIKey: "k"}); err != nil {
		t.Fatalf("空 query 应静默返回 nil，得 %v", err)
	}
}
