//go:build probe

// 临时取证探针（build tag "probe" 隔离，不属于正式构建、也不会进二进制）。
//
// 目的：把 8 个引擎的**原始响应体**各落盘一份，用于回答"能不能从引擎返回里解析
// 结果类型（type）"—— 这是个必须看真实响应才能回答的问题，照 API 文档猜是本项目
// 明确禁止的做法（见 docs/decisions.md 的"证据纪律"）。
//
// 放在 package provider 内是为了能直接调用内部的 do()/getClient()，保证发出的请求
// 与正式代码**同一套传输层**；载荷则照抄各 provider 的 Search()。
//
// 用法：go test -tags probe -run TestDumpRawBodies -v ./internal/provider/
// 输出：D:/App/Downloads/metasearch_cli/_rawdump/*.{json,html}
// 跑完即删，勿留仓库。
package provider

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhidian-cmd/metasearch_cli/internal/config"
	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

const rawDumpQuery = "跨境电商 政策"

// rawDumpQueryOf 取本次要打的关键词：默认用 rawDumpQuery，可用 RAWMAP_Q 覆盖
// （用于跨 query 复核"首页能不能凑满 limit"这类与 query 相关的事实）。
func rawDumpQueryOf() string {
	if v := os.Getenv("RAWMAP_Q"); v != "" {
		return v
	}
	return rawDumpQuery
}

func dumpDir() string {
	if v := os.Getenv("RAWMAP_DIR"); v != "" {
		return v
	}
	return `D:/App/Downloads/metasearch_cli/_rawdump`
}

func save(t *testing.T, name, ext string, raw []byte) {
	t.Helper()
	p := filepath.Join(dumpDir(), name+"."+ext)
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Errorf("%s: 落盘失败: %v", name, err)
		return
	}
	t.Logf("  ✔ %-10s %8d bytes", name, len(raw))
}

// TestDumpRawBodies 逐个引擎实发一次请求并把原始响应体落盘。
//
// 每个引擎独立成子测试：某个引擎失败（无 key / 被限流）不影响其余取证。
func TestDumpRawBodies(t *testing.T) {
	if err := os.MkdirAll(dumpDir(), 0o755); err != nil {
		t.Fatalf("建输出目录失败: %v", err)
	}
	keys, err := config.LoadAPIKeys("")
	if err != nil {
		t.Fatalf("加载密钥失败: %v", err)
	}
	q := rawDumpQueryOf()
	ctx := context.Background()

	mk := func(name string) Options {
		return Options{
			Query:   q,
			Limit:   config.DefaultLimit,
			APIKey:  keys.Get(name),
			Timeout: 40 * time.Second,
		}
	}

	// ---------- 付费/API 引擎：6 个 ----------
	steps := []struct {
		name string
		run  func() ([]byte, error)
	}{
		{"serpapi", func() ([]byte, error) {
			opt := mk("serpapi")
			num := config.DefaultLimit
			if v := os.Getenv("RAWMAP_NUM"); v != "" {
				if n, e := strconv.Atoi(v); e == nil {
					num = n
				}
			}
			params := url.Values{
				"engine":  {"google"},
				"q":       {q},
				"num":     {strconv.Itoa(num)},
				"gl":      {"cn"},
				"hl":      {"zh-cn"},
				"api_key": {opt.APIKey},
			}
			return do(ctx, opt, "SerpApi", "GET", serpapiEndpoint, params, nil, nil)
		}},
		{"tavily", func() ([]byte, error) {
			opt := mk("tavily")
			return do(ctx, opt, "Tavily", "POST", tavilyEndpoint, nil,
				map[string]string{"Authorization": "Bearer " + opt.APIKey},
				map[string]any{"query": q, "max_results": trimLimit(opt.Limit, 20)})
		}},
		{"exa", func() ([]byte, error) {
			opt := mk("exa")
			return do(ctx, opt, "Exa", "POST", exaEndpoint, nil,
				map[string]string{"x-api-key": opt.APIKey},
				map[string]any{
					"query":      q,
					"numResults": trimLimit(opt.Limit, 100),
					"contents":   map[string]any{"text": map[string]any{"maxCharacters": exaMaxChars}},
				})
		}},
		{"qianfan", func() ([]byte, error) {
			opt := mk("qianfan")
			return do(ctx, opt, "Qianfan", "POST", qianfanEndpoint, nil,
				map[string]string{"Authorization": "Bearer " + opt.APIKey},
				map[string]any{
					"messages":             []map[string]any{{"content": q, "role": "user"}},
					"resource_type_filter": []map[string]any{{"type": "web", "top_k": trimLimit(opt.Limit, qianfanTopKMax)}},
					"edition":              "standard",
				})
		}},
		{"anysearch", func() ([]byte, error) {
			opt := mk("anysearch")
			return do(ctx, opt, "AnySearch", "POST", anysearchBase()+anysearchSearchPath,
				nil, anysearchHeaders(opt.APIKey),
				map[string]any{"query": q, "max_results": trimLimit(opt.Limit, 10)})
		}},
		{"metaso", func() ([]byte, error) {
			opt := mk("metaso")
			return do(ctx, opt, "Metaso", "POST", metasoEndpoint, nil,
				map[string]string{"Authorization": "Bearer " + opt.APIKey},
				map[string]any{
					"q": q, "scope": "webpage", "size": strconv.Itoa(opt.Limit),
					"includeSummary": false, "includeRawContent": false, "conciseSnippet": false,
				})
		}},
	}

	for _, s := range steps {
		s := s
		t.Run(s.name, func(t *testing.T) {
			raw, err := s.run()
			if err != nil {
				t.Logf("  ✗ %-10s 失败: %v", s.name, err)
				return
			}
			save(t, s.name, "json", raw)
		})
	}

	// ---------- 网页抓取引擎：2 个 ----------
	//
	// ⚠️ bing 正式代码走 scrapling-go 的 fetcher（带浏览器指纹），这里为省事用共享
	//    do() 发同样的 header。取证目标是"HTML 里有没有类型标记"，两者等价。
	t.Run("bing", func(t *testing.T) {
		h := map[string]string{
			"User-Agent":      bingUA,
			"Accept":          "text/html,application/xhtml+xml",
			"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
			"Accept-Encoding": "identity",
			"Cookie":          "_EDGE_V=1",
		}
		raw, err := do(ctx, mk("bing"), "Bing", "GET", bingPageURL(q, 1), nil, h, nil)
		if err != nil {
			t.Logf("  ✗ bing 失败: %v", err)
			return
		}
		save(t, "bing", "html", raw)
	})

	t.Run("quark", func(t *testing.T) {
		h := map[string]string{
			"User-Agent":      quarkUA,
			"Accept":          "text/html,application/xhtml+xml",
			"Accept-Language": "zh-CN,zh;q=0.9",
			"Accept-Encoding": "identity",
		}
		raw, err := do(ctx, mk("quark"), "Quark", "GET", quarkTarget(q), nil, h, nil)
		if err != nil {
			t.Logf("  ✗ quark 失败: %v", err)
			return
		}
		save(t, "quark", "html", raw)
	})
}

// TestEngineSearch 走**完整的 provider.Search**（含引擎内部翻页），统计"默认 -limit=15 下
// 某引擎实际能拿几条"。
//
// 与 TestDumpRawBodies 的区别：后者只发**一次**请求（单页），所以 serpapi 永远只看到 10 条；
// 要回答"serpapi 到底给 15 条还是 10 条"必须走完整 Search（首页 + start=10 翻页）。
//
// 关键价值：**只发指定引擎的请求** → 可在**不惊动 quark 反爬风控**的前提下做单引擎回归
// （改 quark 解析后要对比条数，但连跑全引擎会把自己打成惩罚页）。
//
// 用法：RAWMAP_ENGINE=serpapi go test -tags probe -run TestEngineSearch -v ./internal/provider/
//
//	RAWMAP_ENGINE=serpapi,qianfan（逗号分隔可多选）
func TestEngineSearch(t *testing.T) {
	keys, err := config.LoadAPIKeys("")
	if err != nil {
		t.Fatalf("加载密钥失败: %v", err)
	}
	var names []string
	if v := os.Getenv("RAWMAP_ENGINE"); v != "" {
		for _, n := range strings.Split(v, ",") {
			if n = strings.TrimSpace(n); n != "" {
				names = append(names, n)
			}
		}
	} else {
		names = []string{"serpapi"}
	}
	q := rawDumpQueryOf()
	limit := rawDumpLimit()

	for _, n := range names {
		n := n
		t.Run(n, func(t *testing.T) {
			p, ok := registry[n]
			if !ok {
				t.Fatalf("未注册的引擎: %s", n)
			}
			opt := Options{
				Query:   q,
				Limit:   limit,
				APIKey:  keys.Get(n),
				Timeout: 40 * time.Second,
			}
			t0 := time.Now()
			items, err := p.Search(context.Background(), opt)
			el := time.Since(t0)
			if err != nil {
				t.Logf("  ✗ %-10s 失败 (耗时 %v): %v", n, el, err)
				return
			}
			mark := ""
			if len(items) >= opt.Limit {
				mark = "  ← 凑满"
			}
			t.Logf("  ✔ %-10s count=%-3d / limit=%d  耗时=%v%s", n, len(items), opt.Limit, el, mark)
			for i, it := range items {
				if i >= 3 {
					break
				}
				t.Logf("       [%d] %s | %s", i+1, clip(it.Title, 36), clip(it.URL, 72))
			}
			dumpItems(t, n, q, items)
		})
	}
}

// rawDumpLimit 本次每引擎请求条数：默认 config.DefaultLimit，可用 RAWMAP_LIMIT 覆盖。
//
// ⚠️ **只有探针能取到 >15**：正式 CLI 的聚合层把"每引擎入池"钉死在 15（`-limit` 调大也不放宽，
// 见 aggregate.go），所以"某引擎到底能给 20 条吗"这类问题必须绕过聚合层、直接调 provider.Search。
func rawDumpLimit() int {
	if v := strings.TrimSpace(os.Getenv("RAWMAP_LIMIT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return config.DefaultLimit
}

// rawDumpItemsPath 逐条结果的落盘位置：RAWMAP_ITEMS 优先，默认 <dumpDir>/items.jsonl。
func rawDumpItemsPath() string {
	if v := strings.TrimSpace(os.Getenv("RAWMAP_ITEMS")); v != "" {
		return v
	}
	return filepath.Join(dumpDir(), "items.jsonl")
}

// dumpItems 把本次每条结果**逐条追加**到 JSONL —— 供离线判质量（广告占比 / 跑题占比这类
// 需要人（或模型）读文本的问题）。字段取 title/url/snippet 原文，不做截断。
func dumpItems(t *testing.T, engine, query string, items []model.RawItem) {
	t.Helper()
	f, err := os.OpenFile(rawDumpItemsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Logf("  ✗ 写 items 失败: %v", err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for i, it := range items {
		_ = enc.Encode(map[string]any{
			"engine": engine, "query": query, "rank": i + 1,
			"title": it.Title, "url": it.URL, "snippet": it.Snippet,
			"date": it.Date, "signals": it.Signals,
		})
	}
}

// clip 截断长文本用于日志输出（仅探针用）。
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
