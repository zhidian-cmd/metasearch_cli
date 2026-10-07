package coherence

import (
	"strings"
	"testing"
)

// 诱饵桶正例（建模自 free-search 2026-08-29 capture 的真实案例）：
// "rust ownership borrowing" → 游戏 Rust 的页面，十条里没有一条提 ownership/borrowing。
func rustDecoyRows() []Row {
	rows := make([]Row, 0, 10)
	for i := 0; i < 10; i++ {
		rows = append(rows, Row{
			Title:   "Rust (video game) - Wikipedia",
			Snippet: "Rust is a multiplayer survival game where players craft tools and build bases.",
			URL:     "https://en.wikipedia.org/wiki/Rust_(video_game)",
		})
	}
	return rows
}

// 健康桶正例：正常回显 query 的后续词。
func rustHealthyRows() []Row {
	return []Row{
		{Title: "The Rust Ownership Model", Snippet: "Ownership and borrowing rules explained.", URL: "https://doc.rust-lang.org/book/ch04-00-understanding-ownership.html"},
		{Title: "Rust Borrow Checker Deep Dive", Snippet: "How the borrow checker enforces ownership at compile time.", URL: "https://blog.example/rust-borrow-checker"},
		{Title: "Understanding Ownership in Rust", Snippet: "Move semantics, borrowing and lifetimes.", URL: "https://blog.example/ownership-rust"},
		{Title: "Rust ownership vs GC languages", Snippet: "Why borrowing replaces garbage collection.", URL: "https://blog.example/rust-gc"},
		{Title: "Common ownership mistakes in Rust", Snippet: "Fixing borrow checker errors.", URL: "https://blog.example/rust-mistakes"},
	}
}

func TestBucketDecoyEnglish(t *testing.T) {
	if sc := Bucket("rust ownership borrowing", rustDecoyRows()); sc >= CoherenceMin {
		t.Fatalf("诱饵桶相干度应 < %.1f，得 %.2f", CoherenceMin, sc)
	}
	if sc := Bucket("rust ownership borrowing", rustHealthyRows()); sc < WitnessMin {
		t.Fatalf("健康桶相干度应 >= %.1f，得 %.2f", WitnessMin, sc)
	}
}

// postgres 诱饵：只命中首词 "postgres"（首页 + 维基）。
func TestBucketDecoyFirstTokenOnly(t *testing.T) {
	rows := []Row{
		{Title: "PostgreSQL: The world's most advanced database", Snippet: "PostgreSQL homepage.", URL: "https://www.postgresql.org/"},
		{Title: "PostgreSQL - Wikipedia", Snippet: "PostgreSQL is an open source database system.", URL: "https://en.wikipedia.org/wiki/PostgreSQL"},
		{Title: "Postgres downloads", Snippet: "Download PostgreSQL for your platform.", URL: "https://www.postgresql.org/download/"},
		{Title: "PostgreSQL tutorial for beginners", Snippet: "Learn PostgreSQL step by step.", URL: "https://tutorial.example/postgresql"},
	}
	if sc := Bucket("postgres explain analyze", rows); sc >= CoherenceMin {
		t.Fatalf("首词诱饵应 < %.1f，得 %.2f（explain/analyze 不该被 URL 里的 postgres 凑数）", CoherenceMin, sc)
	}
}

// 纯中文未分词 query：整串 ≥4 字，首 bigram 做 anchor，rest 从第 3 字起。
func TestBucketCJKBigram(t *testing.T) {
	// 诱饵：只讲"上海"，不提明珠塔观光（bigram 明珠/珠塔/塔观/观光/光攻/攻略）。
	decoy := []Row{
		{Title: "上海市人民政府门户网站", Snippet: "上海市新闻、政务公开与服务。", URL: "https://www.shanghai.gov.cn/"},
		{Title: "上海地铁线路图", Snippet: "上海地铁官方线路查询。", URL: "https://metro.sh.cn/"},
		{Title: "上海天气预报", Snippet: "上海市未来七天天气预报。", URL: "https://weather.example/shanghai"},
		{Title: "上海 Map - Google", Snippet: "上海市地图与卫星图像。", URL: "https://maps.google.com/shanghai"},
	}
	if sc := Bucket("上海明珠塔观光攻略", decoy); sc >= CoherenceMin {
		t.Fatalf("中文诱饵应 < %.1f，得 %.2f", CoherenceMin, sc)
	}
	healthy := []Row{
		{Title: "东方明珠塔观光攻略", Snippet: "上海明珠塔门票与观光层指南。", URL: "https://travel.example/mingzhu"},
		{Title: "上海明珠塔夜景观光", Snippet: "观光走廊与旋转餐厅体验。", URL: "https://travel.example/mingzhu-night"},
		{Title: "明珠塔观光层购票攻略", Snippet: "上海地标明珠塔的游览路线。", URL: "https://travel.example/mingzhu-ticket"},
		{Title: "上海三日游含明珠塔观光", Snippet: "附明珠塔周边攻路推荐。", URL: "https://travel.example/shanghai-3day"},
	}
	if sc := Bucket("上海明珠塔观光攻略", healthy); sc < WitnessMin {
		t.Fatalf("中文健康桶应 >= %.1f，得 %.2f", WitnessMin, sc)
	}
}

// witness 门控：只有一个弱桶、没有健康桶证明"回显可能"时，不判诱饵。
func TestJudgeNoWitnessNoDecoy(t *testing.T) {
	buckets := map[string][]Row{"bing": rustDecoyRows()}
	_, decoy := Judge("rust ownership borrowing", buckets)
	if len(decoy) != 0 {
		t.Fatalf("无 witness 时不得判诱饵，得 %v", decoy)
	}
	// 两个桶、一个健康一个诱饵 → 诱饵成立。
	buckets["duckduckgo"] = rustHealthyRows()
	_, decoy = Judge("rust ownership borrowing", buckets)
	if !decoy["bing"] || decoy["duckduckgo"] {
		t.Fatalf("有 witness 后 bing 应判诱饵：decoy=%v", decoy)
	}
}

// 不可判定的三种形态：<4 行、无法切出 anchor/rest、返回 -1。
func TestBucketUnevaluable(t *testing.T) {
	if sc := Bucket("rust ownership borrowing", rustDecoyRows()[:3]); sc != -1 {
		t.Fatalf("3 行应返回 -1，得 %.2f", sc)
	}
	if sc := Bucket("rust", rustDecoyRows()); sc != -1 {
		t.Fatalf("单词 query 切不出 rest，应返回 -1，得 %.2f", sc)
	}
	if sc := Bucket("量子计算", rustHealthyRows()); sc != -1 {
		t.Fatalf("纯中文短 query（<4 字无 bigram rest）应返回 -1，得 %.2f", sc)
	}
}

// 操作符与排除短语剥离：site:/filetype: 等不参与 anchor/rest。
// 注意 stem 口径与 Python 原版一致："postgres" 削为 "postgr"——子串匹配下
// 仍命中页面里的完整词，这是参考实现的既定行为，不是 bug。
func TestTermsStripsOperators(t *testing.T) {
	anchor, rest := Terms("site:example.com postgres explain analyze")
	if len(anchor) == 0 || anchor[0] != "postgr" {
		t.Fatalf("anchor 应为 postgr（stem 口径），得 %v", anchor)
	}
	joined := strings.Join(rest, " ")
	if !strings.Contains(joined, "explain") || !strings.Contains(joined, "analyz") {
		t.Fatalf("rest 应含 explain/analyz，得 %v", rest)
	}
}

// 短拉丁词整词匹配："gdp" 不得命中 "gdp growth" 之外的误拼。
func TestMatcherWordBoundary(t *testing.T) {
	anchor, rest := Terms("vietnam gdp growth")
	if len(anchor) == 0 || anchor[0] != "vietnam" {
		t.Fatalf("anchor 应为 vietnam，得 %v", anchor)
	}
	re := matcher(rest)
	if re == nil {
		t.Fatal("rest 切出后 matcher 不应为 nil")
	}
	if !re.MatchString(strings.ToLower("GDP grew 6% in Vietnam")) {
		t.Fatalf("正常回显应命中")
	}
	if re.MatchString("gdpq grew") {
		t.Fatalf("整词匹配失败：gdp 命中了 gdpq")
	}
}
