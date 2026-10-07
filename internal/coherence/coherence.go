// Package coherence 桶级相干度判定 —— 一个引擎的整桶结果是不是只回答了
// query 的第一个词（诱饵桶）。
//
// 动机（2026-10-07 引入，判据移植自 free-search-mcp src/search_mcp/coherence.py，
// MIT）：Bing 类引擎被反感时会返回 HTTP 200 + 十条**格式完好**却只匹配首词的
// 结果（"rust ownership borrowing" → 游戏 Rust 的 Steam 页），每个结构检查都
// 通过，融合排序随后把噪声插进每份答案的第 3/6/9 位。逐条相关性闸拦不住这种
// "长得像成功"，只有**整桶**信号看得见。
//
// 实测分布（free-search 2026-09 实测 + 本仓库 2026-10-07 食品包装归档复核）：
// 健康桶 0.5~1.0，诱饵桶 0.0~0.2，中间是空档 —— COHERENCE_MIN=0.3 落在空档里。
//
// ⚠️ 契约：本包**只打标，不删结果**（"过滤 = 删结果一律不做"，见 docs/decisions.md）。
// Judge 返回分数与 decoy 集合，删不删、怎么用由调用方决定。
//
// 依赖约束：被 aggregate 与（将来的）引擎 provider 共用，**不得 import 任何
// 本仓库其他内部包**。
package coherence

import (
	"regexp"
	"strings"
)

// 阈值与最小样本（与 free-search coherence.py 同值，实测依据见包注释）。
const (
	CoherenceMin = 0.3  // 低于此判诱饵（前提：存在 witness 桶）
	WitnessMin   = 0.5  // 达到此值证明"该 query 的词是可能被回显的"
	MinResults   = 4    // 桶内少于该条数不可判定
	MinRestTerms = 2    // 去首词后剩余 term 少于该数不可判定
)

// Row 参与判定的最小字段。URL 计入证据：文档页的关键词常只在路径里出现。
type Row struct {
	Title   string
	Snippet string
	URL     string
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK 统一表意
		(r >= 0x3040 && r <= 0x30FF) || // 日文假名
		(r >= 0xAC00 && r <= 0xD7A3) // 韩文谚文
}

var cjkRunRe = regexp.MustCompile(`[\x{4E00}-\x{9FFF}\x{3040}-\x{30FF}\x{AC00}-\x{D7A3}]+`)

// 操作符与布尔词：没有任何结果会被期待回显它们，判定前剥掉。
var operatorRe = regexp.MustCompile(`(?i)^[+-]?(?:site|filetype|ext|intitle|allintitle|inurl|allinurl|intext|inbody|inanchor|lang|language|loc|location|before|after|feed|contains|ip|prefer):`)

var booleanTokens = map[string]bool{"OR": true, "AND": true, "NOT": true, "|": true, "&": true}

// 停用词：诱饵页和正常页一样会包含 "the"/"how"，无判别力。
var stopwords = map[string]bool{
	"a": true, "about": true, "an": true, "and": true, "are": true, "as": true,
	"at": true, "be": true, "been": true, "best": true, "by": true, "can": true,
	"could": true, "did": true, "do": true, "does": true, "for": true,
	"from": true, "get": true, "had": true, "has": true, "have": true,
	"how": true, "i": true, "if": true, "in": true, "into": true, "is": true,
	"it": true, "its": true, "me": true, "my": true, "new": true, "no": true,
	"not": true, "of": true, "on": true, "or": true, "our": true, "should": true,
	"so": true, "than": true, "that": true, "the": true, "their": true,
	"then": true, "there": true, "these": true, "they": true, "this": true,
	"to": true, "top": true, "up": true, "us": true, "use": true, "using": true,
	"vs": true, "was": true, "we": true, "were": true, "what": true,
	"when": true, "where": true, "which": true, "who": true, "why": true,
	"will": true, "with": true, "would": true, "you": true, "your": true,
}

// wordRe 匹配一个拉丁词（可选小数版本尾巴："3.12" 是一个词，
// "asyncio.TaskGroup" 拆成页面实际会印出的两个词）。
// Python 原式 [^\W_]+ 在 RE2 无类减法，改用 \p{L}\p{N}（下划线本就不在内）。
var wordRe = regexp.MustCompile(`[\p{L}\p{N}]+(?:\.\d+)+|[\p{L}\p{N}]+`)

var stemSuffixes = []string{"ing", "ed", "es", "s"}

// stem 最粗词干：让 "borrowing" 能命中 "borrow checker"。仅 ASCII、
// 永不短于 4 字母，不会把真词削成到处匹配的碎片。
func stem(word string) string {
	for _, r := range word {
		if r >= 0x80 {
			return word // 非 ASCII：不削
		}
	}
	for _, suf := range stemSuffixes {
		if strings.HasSuffix(word, suf) && len(word)-len(suf) >= 4 && !strings.HasSuffix(word, "ss") {
			return word[:len(word)-len(suf)]
		}
	}
	return word
}

func bigrams(run string, start int) []string {
	rs := []rune(run)
	var out []string
	for i := start; i+1 < len(rs); i++ {
		out = append(out, string(rs[i:i+2]))
	}
	return out
}

// tokenTerms 一个空白分隔 token 的可匹配 terms（有序）。
// CJK 串按字符 bigram 切（结果页常把"模型架构"拆成"模型"+"架构"分印，
// bigram 仍能命中）；拉丁词去停用词后削干。
func tokenTerms(token string) []string {
	var terms []string
	for _, run := range cjkRunRe.FindAllString(token, -1) {
		terms = append(terms, bigrams(run, 0)...)
	}
	latin := strings.ToLower(cjkRunRe.ReplaceAllString(token, " "))
	for _, w := range wordRe.FindAllString(latin, -1) {
		if len([]rune(w)) < 2 || stopwords[w] {
			continue
		}
		terms = append(terms, stem(w))
	}
	return terms
}

// contentTokens query 里"切题结果可能回显"的 token：剥操作符、布尔词、
// 排除短语（-"some phrase" 的首尾）。
func contentTokens(query string) []string {
	var tokens []string
	inExcluded := false
	for _, tok := range strings.Fields(query) {
		if inExcluded {
			inExcluded = !strings.HasSuffix(tok, `"`)
			continue
		}
		if strings.HasPrefix(tok, `-"`) {
			inExcluded = !(len(tok) > 2 && strings.HasSuffix(tok, `"`))
			continue
		}
		if booleanTokens[tok] || strings.HasPrefix(tok, "-") || operatorRe.MatchString(tok) {
			continue
		}
		tokens = append(tokens, tok)
	}
	return tokens
}

// Terms 把 query 切成 (anchor, rest)。
//
// anchor = 诱饵也会匹配的部分（第一个内容 token 的 terms）；rest = 其后所有
// 内容 token 的 terms，再减去与 anchor 有包含关系的项（anchor 是 "postgres"
// 时，"postgresql" 证明不了任何事）。
//
// 未分词的纯中文 query 是一个 token：在串内切——首 bigram 做 anchor，
// rest 从第 3 字起，跨接 bigram（"上海明珠"的"海明"）两边都不算。
func Terms(query string) (anchor, rest []string) {
	tokens := contentTokens(query)

	if len(tokens) == 1 {
		runs := cjkRunRe.FindAllString(tokens[0], -1)
		trimmed := strings.Trim(tokens[0], "\"'\u201c\u201d《》「」")
		if len(runs) == 1 && runs[0] == trimmed && len([]rune(runs[0])) >= 4 {
			anchor = []string{string([]rune(runs[0])[:2])}
			rest = bigrams(runs[0], 2)
		}
	}
	if anchor == nil {
		for i, tok := range tokens {
			terms := tokenTerms(tok)
			if len(terms) == 0 {
				continue
			}
			anchor = terms
			for _, later := range tokens[i+1:] {
				rest = append(rest, tokenTerms(later)...)
			}
			break
		}
	}

	seen := map[string]bool{}
	var uniq []string
	for _, t := range rest {
		if seen[t] {
			continue
		}
		dup := false
		for _, a := range anchor {
			if strings.Contains(t, a) || strings.Contains(a, t) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		seen[t] = true
		uniq = append(uniq, t)
	}
	return anchor, uniq
}

// matcher rest terms 的合并正则：≤3 字符的拉丁词必须整词（否则 "go" 命中
// "google"、"ai" 命中 "again"——意外命中会放走诱饵）。RE2 无 lookaround，
// 用 \b 表达同一语义（latin 短词两侧要么是空白/标点/行首尾，要么是 CJK——
// CJK 对 ASCII \b 属于 \W，边界同样成立）。
func matcher(terms []string) *regexp.Regexp {
	var parts []string
	for _, t := range terms {
		esc := regexp.QuoteMeta(t)
		rs := []rune(t)
		cjkTerm := false
		for _, r := range rs {
			if isCJK(r) {
				cjkTerm = true
				break
			}
		}
		if len(rs) <= 3 && !cjkTerm {
			parts = append(parts, `\b`+esc+`\b`)
		} else {
			parts = append(parts, esc)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return regexp.MustCompile("(?i)" + strings.Join(parts, "|"))
}

// Bucket 桶相干度：命中 rest 任一 term 的行占比。
// 返回 -1 = 无法判定（行数不足 / 切不出 anchor 或 rest），调用方按健康处理。
func Bucket(query string, rows []Row) float64 {
	if len(rows) < MinResults {
		return -1
	}
	anchor, rest := Terms(query)
	if len(anchor) == 0 || len(rest) < MinRestTerms {
		return -1
	}
	re := matcher(rest)
	if re == nil {
		return -1
	}
	hits := 0
	for _, r := range rows {
		hay := strings.ToLower(r.Title + " " + r.Snippet + " " + r.URL)
		if re.MatchString(hay) {
			hits++
		}
	}
	return float64(hits) / float64(len(rows))
}

// Judge 桶级诱饵判定（witness 门控）。
//
// 返回 (scores, decoy)：scores 只含可判定桶的相干度；decoy 仅当**存在至少一个
// witness 桶**（≥WitnessMin，证明该 query 的词是可能被回显的）时才非空——
// 冷门 query 全体弱桶是"没法比"，不是"都跑题"。
func Judge(query string, buckets map[string][]Row) (map[string]float64, map[string]bool) {
	scores := map[string]float64{}
	for e, rows := range buckets {
		if sc := Bucket(query, rows); sc >= 0 {
			scores[e] = sc
		}
	}
	witness := false
	for _, sc := range scores {
		if sc >= WitnessMin {
			witness = true
			break
		}
	}
	decoy := map[string]bool{}
	if witness {
		for e, sc := range scores {
			if sc < CoherenceMin {
				decoy[e] = true
			}
		}
	}
	return scores, decoy
}
