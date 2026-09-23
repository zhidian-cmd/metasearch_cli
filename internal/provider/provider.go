// Package provider 定义检索引擎的统一接口、注册表与 HTTP 公共通道。
//
// 移植自 Python 参考实现 search_core/providers/：
// 每个引擎一个文件，统一以 Search(ctx, Options) 返回 []model.RawItem；
// 单引擎失败只影响自己（由聚合层降级处理），不向上抛致命错误。
package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

// Options 传给单个 provider 的调用参数。
type Options struct {
	Query    string
	Limit    int
	APIKey   string
	Timeout  time.Duration
	Insecure bool   // 跳过 TLS 校验（参考实现 verify_ssl=False，默认 true 即跳过）
	Proxy    string // 显式代理；空串则走环境变量 HTTP_PROXY/HTTPS_PROXY

	// AnySearchVertical 是否启用 AnySearch 的垂直领域自动解析
	// （query 命中 domain → 拉 sub-domains 目录 → 选 sub_domain → 带 tag 检索）。
	// 关闭则一律走通用搜索。**当前只有 anysearch 读它**（由 `-anysearch-vertical` 透传）。
	AnySearchVertical bool

	// MaxPages 内部翻页的最大页数（含首页）。0 = 用引擎自己的默认值。
	//
	// ⚠️ **只有 quark 读它**（quark.go 是唯一读它的 provider）：
	//   - bing   ：按 limit 自算页数（`(limit+9)/10` 封顶 5），不读本字段；
	//   - serpapi：读自己的常量 `serpapiMaxPages`（见 serpapi.go），不读本字段。
	//
	// （此字段曾以"将来别的引擎可能也要这个旋钮"为由辩护 —— 那不是本仓库认的理由
	//   （见 docs/decisions.md 第六节）。留下它只是因为 `-max-pages` 这个真实开关
	//   需要一条从命令行到 provider 的通路。）
	MaxPages int

	// Logf 引擎级日志回调（nil 则静默）。
	Logf func(format string, args ...any)
}

// defaultTimeout 未显式指定时的单次请求超时。
const defaultTimeout = 20 * time.Second

// proxyFor 解析该 URL 应使用的代理：显式 -proxy 优先，否则按环境变量
// （HTTP_PROXY/HTTPS_PROXY/NO_PROXY）判定。返回空串表示不走代理。
//
// 单独实现是因为 scrapling-go 的 fetcher 只认显式 proxyURL，
// 不会自动读环境变量 —— 不补这一步，bing 就没法跟着系统代理走。
func proxyFor(rawURL string, opt Options) string {
	if p := strings.TrimSpace(opt.Proxy); p != "" {
		return p
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return ""
	}
	u, err := http.ProxyFromEnvironment(req)
	if err != nil || u == nil {
		return ""
	}
	return u.String()
}

// Provider 检索引擎接口。
type Provider interface {
	// Name 引擎标识，同时是输出 engine 字段与 positions 的键
	Name() string
	// Search 执行检索；返回的 RawItem 里 Engine/Rank 由本包统一补齐
	Search(ctx context.Context, opt Options) ([]model.RawItem, error)
}

var registry = map[string]Provider{}

// Register 注册一个 provider（各 provider 文件在 init 中调用）。
func Register(p Provider) { registry[p.Name()] = p }

// Get 按名取 provider。
func Get(name string) (Provider, bool) {
	p, ok := registry[strings.ToLower(strings.TrimSpace(name))]
	return p, ok
}

// ========== HTTP 公共通道 ==========

// newClient 构造带超时与代理设置的 HTTP 客户端。
//
// 超时策略（可层层兜底）：
//  1. Transport 层：Dial / TLS 握手 / 响应头各自的硬超时；
//  2. 单次调用：由聚合层下发的 ctx deadline 控制（等价参考实现的 asyncio.wait_for 45s）。
//
// 不设 http.Client.Timeout —— 否则长响应体读取会被整体截断，
// 且 ctx 已经提供了更精确的取消能力。
func newClient(opt Options) *http.Client {
	// Dial 超时 5s：本机可达域名实测握手 ≤287ms，5s 已有 17 倍余量；
	// 对不可达域名不必浪费 10s——真正的硬上限是调用方的 ctx（单引擎 20s）。
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         dialer.DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// 不设 ResponseHeaderTimeout：单引擎超时 20s 由 ctx 兜底，
		// 这里再设一个更大的值永远轮不到生效，只会误导读者。
		TLSClientConfig: &tls.Config{InsecureSkipVerify: opt.Insecure}, //nolint:gosec // 与参考实现 verify=False 对齐
	}
	if p := strings.TrimSpace(opt.Proxy); p != "" {
		if u, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr}
}

// ========== 连接复用 ==========

// clientCache 按「代理 + 是否跳过 TLS 校验」缓存 HTTP 客户端，全局共享。
//
// 为什么必须缓存：若 do() 每次请求都新建 Transport，同一引擎的连续请求
// （serpapi 翻 3 页、anysearch 拉目录+搜索）会各自重新走一遍 TCP + TLS，
// 每次 100~300ms；翻页页数越多浪费越明显。共享 Transport 后 TCP 连接与
// TLS 会话都能复用。
//
// ⚠️ 翻页本身是**串行**的（2026-09-16 实测：并发翻页会触发引擎限流，
// 实际耗时变成 max(各页) + 限流惩罚，比串行更慢），故这里的收益纯粹来自
// 连接复用，不来自并发。
//
// 并发安全：http.Client 本身支持并发使用；缓存表用互斥锁保护。
//
// ⚠️ 调用方**不要**再对取回的 client 调 CloseIdleConnections()——那会把
// 复用中的空闲连接全关掉，等于白缓存。
var (
	clientMu    sync.Mutex
	clientCache = map[string]*http.Client{}
)

// getClient 取（或建）与 opt 匹配的共享客户端。
func getClient(opt Options) *http.Client {
	key := opt.Proxy + "\x00" + strconv.FormatBool(opt.Insecure)
	clientMu.Lock()
	defer clientMu.Unlock()
	if c, ok := clientCache[key]; ok {
		return c
	}
	c := newClient(opt)
	clientCache[key] = c
	return c
}

// HTTPError 携带状态码与响应体，便于归因（对应参考实现 _handle_http_err）。
type HTTPError struct {
	Provider string
	Status   int
	Reason   string
	Body     string
}

func (e *HTTPError) Error() string {
	msg := fmt.Sprintf("%s: HTTP %d %s", e.Provider, e.Status, e.Reason)
	if e.Body != "" {
		msg += " body=" + e.Body
	}
	return msg
}

// bodySnippet 截断响应体用于报错，避免日志里塞进整页 HTML。
func bodySnippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func do(ctx context.Context, opt Options, provider, method, endpoint string, params url.Values, headers map[string]string, jsonBody any) ([]byte, error) {
	if params != nil && len(params) > 0 {
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("%s: 非法端点 %q: %w", provider, endpoint, err)
		}
		q := u.Query()
		for k, vs := range params {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		u.RawQuery = q.Encode()
		endpoint = u.String()
	}

	var bodyReader io.Reader
	if jsonBody != nil {
		buf, err := json.Marshal(jsonBody)
		if err != nil {
			return nil, fmt.Errorf("%s: 请求体序列化失败: %w", provider, err)
		}
		bodyReader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("%s: 构造请求失败: %w", provider, err)
	}
	if jsonBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// 复用共享客户端（见 getClient）：翻页/多次调用共用一个 Transport，
	// 省掉重复的 TCP + TLS 握手。故这里**不能**再 CloseIdleConnections。
	client := getClient(opt)

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: 超时/取消: %w", provider, ctx.Err())
		}
		return nil, fmt.Errorf("%s: 请求失败: %w", provider, err)
	}
	defer resp.Body.Close()

	// 响应体上限 32MB：防某个异常端点把内存打爆
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: 读取响应失败: %w", provider, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{
			Provider: provider,
			Status:   resp.StatusCode,
			Reason:   resp.Status,
			Body:     bodySnippet(raw),
		}
	}
	return raw, nil
}

// doJSON 发请求并反序列化 JSON 响应。
func doJSON(ctx context.Context, opt Options, provider, method, endpoint string, params url.Values, headers map[string]string, jsonBody any, out any) error {
	raw, err := do(ctx, opt, provider, method, endpoint, params, headers, jsonBody)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: 响应非预期 JSON: %w (body=%s)", provider, err, bodySnippet(raw))
	}
	return nil
}

// trimLimit 把 limit 收敛到 [1, max]。
func trimLimit(limit, max int) int {
	if limit < 1 {
		limit = 1
	}
	if limit > max {
		limit = max
	}
	return limit
}

// signalOf 把"全空"的 Signals 收敛成 nil —— 让绝大多数条目在 JSON 里
// 干脆不出现 signals 字段，只有真有信号的条目才带。
//
// ⚠️ 依赖 model.Signals 是**可比较类型**（只有数值/字符串字段）。
// 将来若往 Signals 里加 slice/map，这里会直接编译失败 —— 等于自带防呆。
func signalOf(s model.Signals) *model.Signals {
	if s == (model.Signals{}) {
		return nil
	}
	return &s
}

// mdImageRe 匹配 markdown 图片语法 `![alt](url)`（exa/tavily 的摘要里常混进正文图片）。
var mdImageRe = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)

// mdLinkRe 匹配 markdown 链接 `[text](url)`，只保留 text。
var mdLinkRe = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)

// cleanSnippet 把摘要里的 markdown 噪声还原成纯文本。
//
// 实测（2026-09-16）两引擎摘要会带 markdown：
// exa 1/39、tavily 1/37 的条目命中，典型形态是
// `...交流发言。\n!\n[](./W020231010319429517399.png)\n会后，...`。
//
// ⚠️ 这是**文本清洗，不是过滤**——不删结果、不改 URL，只把展示层的语法糖去掉，
// 与项目"过滤 = 删结果，一律不做"的契约不冲突。
func cleanSnippet(s string) string {
	if s == "" {
		return s
	}
	s = repairMojibake(s)
	if strings.Contains(s, "![") {
		s = mdImageRe.ReplaceAllString(s, "")
	}
	if strings.Contains(s, "](") {
		s = mdLinkRe.ReplaceAllString(s, "$1")
	}
	return normalizeSpace(s)
}

// repairMojibake 修"UTF-8 字节被当成 Latin-1 解码"造成的乱码（mojibake）。
//
// 现象（2026-09-17 实测）：exa 的 `contents.text` 字段会返回坏字节，
// 表现为 `ç°è±¡çº§ççº¢` 这类 latin-1 区间的字符，而**同一响应里 `title` 完全正常**。
//
// 证据链（直连 api.exa.ai 复现）：
//
//	字段      乱码率
//	title     0.00（5/5 全正常）
//	text      0.84 / 0.83（5 条里 2 条几乎整段是坏字节）
//
// 同一份响应里 title 好、text 坏 → **是上游对 `contents.text` 的编码处理有缺陷**，
// 不是本 CLI 的解码问题（我们若解码错，title 也会一起坏）。
// 全引擎扫描（同一 query）确认**只有 exa 中招**，其余 7 个引擎 maxS ≤ 0.07。
//
// ⚠️ 修复口径是**保守且可逆**，与"宁放过不杀错"同构：
//
//  1. 只接受**无损往返**的还原：`s` 逐字符映射为字节 → 按 UTF-8 解码 → 必须
//     能**逐字节解回去且无替换符**，才采用结果。
//  2. 任一步失败（出现 U+FFFD、或解码报错）→ **原样返回**，绝不用替换符凑数。
//  3. 不做部分修复。实测 exa 的坏字节里**有真丢字节**的情形（`â uu` 少了 0xC3），
//     那种情况下直接解码必然失败 → 按第 2 条原样保留，不会把数据改得更烂。
//
// 之所以敢在 snippet 上做这一步：它**只影响展示文本**，不改 URL、不删结果、
// 不动 title，且失败时是恒等变换（零风险）。与"过滤 = 删结果一律不做"不冲突。
func repairMojibake(s string) string {
	// 快速跳过：全是 ASCII 就不可能有 UTF-8/Latin-1 混淆。
	var hasSuspect bool
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			hasSuspect = true
			break
		}
	}
	if !hasSuspect {
		return s
	}

	// 若已含 U+FFFD，说明上游已经做过一次有损替换，不可逆 —— 直接放行，
	// 避免在已损坏的文本上二次操作。
	if strings.ContainsRune(s, '\uFFFD') {
		return s
	}

	// 步骤 1：每个 rune 必须能映射回单字节（即字符全在 Latin-1 区间）。
	// 超出 Latin-1 的字符说明这段文本不是"UTF-8 字节被当 Latin-1"的形态，放行。
	buf := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xFF {
			return s
		}
		buf = append(buf, byte(r))
	}

	// 步骤 2：还原出来的字节必须是**合法 UTF-8**，否则说明上游丢过字节
	// （实测 exa 存在这种样本），无法无损还原 —— 原样返回，绝不用替换符凑数。
	if !utf8.Valid(buf) {
		return s
	}

	// 步骤 3：无损性证明 + "是否真像 mojibake"的判据（合并在这一步）。
	//
	// ⚠️ 这里**不能**用"把解码结果再展开回 Latin-1 比对"作判据 ——
	// 解码后得到的是中文（>0xFF），那个比对必然失败，等于永远不修复。
	//
	// 正确的无损判据是：`buf` 本身是**合法 UTF-8**（步骤 2 已保证，无替换符、
	// 无非法序列），因此 `string(buf)` 是一次**精确的、不丢字节的**转换。
	// Latin-1 的每个字节 0x00~0xFF 都能唯一映射到 rune U+0000~U+00FF，
	// 而 UTF-8 解码是可逆的一一映射 —— 全程无信息损失。
	//
	// 末尾"还原结果里必须出现非 Latin-1 字符"这一条**同时**就是 mojibake 特征判据：
	// 出现 rune > 0xFF ⟺ 字节流里存在多字节 UTF-8 序列（反之亦然）。
	// 原先另有一个 looksLikeMojibake 函数做同一件事，2026-09-23 删除 ——
	// 它判的是"存在多字节序列"（更弱：U+0080~U+00FF 也算），本判据成立时它必然成立，
	// 等于在同一处复述一遍更宽松的版本。
	decoded := string(buf)
	var nonLatin int
	for _, r := range decoded {
		if r > 0xFF {
			nonLatin++
		}
	}
	if nonLatin == 0 {
		return s
	}
	return decoded
}
