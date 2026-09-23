// Package config 负责：调参常量、引擎注册表、.env 密钥的读写与分层加载。
//
// 移植自 Python 参考实现 deep_search/search_engine/search_core/{aggregate.py,env.py}。
//
// **有意未移植的部分**（照搬会违反"宁放过不杀错"）：
//   - RRF 融合排序 + 引擎权重 + content_quality 内容质量分：
//     本 CLI 只做检索，不做排序。理由见 README「与参考实现的差异」。
//   - 广告/垃圾页关键词剔除：那是"过滤即删结果"，本 CLI 一律不删，
//     把判断权留给调用方（engine_status / 字段本身已足够诊断）。
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ========== 检索条数（口径与参考实现一致） ==========
//
// DefaultLimit 即参考实现的 text_sources_per_query / ENGINE_RESULT_CAP：
// 「每引擎请求条数」固定 15，**不由调用方按需下发**。
// 各引擎真实天花板不同（2026-09-16 复核）：
//   - serpapi：num 被忽略（单页 ≤10），靠 start 翻页累积 → 可达 15
//   - tavily / qianfan：请求多少回多少 → 15
//   - exa：numResults 放宽到 100，受真实结果数限制 → 约 12
//   - bing：每页约 10，靠 first 翻页聚合（**必须带 Cookie: _EDGE_V=1**）→ 约 13
//   - quark：每页约 9，靠 snum 翻页聚合（**必须移动 UA**），且剔除媒体垂直/中转壳
//     → 约 11~15（受自身结果集与翻页重叠限制，不保证满 15）
//   - anysearch：API 规定 1~10 → 最多 10
//   - metaso：一次请求要多少给多少（实测 size=20 回满、50/100 不报错、按自身结果集封顶）
//     → 受上限口径限制，入池仍是 15
//
// 所以"每引擎 15 条"是上限口径，不等于每个源都凑够 15 条。
const DefaultLimit = 15

// ⚠️ 摘要**不截断**：参考实现的 aggregate.SNIPPET_MAX = 400 已被**刻意移除**。
// 理由：上游正文的流量已经付过，截断只是让调用方少掉一段可用于精细筛选的依据。
// 此处**不留常量**，以免后人"顺手"把它接回去。完整取舍见 docs/decisions.md。

// DefaultEngineTimeout 单引擎超时。参考实现给 serpapi 放宽到 30s、其余 20s；
// 这里统一 20s，由 -engine-timeout 覆盖。
const DefaultEngineTimeout = 20

// EngineTimeoutFloor 单引擎超时的**下限**表：即便是调用方传进来的较小值，
// 这些引擎也至少给到这里。理由是它们的正常耗时本就波动很大：
//   - serpapi：观测到单次 1.8s ~ 32.5s（同一 query 同一 key，上游抖动）。
//
// ⚠️ 只设下限不设上限：调用方传更大的值一律尊重。
// （quark 曾因浏览器渲染需要 25s 下限，改用移动站纯 HTTP 后只要 1.6s，已移出本表。）
//
// ⚠️ serpapi 曾有一套重试装置（`serpapiAttempts`/`serpapiDelay`），2026-09-19 已整体删除
// —— 它只砍过"理论最坏 2×30s"，**该最坏情形从未被观测到**，32.5s 是首次请求就慢、
// 与重试无关。**本表的 30s 下限才是 serpapi 跟上游抖动较劲的唯一真手段，保留。**
var EngineTimeoutFloor = map[string]int{
	"serpapi": 30,
}

// DefaultGlobalTimeout 整轮硬熔断上限（参考实现聚合层 45s 硬熔断 + 全局 120s）。
const DefaultGlobalTimeout = 60

// PrecheckTimeout 单域名握手超时。
//
// 实测依据（本机、61 个真实可达域名）：可达域名握手最慢 287ms，绝大多数 <100ms；
// 不可达域名要吃满本值才判死，而 32 并发下其余域名 0.3s 内全部完成，
// **整段敲门时长几乎就等于本值**（只剩那一个域名在等）。
//
// 参考实现给 5.0s，单独看没问题；但它的整轮基数是 15s，5s 不显眼。
// 本 CLI 聚合阶段只要 2.4s，5s 立刻成为瓶颈（实测整轮 7.3s 里 5s 是这一项）。
// 2s 相对 287ms 仍有 7 倍余量，且可用 -precheck-timeout 覆盖。
const PrecheckTimeout = 2

// PrecheckWorkers 并发探测数。预热与敲门共用这一个并发上限。
const PrecheckWorkers = 32

// ========== 引擎集合 ==========
//
// FreeEngines 免费/匿名可用（不需要 key，无条件尝试）。
// PaidEngines 需要 key，配了才启用。
var (
	FreeEngines = []string{"bing", "anysearch", "quark"}
	PaidEngines = []string{"exa", "tavily", "serpapi", "qianfan", "metaso"}
)

// AllEngines 返回全部已实现的引擎名（升序）。
func AllEngines() []string {
	out := append(append([]string{}, FreeEngines...), PaidEngines...)
	sort.Strings(out)
	return out
}

// DefaultEngines 返回默认候选引擎：免费引擎 + 已配 key 的付费引擎。
func DefaultEngines(hasKey func(string) bool) []string {
	var out []string
	out = append(out, FreeEngines...)
	for _, e := range PaidEngines {
		if hasKey(e) {
			out = append(out, e)
		}
	}
	return out
}

// ========== 追踪参数 ==========
//
// 追踪参数 = 点击/归因/分享用、本身不决定"这是哪一页"。去重层在算 URL 是否同一页时
// 会按档位剥掉它们（`?utm_source=bing` 与裸 URL 是同一个页面）。
//
// minimalTracking 只含"无歧义"的点击/归因参数 —— 剥离它们不可能改变页面内容。
//
// ⚠️ `srsltid`（Google 购物的广告点击 id）是 **2026-09-18 实测补进来的**，别当冗余删掉：
// 它不在表里时 `…/x?srsltid=…` 与 `…/x` 规范化后**不相等** → 走不到契约 0，
// 落到相似度层又被 `veto:diff_numbers` 拦下（长 token 把数字集合撑大了）→ **同一页判成两条**。
// 实测 3 对（openshop / keychron / philips）。补进来后走契约 0 无条件合并，**不碰任何阈值**。
var minimalTracking = []string{
	"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content",
	"utm_id", "utm_name", "utm_reader", "utm_social", "utm_brand",
	"gclid", "gclsrc", "dclid", "fbclid", "msclkid", "yclid", "srsltid",
	"mc_cid", "mc_eid", "igshid", "wbraid", "gbraid",
	"_ga", "_gl", "vero_id", "wickedid",
}

// extendedTracking 参考实现 aggregate._TRACKING_PARAMS 的额外项。
//
// ⚠️ spm / from / ref / referrer / referral / share_token 这 6 个在个别站点上
// 可能是**有语义**的查询参数（如 ?from=chapter2），剥离后会把不同页面
// 归一成同一个 key。参考实现选择了剥离；本 CLI 保守起见把它单列为 default 档，
// "宁放过不杀错"可用 -tracking=minimal。
var extendedTracking = []string{
	"spm", "from", "ref", "referrer", "referral", "share_token",
}

// TrackingMode 追踪参数剥离档位。
type TrackingMode string

const (
	// TrackingDefault 参考实现口径：minimal + extended。
	TrackingDefault TrackingMode = "default"
	// TrackingMinimal 只剥无歧义参数（最保守）。
	TrackingMinimal TrackingMode = "minimal"
	// TrackingNone 完全不剥（去重仍会去除 fragment 与排序 query）。
	TrackingNone TrackingMode = "none"
)

// TrackingSet 返回指定档位的参数集合。
func TrackingSet(m TrackingMode) map[string]bool {
	set := map[string]bool{}
	if m == TrackingNone {
		return set
	}
	for _, k := range minimalTracking {
		set[k] = true
	}
	if m != TrackingMinimal {
		for _, k := range extendedTracking {
			set[k] = true
		}
	}
	return set
}

// ========== 引擎 → 环境变量 ==========

// APIKeyVars 各 provider 需要的环境变量名。
var APIKeyVars = map[string]string{
	"exa":       "EXA_API_KEY",
	"tavily":    "TAVILY_API_KEY",
	"anysearch": "ANYSEARCH_API_KEY",
	"serpapi":   "SERPAPI_API_KEY",
	"qianfan":   "QIANFAN_API_KEY",
	"metaso":    "METASO_API_KEY",
}

// ProviderOfEnvVar 反查：环境变量名 → provider 名（apikey 命令用）。
func ProviderOfEnvVar(envVar string) (string, bool) {
	for p, v := range APIKeyVars {
		if v == envVar {
			return p, true
		}
	}
	return "", false
}

// ========== .env 路径与分层 ==========

// EnvFileName 密钥文件名（各层统一叫 .env）。
const EnvFileName = ".env"

// EnvFileVar 显式指定 .env 路径的环境变量（优先级最高）。
const EnvFileVar = "METASEARCH_ENV_FILE"

// PersistentEnvPath 密钥的**持久落盘位置**：与当前工作目录无关。
//
// 这是 apikey set/unset 的默认写入目标。放用户级配置目录的理由：
// 密钥属于"用户级配置"，不属于"某个项目"。若写进 ./.env，换个目录执行
// 就读不到了 —— 表现为"我明明配过 key，怎么又没配"。
//
//	Windows : %AppData%\metasearch_cli\.env
//	macOS   : ~/Library/Application Support/metasearch_cli/.env
//	Linux   : ~/.config/metasearch_cli/.env
func PersistentEnvPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		if home, herr := os.UserHomeDir(); herr == nil {
			dir = filepath.Join(home, ".config")
		} else {
			dir = "."
		}
	}
	return filepath.Join(dir, "metasearch_cli", EnvFileName)
}

// LocalEnvPath 当前工作目录下的 .env（只读层，兼容"就地放配置"的用法）。
func LocalEnvPath() string {
	if p, err := filepath.Abs(EnvFileName); err == nil {
		return p
	}
	return EnvFileName
}

// exeDirEnvPath 可执行文件同目录的 .env（便携用法）。
func exeDirEnvPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), EnvFileName)
}

// EnvLayer 一个参与合并的 .env 来源。
type EnvLayer struct {
	Path   string
	Vars   map[string]string
	Exists bool
	// Writable 是否是 apikey set/unset 的默认写入目标。
	Writable bool
}

// EnvSources 按优先级（高 → 低）列出参与合并的 .env 路径。
//
// 分层合并而非"取第一个"：持久文件里只放改动过的几个 key 时，其余 key 仍能从
// 更下层（当前目录 / exe 同目录）读到，不会因为上一层文件存在就整体失效。
// 进程环境变量优先级高于全部 .env（已存在的环境变量不被覆盖）。
func EnvSources(explicit string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" {
			return
		}
		p = normalizePath(p, "")
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}

	add(explicit)              // 1. 显式 -env
	add(os.Getenv(EnvFileVar)) // 2. 环境变量指定
	add(exeDirEnvPath())       // 3. 可执行文件同目录（便携）
	add(LocalEnvPath())        // 4. 当前工作目录
	add(PersistentEnvPath())   // 5. 持久文件（与 cwd 无关）
	return out
}

// WriteTargetPath apikey set/unset 的落盘文件：-env 优先，否则持久文件。
//
// 返回值与 EnvSources 用**同一套规范化**（Abs + ToSlash），否则
// "写入目标"和"生效来源"会是同一文件的两个字符串形式，
// 字符串比较就会误报"本次写入被遮蔽"。
func WriteTargetPath(explicit string) string {
	return normalizePath(explicit, PersistentEnvPath())
}

// normalizePath 把路径统一成绝对路径 + 正斜杠。
func normalizePath(explicit, fallback string) string {
	p := strings.TrimSpace(explicit)
	if p == "" {
		p = fallback
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.ToSlash(p)
}

// samePath 判断两个路径是否指向同一个文件（已规范化）。
func samePath(a, b string) bool {
	return normalizePath(a, a) == normalizePath(b, b)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ParseEnvFile 解析 KEY=VALUE 行，忽略空行与 # 注释，支持引号包裹。
// 与参考实现 env._parse_env_file 行为一致。
func ParseEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if k != "" {
			out[k] = v
		}
	}
	return out
}

// LoadEnvLayers 按优先级读出全部参与合并的 .env 层。
func LoadEnvLayers(explicit string) ([]EnvLayer, error) {
	var layers []EnvLayer
	writeTarget := WriteTargetPath(explicit)
	for _, p := range EnvSources(explicit) {
		l := EnvLayer{Path: p, Writable: samePath(p, writeTarget)}
		if fileExists(p) {
			l.Exists = true
			l.Vars = ParseEnvFile(p)
		} else if explicit != "" && samePath(p, explicit) {
			return nil, fmt.Errorf("指定的 .env 不存在: %s", explicit)
		}
		layers = append(layers, l)
	}
	return layers, nil
}

// APIKeys 最终生效的密钥集合；仅含已配置的项。
type APIKeys struct {
	Keys map[string]string
	// Sources 每个 key 来自哪个 .env（值为 "" 表示来自进程环境变量）
	Sources map[string]string
	Layers  []EnvLayer
}

// LoadAPIKeys 读取密钥：进程环境变量优先，.env 按层合并（前层覆盖后层）。
func LoadAPIKeys(explicitEnvFile string) (APIKeys, error) {
	layers, err := LoadEnvLayers(explicitEnvFile)
	if err != nil {
		return APIKeys{}, err
	}
	keys := map[string]string{}
	sources := map[string]string{}
	for provider, varName := range APIKeyVars {
		if v := os.Getenv(varName); v != "" {
			keys[provider] = v
			sources[provider] = "" // 进程环境变量
			continue
		}
		for _, l := range layers {
			if !l.Exists {
				continue
			}
			if v := l.Vars[varName]; v != "" {
				keys[provider] = v
				sources[provider] = l.Path
				break
			}
		}
	}
	return APIKeys{Keys: keys, Sources: sources, Layers: layers}, nil
}

// HasKey 返回该 provider 是否已配置密钥。
func (a APIKeys) HasKey(provider string) bool {
	_, ok := a.Keys[provider]
	return ok
}

// Get 取密钥。
func (a APIKeys) Get(provider string) string { return a.Keys[provider] }

// Mask 密钥脱敏展示：只留前 6 后 4。
func Mask(s string) string {
	if s == "" {
		return "(未配置)"
	}
	r := []rune(s)
	if len(r) <= 12 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:6]) + strings.Repeat("*", 6) + string(r[len(r)-4:])
}

// ========== .env 行级读写（apikey 命令用） ==========
//
// 写入策略：**原位替换**已存在的行，不存在则追加到末尾；
// 注释行与其余键一律原样保留 —— 参考实现的 .env 里有大量说明注释，不能被覆盖掉。
// 因此反复 set 同一个 key 不会产生重复行（可安全地"重写/替换"）。

// SetKey 在 path 中设置 KEY=VALUE。返回是否发生了新建文件（false = 更新已有文件）。
func SetKey(path, key, value string) (created bool, err error) {
	lines, existed, err := readLines(path)
	if err != nil {
		return false, err
	}
	re := keyLineRe(key)
	replaced := false
	for i, l := range lines {
		if re.MatchString(l) {
			lines[i] = key + "=" + value
			replaced = true
			break
		}
	}
	if !replaced {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, key+"="+value)
	}
	if err := writeLines(path, lines); err != nil {
		return false, err
	}
	return !existed, nil
}

// UnsetKey 删除 path 中的全部同名 KEY 行。返回是否确实删掉了什么。
func UnsetKey(path, key string) (bool, error) {
	lines, existed, err := readLines(path)
	if err != nil {
		return false, err
	}
	if !existed {
		return false, nil
	}
	re := keyLineRe(key)
	out := make([]string, 0, len(lines))
	removed := false
	for _, l := range lines {
		if re.MatchString(l) {
			removed = true
			continue
		}
		out = append(out, l)
	}
	if !removed {
		return false, nil
	}
	return true, writeLines(path, out)
}

// keyLineRe 匹配一行是否是该 KEY 的赋值（容忍前后空白）。
func keyLineRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
}

func readLines(path string) ([]string, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines, true, nil
}

func writeLines(path string, lines []string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	// 0o600：.env 含真实密钥，不给同机其他用户读
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return nil
}
