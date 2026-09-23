// Package dedup 实现"宁放过，不杀错"的保守去重。
//
// 判据与阈值移植自一份 Python 侧级联判据（当时住在 `D:/桌面/core_search/dedup.py`，
// 已于 2026-09-18 整体归档），目标是让检索内核**自己**产出合并后的结果，下游不必再补一层。
// ⚠️ 此后**只有这一份实现**：判据事实不许只活在归档件里，已全部落到本文件注释与
// `dedup_test.go` 的回归用例（Unicode 空白 / `\p{Nd}` 取数 / 否决链 / 备用 URL 字典）。
//
// 级联两维，不是并列 —— 「先索引、后判官」：
//
//	维度一（零成本，规范化精确）  url / title / snippet 任一规范化后相等 → 同一个桶
//	                            只用来**产出候选对**，本身不删任何东西
//	维度二（有成本，只对候选对算）n-gram Jaccard + 4 个结构化正交信号 → 判决
//
// 三条硬契约：
//
//  0. **规范化 URL 全等 = 同一页 → 无条件合并**，文本像不像**不构成**保留理由。
//     实测（「紫色面具」2026-09-17）：pexels 同一页被两个引擎返回，URL 完全相同，
//     但一个给站点模板标题、另一个给查询词标题（title 相似度仅 0.12）——
//     靠"两边文本都得像"永远合不上。同理 baike 的 `/item/紫色面具` 与
//     `/item/%E7%B4%AB...` 也是同一页（EscapedPath 天然归一）。
//  1. **合并必须留痕**：被吞并的 URL、来源引擎、判定理由全部进 `Hit.MergedFrom`。
//  2. **两维都过才删**：维度一不过 → 根本不进二审；维度二没过 → 保留。
//
// **偏置：宁可漏杀。** 删错丢的是一条真结果且不易察觉；漏杀只是多几条。
// 所以两条通过路径都要求「同站」—— 跨注册域永不合并，没有开关能打开它。
//
// ⚠️ 相对原版（只做"URL 剥 tracking 后精确相等"）的若干处**放宽**，都是为修实测缺陷：
//   - URL 归一新增**尾斜杠归并**、**query 解码后重排再编码**（`?write` 与 `?write=` 同页）、
//     路径 `%xx` 的**大小写归一**（`%e7` 与 `%E7` 同页）；
//   - 新增**标题剥站点后缀**（`… - 知乎` / `…_百度百科`），否则同一页被不同引擎挂上
//     各自站点名就合不上。
//
// 这些细节**曾逐条对着 Python 侧 `norm_url` 对齐**（同一份数据不能有两个答案）。
// 改这里的任何一处，都必须重跑
//
//	DEDUP_PROBE_JSON=<结果.json> go test -tags probe -run TestProbeRealData -v ./internal/dedup/
//
// 并核对合并明细（谁吞了谁、理由、备用 URL 的标题与引擎对不对）——
// 判据硬事实与踩过的坑见 `docs/decisions.md` 第五节。
//
// ⚠️ 但**必须同时有否决链**，否则会误杀：京东首页与品牌页的标题归一后完全相同
// （都是"紫色面具品牌及商品"），只靠标题判等会把两个不同页面合并 ——
// `veto:diff_section`（路径首段 `brand` vs 空）正是拦住它的那道闸。别把否决链当累赘删掉。
package dedup

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zhidian-cmd/metasearch_cli/internal/config"
	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

// ========== 阈值 ==========

// ModeConservative 是**唯一**的档位，只用于输出字段 `dedup.mode` ——
// 让人一眼看出"去重跑没跑"。
//
// ⚠️ 这层恒开、判据强度不可下调：`-dedup` / `-dedup-threshold` 两个开关与 `off` 档
// 均已删除（理由见 `docs/decisions.md`「为什么没有去重开关」）。`url` / `balanced` /
// `loose` 三个内部预设同样已删（2026-09-19）—— 它们只有带 probe 标签的离线探针能选中，
// 留着就是一条没人走的旁路；日后真要调松/收紧，改上面的 preset 一处即可。
const ModeConservative = "conservative"

// thresholds 一组判决阈值。
type thresholds struct {
	// TitleSim / SnippetSim / URLSim 维度二的相似度门槛
	TitleSim   float64
	SnippetSim float64
	URLSim     float64
	// MinNumOverlap 数字集合重合度低于此值 → 大概率是不同条目（如分页 id），放过
	MinNumOverlap float64
}

// preset **唯一**的阈值组合：删多了丢的是结果，删少了只是多几条。
var preset = thresholds{TitleSim: 0.90, SnippetSim: 0.85, URLSim: 0.70, MinNumOverlap: 0.30}

// ========== 规范化（维度一用） ==========

// textNoise 文本噪声里的**标点与信息分隔符**部分。**只在比较用**的规范化文本里剔除，
// 不碰原始数据。
//
// ⚠️ 空白类不在这里 —— 由 isTextNoise 的 `unicode.IsSpace` 负责。Python 侧的 `\s` 是
// Unicode 空白，比 ASCII 六个多出 23 个（`&nbsp;` U+00A0、全角空格 U+3000、
// 细空格 U+2002~U+200A、U+2028/U+2029 等），这些在中文网页里真实常见。
// 只列 ASCII 六空白的旧写法会让「正文里一方是 `&nbsp;`、另一方是普通空格」的两条归一后不等
// → 少合并（方向是漏杀、不误杀，但会让 Go 与 Python 对同一份数据给出两个答案）。
// U+001C~U+001F 是 Python `\s` 匹配、而 Go 的 `unicode.IsSpace` **不**匹配的四个 Cc 控制符，
// 故显式列出补平。
const textNoise = " \t\n\r\v\f-_|—–·,，。！!？?：:；;（）()[]【】\"'`\x1c\x1d\x1e\x1f"

// isTextNoise 该字符是否算文本噪声（比较用规范化时剔除）。
//
// 与 Python 侧 `_TEXT_NOISE = re.compile(r"""[\s\-_|—–·,，。！!？?：:；;（）()[]【】"'`]+""")`
// **逐码点等价**：`\s` → `unicode.IsSpace`，其余字符走 textNoise 表。
// 等价性由 `TestProbeCharset`（`go test -tags probe`）把两侧集合逐码点导出后做集合差验证，
// 不靠肉眼读常量。
func isTextNoise(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune(textNoise, r)
}

// ivp4Re 用于识别裸 IPv4 host（注册域启发式对它不适用）。
var ipv4Re = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)

// multiSuffix 常见「两段式公共后缀」—— 只用于把 host 粗化成注册域，**不追求 PSL 完整**。
// 判错的方向是保守的：把同站误判成跨站 → 少删一条（漏杀），不会反过来多删。
var multiSuffix = map[string]bool{
	"com.cn": true, "net.cn": true, "org.cn": true, "gov.cn": true, "edu.cn": true, "ac.cn": true,
	"co.uk": true, "org.uk": true, "ac.uk": true, "co.jp": true, "ne.jp": true, "or.jp": true,
	"com.hk": true, "com.tw": true, "com.mo": true, "com.au": true, "co.kr": true, "com.sg": true,
}

// NormURL URL 规范化：host 小写、去与 scheme 等价的默认端口、尾斜杠归并、剥追踪参数、
// query **解码后按 (键, 值) 重排再编码**、路径按 EscapedPath 归一并把 `%xx` 统一成大写。
//
// ⚠️ **不含 scheme**：`http://x/a` 与 `https://x/a` 归一后相等（同一页的两种入口）。
// 解析失败或相对 URL → 退化为"整串小写"，仍要求精确相等才合并，不会误杀。
//
// 路径用 `u.EscapedPath()`：Go 会把 `/item/紫色面具/1` 重新编码成 `/item/%E7%B4%AB.../1`，
// 于是"明码写"和"百分号写"的同一页天然归一；而 `/a%2Fb`（编码的分隔符）因为保留在
// RawPath 里不会被误解成两级路径 —— 这正是我们想要的，无需再手写解码白名单。
func NormURL(raw string, tracking map[string]bool) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return strings.ToLower(s)
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host = host + ":" + port
	}
	path := upperHexEscapes(u.EscapedPath())
	if path == "" {
		path = "/"
	}
	if path != "/" {
		path = strings.TrimRight(path, "/")
	}
	if path == "" {
		path = "/"
	}
	q := normQuery(u.RawQuery, tracking)
	if q == "" {
		return host + path
	}
	return host + path + "?" + q
}

// upperHexEscapes 把 `%xx` 形式的十六进制转义统一成大写 `%XX`（非合法转义原样保留）。
//
// 存在的理由：Go 的 `EscapedPath()` 在 URL 里**已有**转义时会原样返回 `RawPath`，
// 于是 `%e7%b4%ab`（小写）与 `%E7%B4%AB`（大写）归一后**不相等** —— 而 Python 侧
// `unquote` 把两者都解成 `紫色`，是相等的。不补齐这一处，同一页的两种写法就会
// 「Python 合得掉、Go 合不掉」= Go 少删，破坏「Go 的删除集合 ⊇ Python」。
// 转义大小写不改变字节含义，归一它是无损的。
func upperHexEscapes(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	b := []byte(s)
	for i := 0; i+2 < len(b); i++ {
		if b[i] != '%' {
			continue
		}
		if h := upperHexDigit(b[i+1]); h != 0 {
			b[i+1] = h
			b[i+2] = upperHexDigit(b[i+2])
		}
	}
	return string(b)
}

func upperHexDigit(c byte) byte {
	switch {
	case c >= '0' && c <= '9', c >= 'A' && c <= 'F':
		return c
	case c >= 'a' && c <= 'f':
		return c - ('a' - 'A')
	}
	return 0
}

// normQuery 剥追踪参数 + 归一后排序 —— **与 Python 侧 `norm_url` 的 query 处理逐条等价**。
//
// 顺序：按 `&` 拆 → 每个片段拆 `k` / `v`（无 `=` 视为空值，与 Python 的
// `parse_qsl(..., keep_blank_values=True)` 一致）→ 两边都做 percent-解码 → 按**解码后**的键
// 小写比对追踪参数表 → 按 (k, v) 排序（对应 Python 的 `q.sort()`）→ 重新 percent-编码。
//
// ⚠️ 早先这里是"保留参数原始写法、不重新编码"，本意是保守（不把 `+` 与 `%20` 归一）。
// 但那会让 Go 的等价类**比 Python 窄**：`?write` 与 `?write=`、`?q=a+b` 与 `?q=a%20b`
// 在 Python 都归一成同一个 key，Go 却当成两页 → Go 少删。判据要移植就得连 query 的
// 归一方式一起搬，否则同一份数据两边给出两个答案。
func normQuery(rawQuery string, tracking map[string]bool) string {
	if rawQuery == "" {
		return ""
	}
	type kv struct{ k, v string }
	pairs := make([]kv, 0, strings.Count(rawQuery, "&")+1)
	for _, p := range strings.Split(rawQuery, "&") {
		if p == "" {
			continue
		}
		k, v := p, ""
		if i := strings.IndexByte(p, '='); i >= 0 {
			k, v = p[:i], p[i+1:]
		}
		dk, dv := k, v
		if dec, err := url.QueryUnescape(k); err == nil {
			dk = dec
		}
		if dec, err := url.QueryUnescape(v); err == nil {
			dv = dec
		}
		if tracking != nil && tracking[strings.ToLower(dk)] {
			continue
		}
		pairs = append(pairs, kv{dk, dv})
	}
	sort.Slice(pairs, func(a, b int) bool {
		if pairs[a].k != pairs[b].k {
			return pairs[a].k < pairs[b].k
		}
		return pairs[a].v < pairs[b].v
	})
	var sb strings.Builder
	for i, p := range pairs {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(url.QueryEscape(p.k))
		sb.WriteByte('=')
		sb.WriteString(url.QueryEscape(p.v))
	}
	return sb.String()
}

// NormText 文本规范化：小写 + 去空白与标点。只用于比较，不改原始数据。
func NormText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if isTextNoise(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// siteSuffixSep 标题里用来挂"站点名"的分隔符（中英都要，实测都出现过）。
var siteSuffixSep = []rune{'|', '｜', '—', '–', '·', '_', '-'}

// sentencePunct 尾段里出现这些就不是站点名，是句子。
const sentencePunct = "，。！？；：、,.;!?:\t\n"

const (
	// maxSuffixPasses 最多剥几层（`… - 2026年9月更新 - 淘宝Taobao｜天猫Tmall` 挂了两三层）
	maxSuffixPasses = 2
	// maxSuffixLen 站点名长度上限（超过这个长度基本是副标题，不是站点名）
	maxSuffixLen = 12
)

// StripSiteSuffix 剥掉标题尾部的**站点名**段：`… - 知乎` / `…_百度百科` / `…｜天猫Tmall`。
//
// 同一篇内容被不同站点、不同引擎模板挂上各自的站点名，是很常见的形态；
// 不剥就会让"同一页被两个引擎返回"因为标题不同而合不上（实测 pexels 那对）。
//
// 只在**看着确实像站点名**时才剥，四个条件全满足才动手（任一不满足就停）：
// ① 分隔符右侧有内容；② 尾段长度 ≤ maxSuffixLen；③ 尾段不含句子级标点；④ 左侧还剩内容。
// 判错的方向是保守的：不剥 → 只是少合并一对，不会误杀。
func StripSiteSuffix(t string) string {
	s := []rune(strings.TrimSpace(t))
	if len(s) == 0 {
		return ""
	}
	for pass := 0; pass < maxSuffixPasses; pass++ {
		cut := -1
		for _, sep := range siteSuffixSep {
			for i := len(s) - 1; i >= 0; i-- {
				if s[i] == sep {
					if i > cut {
						cut = i
					}
					break
				}
			}
		}
		if cut <= 0 {
			break
		}
		head := strings.TrimRight(string(s[:cut]), " \t")
		tail := strings.TrimSpace(string(s[cut+1:]))
		if head == "" || tail == "" {
			break
		}
		if utf8.RuneCountInString(tail) > maxSuffixLen || strings.ContainsAny(tail, sentencePunct) {
			break
		}
		s = []rune(head)
	}
	return string(s)
}

// NormTitle 标题规范化：**剥站点后缀** + 去空白标点小写。维度一与标题 n-gram 共用这一份。
func NormTitle(t string) string { return NormText(StripSiteSuffix(t)) }

// registrableSite 把 host 粗化成「注册域」（`www.36kr.com` / `m.36kr.com` → `36kr.com`）。
//
// 用来区分「同一站的不同子域」与「完全不相干的另一个站」—— 这正是误杀风险的分界线。
// 只用两段/三段启发式，不引 PSL 依赖。
func registrableSite(host string) string {
	h := strings.ToLower(strings.Trim(host, "."))
	if h == "" || !strings.Contains(h, ".") || ipv4Re.MatchString(h) {
		return h
	}
	parts := strings.Split(h, ".")
	if len(parts) >= 3 && multiSuffix[strings.Join(parts[len(parts)-2:], ".")] {
		return strings.Join(parts[len(parts)-3:], ".")
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// ========== 相似度与结构信号（维度二用） ==========

// ngram 按 **rune**（而非 byte）切 n-gram —— 中文才能按"字"而不是按字节切片。
func ngram(s string, n int) map[string]struct{} {
	r := []rune(NormText(s))
	if len(r) == 0 {
		return nil
	}
	if n < 1 {
		n = 1
	}
	if len(r) <= n {
		return map[string]struct{}{string(r): {}}
	}
	out := make(map[string]struct{}, len(r)-n+1)
	for i := 0; i+n <= len(r); i++ {
		out[string(r[i:i+n])] = struct{}{}
	}
	return out
}

// jaccard 集合 Jaccard 相似度。空集 → 0（"两边都没文本"不构成"相同"的证据）。
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	if inter == 0 {
		return 0
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// scores 维度二的三个相似度。
type scores struct {
	URLSim     float64
	TitleSim   float64
	SnippetSim float64
}

// structSignals 4 个**正交**结构信号 + 1 个数重合度 —— 别再往这里加文字相似度。
//
// 三个文字相似度高度相关（标题像的摘要往往也像，一起投等于一个信号投三票）。
// 真正正交的是结构：同不同域、路径首段是否同类栏目、query 键集合是否同一个接口、
// 数字集合是否重合（分页 id）。
//
// SiteSame 不是第 5 个正交维度，而是 HostSame 的**粗化分档**：它把 HostSame=false 的
// 两类情况分开 —— 同站子域（`www.` vs `m.`，几乎必然同内容）与完全跨站（误杀风险最高）。
type structSignals struct {
	HostSame      bool
	SiteSame      bool
	PathHeadSame  bool
	QueryKeysSame bool
	NumberOverlap float64
}

// numRe 数字序列。**用 `\p{Nd}` 而不是 `\d`** —— 两者不等价：
// Go 的 RE2 里 `\d` 只是 ASCII `[0-9]`（10 个码点），而 Python 的 `\d` 是 Unicode 类别 Nd
// （660 个码点，含全角 `０-９` U+FF10~U+FF19 与阿拉伯-印度数字等）。
// 用 `\d` 会让「２０２４年」这类全角写法在 Go 侧提取不到数字 → 数字集合算错 → 否决判据
// 与 Python 不一致。判据要移植就得连"什么算数字"一起搬。
var numRe = regexp.MustCompile(`\p{Nd}+`)

func headSegment(path string) string {
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg != "" {
			return seg
		}
	}
	return ""
}

func queryKeys(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	var ks []string
	for _, p := range strings.Split(rawQuery, "&") {
		if p == "" {
			continue
		}
		k := p
		if i := strings.IndexByte(p, '='); i >= 0 {
			k = p[:i]
		}
		ks = append(ks, strings.ToLower(k))
	}
	sort.Strings(ks)
	return strings.Join(ks, "&")
}

func signalOf(a, b model.Hit) structSignals {
	ua, erra := url.Parse(strings.TrimSpace(a.URL))
	ub, errb := url.Parse(strings.TrimSpace(b.URL))
	var ha, hb, pa, pb, qa, qb string
	if erra == nil {
		ha, pa, qa = strings.ToLower(ua.Hostname()), headSegment(ua.Path), queryKeys(ua.RawQuery)
	}
	if errb == nil {
		hb, pb, qb = strings.ToLower(ub.Hostname()), headSegment(ub.Path), queryKeys(ub.RawQuery)
	}

	na := map[string]bool{}
	nb := map[string]bool{}
	for _, s := range numRe.FindAllString(a.URL+" "+a.Title, -1) {
		na[s] = true
	}
	for _, s := range numRe.FindAllString(b.URL+" "+b.Title, -1) {
		nb[s] = true
	}
	union := len(na) + len(nb)
	for k := range na {
		if nb[k] {
			union--
		}
	}
	numSim := 1.0 // 两边都没数字 → 不构成否决理由
	if union > 0 {
		inter := 0
		for k := range na {
			if nb[k] {
				inter++
			}
		}
		numSim = float64(inter) / float64(union)
	}
	// ⚠️ 数字取自**原始** url/title：一侧百分号编码时其数字会被拆碎，于是 numSim 偏小 ——
	// **只会少合并（漏杀方向）**，不会误杀。同页的编码差异由契约 0 在判决之前就拦下了。

	hostSame := ha == hb && ha != ""
	return structSignals{
		HostSame:      hostSame,
		SiteSame:      hostSame || (ha != "" && hb != "" && registrableSite(ha) == registrableSite(hb)),
		PathHeadSame:  pa != "" && pa == pb,
		QueryKeysSame: qa == qb,
		NumberOverlap: numSim,
	}
}

// ========== 判决 ==========

// decide 纯判决：只看相似度与结构信号，**不含"维度一必须命中"这道门槛**。
//
// 理由里 `veto:` = 否决（有强证据也要拦），其余为通过证据。
// **偏置是刻意的：宁可漏杀。** 两条通过路径都要求「同站」，跨注册域一律不合并。
func decide(sc scores, sg structSignals, p thresholds) (bool, []string) {
	// ---- 否决（先拦，再谈合并）----
	// 顺序有讲究：**真实原因是什么就先报什么**。实测「同一篇被两个站转载」曾被记成
	// veto:diff_numbers（不同站的 id 必然不同），把真正的原因（跨站）盖住了。
	if !sg.SiteSame {
		return false, []string{"veto:cross_site"}
	}
	if sg.SiteSame && !sg.PathHeadSame {
		return false, []string{"veto:diff_section"}
	}
	if sg.NumberOverlap < p.MinNumOverlap {
		return false, []string{"veto:diff_numbers"}
	}

	// ---- 强证据一：同域 + 同栏目 + 标题/摘要都高 → 同站重复页（含分页）----
	if sg.HostSame && sg.PathHeadSame && sc.TitleSim >= p.TitleSim && sc.SnippetSim >= p.SnippetSim {
		return true, []string{"same_site_path", "text_high"}
	}

	// ---- 强证据二：URL 极像 + 标题高 → 换子域/换后缀/多一个无副作用参数的同一篇 ----
	if sc.URLSim >= p.URLSim && sc.TitleSim >= p.TitleSim {
		if sg.HostSame {
			return true, []string{"url+title_high", "same_host"}
		}
		return true, []string{"url+title_high", "same_site"}
	}
	return false, nil
}

// ========== 合并 ==========

// mergeInto 把 src 的信息并入 dst（不丢信息）：
// 引擎并集、各引擎取最靠前排名、标题/摘要取更长者、日期取首个非空值、信号按约定合并、
// 被吞并的 URL 进**备用 URL 字典**（键 = URL，值 = {标题, 引擎, 理由}）—— 合并 ≠ 消失，
// 那条 URL 连同它的标题与来源引擎随时可回取，不是被无声吞掉。
//
// ⚠️ Engine / Positions / MergedFrom 一律**新建**切片与 map，不 append 到 dst 的旧底层数组 ——
// 否则 dst 与来源项会共享内存，后续任何原地改动都会互相污染。
func mergeInto(dst *model.Hit, src model.Hit, why []string) {
	eng := make([]string, 0, len(dst.Engine)+len(src.Engine))
	seen := map[string]bool{}
	for _, e := range append(append([]string{}, dst.Engine...), src.Engine...) {
		if !seen[e] {
			seen[e] = true
			eng = append(eng, e)
		}
	}
	sort.Strings(eng)
	dst.Engine = eng

	pos := make(map[string]int, len(dst.Positions)+len(src.Positions))
	for e, p := range dst.Positions {
		pos[e] = p
	}
	for e, p := range src.Positions {
		if cur, ok := pos[e]; !ok || p < cur {
			pos[e] = p
		}
	}
	if len(pos) > 0 {
		dst.Positions = pos
	}

	if len(src.Title) > len(dst.Title) {
		dst.Title = src.Title
	}
	if len(src.Snippet) > len(dst.Snippet) {
		dst.Snippet = src.Snippet
	}
	if dst.Date == "" && src.Date != "" {
		dst.Date = src.Date
	}
	dst.Signals = model.MergeSignals(dst.Signals, src.Signals)

	// 备用 URL 字典：先并入两边已有的备用项，再记下 src 自己。
	// ⚠️ 此处 src.Title 仍是**合并前**的原值（上面只改了 dst.Title），落进备用项正是要的。
	refs := make(map[string]model.AltRef, len(dst.MergedFrom)+len(src.MergedFrom)+1)
	for u, a := range dst.MergedFrom {
		refs[u] = a
	}
	for u, a := range src.MergedFrom {
		refs[u] = a
	}
	refs[src.URL] = model.AltRef{
		Title:  src.Title,
		Engine: append([]string{}, src.Engine...),
		Reason: strings.Join(why, ","),
	}
	dst.MergedFrom = refs
}

// ========== 主流程 ==========

// normed 一条结果的预计算规范化形（维度一与 n-gram 共用，只算一次）。
type normed struct {
	url, title, snippet string
	urlNG               map[string]struct{}
	titleNG             map[string]struct{}
	snippetNG           map[string]struct{}
}

func computeNorm(h model.Hit, tracking map[string]bool) normed {
	return normed{
		url:       NormURL(h.URL, tracking),
		title:     NormTitle(h.Title),
		snippet:   NormText(h.Snippet),
		urlNG:     ngram(h.URL, 4),
		titleNG:   ngram(h.Title, 3),
		snippetNG: ngram(h.Snippet, 3),
	}
}

// Run 执行去重，返回保留后的新列表与统计。**输入 items 不作原地改动。**
//
// 唯一的参数是 `-tracking` 档位（剥哪些追踪参数），判据强度固定在上面的 preset。
// 它曾包在一个单字段的 Options 结构体里 + 一个 DefaultOptions()，2026-09-23 删除：
// 档位/阈值一旦删完，结构体里就只剩这一个字段，包一层只是"看起来可扩展"。
//
// 保留下标靠前的那条（输入顺序 = 各引擎顺序 + 引擎内排名），被吞并项进保留项的 MergedFrom。
func Run(items []model.Hit, trackingMode config.TrackingMode) ([]model.Hit, model.DedupStat) {
	stat := model.DedupStat{
		Mode: ModeConservative, Threshold: preset.TitleSim, Raw: len(items),
		Stages: []model.DedupStage{},
	}
	if len(items) <= 1 {
		stat.Final = len(items)
		return items, stat
	}

	tracking := config.TrackingSet(trackingMode)
	n := len(items)

	norms := make([]normed, n)
	for i := range items {
		norms[i] = computeNorm(items[i], tracking)
	}

	// ---- 维度一当索引：三路分桶（url / 标题 / 摘要），桶内两两组合成候选对 ----
	type pair struct{ i, j int }
	cand := map[pair]bool{}
	addBucket := func(idxs []int) {
		for a := 0; a < len(idxs); a++ {
			for b := a + 1; b < len(idxs); b++ {
				cand[pair{idxs[a], idxs[b]}] = true
			}
		}
	}
	bURL := map[string][]int{}
	bTitle := map[string][]int{}
	bSnip := map[string][]int{}
	for i, nm := range norms {
		if nm.url != "" {
			bURL[nm.url] = append(bURL[nm.url], i)
		}
		if nm.title != "" {
			bTitle[nm.title] = append(bTitle[nm.title], i)
		}
		if nm.snippet != "" {
			bSnip[nm.snippet] = append(bSnip[nm.snippet], i)
		}
	}
	for _, idxs := range bURL {
		addBucket(idxs)
	}
	for _, idxs := range bTitle {
		addBucket(idxs)
	}
	for _, idxs := range bSnip {
		addBucket(idxs)
	}

	pairs := make([]pair, 0, len(cand))
	for k := range cand {
		pairs = append(pairs, k)
	}
	sort.Slice(pairs, func(a, b int) bool {
		if pairs[a].i != pairs[b].i {
			return pairs[a].i < pairs[b].i
		}
		return pairs[a].j < pairs[b].j
	})

	// ---- 遍历候选对：契约 0 优先，其余进判决 ----
	kept := make([]model.Hit, n)
	copy(kept, items)
	dropped := make([]bool, n)
	urlMerged, judgedMerged := 0, 0
	veto := map[string]int{}
	pass := map[string]int{}

	for _, pr := range pairs {
		i, j := pr.i, pr.j
		// 一方已被删 → 不再重复判决（等价于并到同一个幸存者上）
		if dropped[i] || dropped[j] {
			continue
		}
		var why []string
		if norms[i].url != "" && norms[i].url == norms[j].url {
			why = []string{"d1:url_exact", "same_url"}
		} else {
			var d1 []string
			if norms[i].title != "" && norms[i].title == norms[j].title {
				d1 = append(d1, "title_exact")
			}
			if norms[i].snippet != "" && norms[i].snippet == norms[j].snippet {
				d1 = append(d1, "snippet_exact")
			}
			if len(d1) == 0 {
				continue // 维度一没过 → 不进二审
			}
			sc := scores{
				URLSim:     jaccard(norms[i].urlNG, norms[j].urlNG),
				TitleSim:   jaccard(norms[i].titleNG, norms[j].titleNG),
				SnippetSim: jaccard(norms[i].snippetNG, norms[j].snippetNG),
			}
			ok, dw := decide(sc, signalOf(items[i], items[j]), preset)
			if !ok {
				for _, r := range dw {
					if strings.HasPrefix(r, "veto:") {
						veto[r]++
					}
				}
				continue
			}
			for _, h := range d1 {
				why = append(why, "d1:"+h)
			}
			why = append(why, dw...)
		}

		mergeInto(&kept[i], items[j], why)
		dropped[j] = true
		if len(why) > 0 && why[0] == "d1:url_exact" {
			urlMerged++
		} else {
			judgedMerged++
		}
		for _, r := range why {
			if !strings.HasPrefix(r, "veto:") && !strings.HasPrefix(r, "d1:") {
				pass[r]++
			}
		}
	}

	out := make([]model.Hit, 0, n)
	for i := range kept {
		if !dropped[i] {
			out = append(out, kept[i])
		}
	}

	stat.Final = len(out)
	stat.TotalMerged = n - len(out)
	stat.Stages = append(stat.Stages,
		model.DedupStage{Stage: "d1_url_exact", Before: n, After: n - urlMerged, Merged: urlMerged},
		model.DedupStage{Stage: "d2_judged", Before: n - urlMerged, After: len(out), Merged: judgedMerged},
	)
	if len(veto) > 0 {
		stat.Veto = veto
	}
	if len(pass) > 0 {
		stat.Pass = pass
	}
	return out, stat
}
