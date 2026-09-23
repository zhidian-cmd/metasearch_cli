package dedup

import (
	"strings"
	"testing"

	"github.com/zhidian-cmd/metasearch_cli/internal/config"
	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

func hit(url, title, snippet string, engines ...string) model.Hit {
	return model.Hit{
		URL: url, Title: title, Snippet: snippet,
		Engine: engines, Positions: map[string]int{},
	}
}

func run(t *testing.T, items []model.Hit) ([]model.Hit, model.DedupStat) {
	t.Helper()
	return Run(items, config.TrackingDefault)
}

func mergedReason(t *testing.T, kept []model.Hit, url string) string {
	t.Helper()
	for _, k := range kept {
		if alt, ok := k.MergedFrom[url]; ok {
			return alt.Reason
		}
	}
	t.Fatalf("没有任何保留项吞并了 %s", url)
	return ""
}

// 备用 URL 字典的契约：被吞并的 URL 是**键**，值里带**它自己的**标题与命中引擎 ——
// 合并 ≠ 消失，调用方随时能把备用入口连同它的来源一起回取。
func TestMergedFromCarriesTitleAndEngine(t *testing.T) {
	items := []model.Hit{
		hit("https://x.com/a", "主条目标题", "正文内容足够长以便比较相似度", "exa"),
		hit("https://x.com/a/", "备用条目标题", "正文内容足够长以便比较相似度", "tavily"),
	}
	kept, _ := run(t, items)
	if len(kept) != 1 {
		t.Fatalf("同页应合并，得到 %d 条", len(kept))
	}
	alt, ok := kept[0].MergedFrom[items[1].URL]
	if !ok {
		t.Fatalf("被吞并的 URL 应作为键出现在 merged_from，实际 %v", kept[0].MergedFrom)
	}
	if alt.Title != "备用条目标题" {
		t.Errorf("备用项标题应是**合并前**它自己的标题，得到 %q", alt.Title)
	}
	if len(alt.Engine) != 1 || alt.Engine[0] != "tavily" {
		t.Errorf("备用项引擎应是它自己的命中引擎，得到 %v", alt.Engine)
	}
	if alt.Reason == "" {
		t.Error("备用项必须带判定理由")
	}
}

// ⚠️ 下面两条钉住**判据硬事实** —— 曾与 Python 侧逐条对齐后确定，对照实现已归档，
// 但事实仍在：谁把字符集/取数改窄了，这两条立刻变红。

// 归一化必须吃掉 Unicode 空白：`&nbsp;`(U+00A0) 与全角空格(U+3000) 在中文网页里遍地都是。
// 同一页被两个引擎返回时，一边是 `&nbsp;`、一边是普通空格 —— 只认 ASCII 空白就判为两页、合不上。
func TestNormTextStripsUnicodeSpaces(t *testing.T) {
	a := NormText("紫色\u00a0面具\u3000是谁\u2002拍的")
	b := NormText("紫色 面具 是谁 拍的")
	if a != b {
		t.Fatalf("Unicode 空白应与 ASCII 空格同被剔除：%q vs %q", a, b)
	}
}

// 取数必须覆盖全角数字：Go 的 `\d` 只是 ASCII `[0-9]`，而 Unicode 的 Nd 类含 `０-９`。
// 用 `\p{Nd}` 才对得上 —— 否则"２０２４"取不到，数字否决闸会漏判（该拦的没拦）。
func TestNumReCoversFullwidthDigits(t *testing.T) {
	got := numRe.FindAllString("２０２４年 2023", -1)
	if len(got) != 2 || got[0] != "２０２４" || got[1] != "2023" {
		t.Fatalf("全角与半角数字都应取出，得到 %v", got)
	}
}

// ========== 契约 0：规范化 URL 全等 → 无条件合并 ==========

// 实测场景（「紫色面具」2026-09-17）：pexels 同一页被两个引擎返回，URL 只差一个尾斜杠，
// 标题却是两套模板（相似度仅 0.12）。靠"两边文本都得像"永远合不上 —— 契约 0 就是为此存在。
func TestContractZeroTrailingSlash(t *testing.T) {
	items := []model.Hit{
		hit("https://www.pexels.com/zh-tw/search/%E7%B4%AB%E8%89%B2%E9%9D%A2%E5%85%B7/",
			"最佳紫色面具相片", "同一段正文", "anysearch"),
		hit("https://www.pexels.com/zh-tw/search/%E7%B4%AB%E8%89%B2%E9%9D%A2%E5%85%B7",
			"最佳紫色面具相片 - Pexels", "同一段正文", "tavily"),
	}
	kept, stat := run(t, items)
	if len(kept) != 1 {
		t.Fatalf("同页（尾斜杠差异）应合并成 1 条，得到 %d 条", len(kept))
	}
	if stat.TotalMerged != 1 {
		t.Fatalf("TotalMerged 应为 1，得到 %d", stat.TotalMerged)
	}
	if len(kept[0].Engine) != 2 {
		t.Fatalf("合并后 engine 应是两家的并集，得到 %v", kept[0].Engine)
	}
	if r := mergedReason(t, kept, items[1].URL); r != "d1:url_exact,same_url" {
		t.Fatalf("留痕理由应为 d1:url_exact,same_url，得到 %q", r)
	}
}

// 同一页的"明码路径"与"百分号路径"：Go 的 EscapedPath 会把两者归一到同一形式，
// 所以这条**不需要**任何解码头就成立（实测 baike 被两个引擎分别给出两种写法）。
func TestContractZeroEncodedPath(t *testing.T) {
	items := []model.Hit{
		hit("https://baike.baidu.com/item/紫色面具/13901580", "紫色面具", "正文甲"),
		hit("https://baike.baidu.com/item/%E7%B4%AB%E8%89%B2%E9%9D%A2%E5%85%B7/13901580", "紫色面具_百度百科", "正文乙"),
	}
	kept, _ := run(t, items)
	if len(kept) != 1 {
		t.Fatalf("编码写法不同的同一页应合并，得到 %d 条", len(kept))
	}
}

// ⚠️ 编码的**分隔符**绝不能被解成真分隔符：/a%2Fb 与 /a/b 是两个不同页面。
func TestEncodedSlashIsNotSamePage(t *testing.T) {
	items := []model.Hit{
		hit("https://x.com/a%2Fb", "标题甲", "正文甲"),
		hit("https://x.com/a/b", "标题乙", "正文乙"),
	}
	kept, _ := run(t, items)
	if len(kept) != 2 {
		t.Fatalf("/a%%2Fb 与 /a/b 是不同页面，不应合并（得到 %d 条）", len(kept))
	}
}

// ⚠️ `srsltid`（Google 购物的广告点击 id）—— **2026-09-18 用真实数据挖出来的漏杀钉子**。
//
// 它不在剥离表里时：URL 不相等 → 走不到契约 0；落到相似度层后 URL 相似度只有 0.409
// （长 token 稀释了 n-gram），而数字否决 `veto:diff_numbers` 又先拦下来（token 里的数字
// 把数字集合撑大，重合度掉到 0.273 < 0.30）→ **同一页被判成两条**。实测 3 对
// （openshop / keychron / philips）。
//
// 补进 minimalTracking 后走契约 0 无条件合并 —— 修的是**索引层**，没有动任何阈值。
func TestTrackingStripsSrsltid(t *testing.T) {
	items := []model.Hit{
		hit("https://shop.example/blog/best-2026", "2026 年推荐清单", "正文甲", "anysearch"),
		hit("https://shop.example/blog/best-2026/?srsltid=AU7gw4WjaTIrf9NIy0hUoFMEtMgte3Pj",
			"2026 年推荐清单", "正文乙", "tavily"),
	}
	kept, _ := run(t, items)
	if len(kept) != 1 {
		t.Fatalf("剥掉 srsltid 后同一页应合并，得到 %d 条（漏杀又回来了？）", len(kept))
	}
	if r := mergedReason(t, kept, items[1].URL); !strings.Contains(r, "url_exact") {
		t.Errorf("应走契约 0 无条件合并，得到理由 %q", r)
	}
	// 反向钉住：参数表里必须真的有它。否则将来被"清理冗余"删掉时，上面那句可能
	// 因为别的路径碰巧通过而**静默失效** —— 那样就只是换了个理由继续漏杀。
	if !config.TrackingSet(config.TrackingDefault)["srsltid"] {
		t.Fatal("srsltid 必须留在 default 档的剥离表里")
	}
	if !config.TrackingSet(config.TrackingMinimal)["srsltid"] {
		t.Error("srsltid 无歧义，minimal 档也应剥离")
	}
}

// ========== 维度一候选 + 维度二判决 ==========

// 同一问题页多一个无副作用参数（?write）：URL 不完全相等（?write 不在 tracking 表里），
// 但标题归一后相同、URL 极像 → 走"url+title_high"这条通过路径。
func TestZhihuWriteParamSameQuestion(t *testing.T) {
	items := []model.Hit{
		hit("https://www.zhihu.com/question/6594412233", "紫色面具是谁拍的？抓住了吗？ - 知乎", "问题页正文"),
		hit("https://www.zhihu.com/question/6594412233?write", "紫色面具是谁拍的？抓住了吗？ - 知乎", "问题页正文"),
	}
	kept, _ := run(t, items)
	if len(kept) != 1 {
		t.Fatalf("同一问题页应合并，得到 %d 条", len(kept))
	}
	r := mergedReason(t, kept, items[1].URL)
	if r == "" {
		t.Fatal("合并必须留痕")
	}
	t.Logf("理由: %s", r)
}

// ⚠️⚠️ **这条是"否决链不能删"的钉子。**
// 京东首页与品牌页的标题归一后**完全相同**（都是"紫色面具品牌及商品"），
// 路径首段却一个是空、一个是 brand —— 只靠标题判等会把两个不同页面合并。
// veto:diff_section 就是拦住它的那道闸。
func TestJDFirstPageVsBrandPageMustNotMerge(t *testing.T) {
	items := []model.Hit{
		hit("https://www.jd.com/", "紫色面具品牌及商品- 京东", "京东首页"),
		hit("https://www.jd.com/brand/6233f225cf60c295954d.html", "紫色面具品牌及商品 - 京东", "品牌页"),
	}
	kept, stat := run(t, items)
	if len(kept) != 2 {
		t.Fatalf("首页 ≠ 内容页，绝不可合并（得到 %d 条）", len(kept))
	}
	if stat.Veto["veto:diff_section"] == 0 {
		t.Fatalf("应记到 veto:diff_section，实际 %v", stat.Veto)
	}
}

// 跨注册域默认不合并 —— 哪怕标题与摘要一字不差（同一篇被两个站转载）。
func TestCrossSiteNotMergedByDefault(t *testing.T) {
	items := []model.Hit{
		hit("https://a.com/post/1", "同一篇文章的标题", "同一段正文内容"),
		hit("https://b.com/other/2", "同一篇文章的标题", "同一段正文内容"),
	}
	kept, stat := run(t, items)
	if len(kept) != 2 {
		t.Fatalf("跨站默认不合并（得到 %d 条）", len(kept))
	}
	if stat.Veto["veto:cross_site"] == 0 {
		t.Fatalf("应记到 veto:cross_site，实际 %v", stat.Veto)
	}
}

// 同站不同子域（www. vs m.）且同栏目、文本一致 → 该合并。
func TestSameSiteSubdomainMerges(t *testing.T) {
	items := []model.Hit{
		hit("https://www.thepaper.cn/newsDetail_forward_1234567", "某新闻标题 - 澎湃新闻", "这是一段足够长的正文内容用于计算相似度以便通过阈值"),
		hit("https://m.thepaper.cn/newsDetail_forward_1234567", "某新闻标题 - 澎湃新闻", "这是一段足够长的正文内容用于计算相似度以便通过阈值"),
	}
	kept, _ := run(t, items)
	if len(kept) != 1 {
		t.Fatalf("同站换子域的同一篇应合并，得到 %d 条", len(kept))
	}
}

// 同站同栏目但**数字不符**（不同分页 id）→ 放过。
func TestDifferentNumbersNotMerged(t *testing.T) {
	items := []model.Hit{
		hit("https://a.com/news/111111.html", "美女图集 - 站点名", "第一页正文内容"),
		hit("https://a.com/news/999999.html", "美女图集 - 站点名", "第二页正文内容"),
	}
	kept, _ := run(t, items)
	if len(kept) != 2 {
		t.Fatalf("数字不符应放过（得到 %d 条）", len(kept))
	}
}

// ========== 规范化函数 ==========

func TestNormURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://Example.com/a/", "example.com/a"},
		{"http://example.com:80/a", "example.com/a"},
		{"https://example.com:443/a", "example.com/a"},
		{"https://example.com/a#frag", "example.com/a"},
		{"https://example.com/a?utm_source=bing", "example.com/a"},
		{"https://example.com/a?b=2&a=1", "example.com/a?a=1&b=2"},
		{"https://example.com/a?a=1&b=2", "example.com/a?a=1&b=2"},
		{"https://example.com/", "example.com/"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormURL(c.in, config.TrackingSet(config.TrackingDefault)); got != c.want {
			t.Errorf("NormURL(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestNormURLTrackingModes(t *testing.T) {
	// spm 在 default 档剥、minimal 档不剥（它可能是有语义的参数）
	if got := NormURL("https://x.com/a?spm=abc", config.TrackingSet(config.TrackingDefault)); got != "x.com/a" {
		t.Errorf("default 档应剥 spm，得到 %q", got)
	}
	if got := NormURL("https://x.com/a?spm=abc", config.TrackingSet(config.TrackingMinimal)); got != "x.com/a?spm=abc" {
		t.Errorf("minimal 档不该剥 spm，得到 %q", got)
	}
}

func TestStripSiteSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"紫色面具_百度百科", "紫色面具"},
		{"最佳紫色面具相片 - Pexels", "最佳紫色面具相片"},
		{"紫色面具是谁拍的？抓住了吗？ - 知乎", "紫色面具是谁拍的？抓住了吗？"},
		// 尾段是句子（含句读）→ 不剥
		{"标题 - 这是一个很长的副标题，不是站点名", "标题 - 这是一个很长的副标题，不是站点名"},
		// 尾段超长 → 不剥
		{"标题 - 一二三四五六七八九十十一十二十三", "标题 - 一二三四五六七八九十十一十二十三"},
		// 左侧没内容 → 不剥
		{"- 知乎", "- 知乎"},
		{"", ""},
	}
	for _, c := range cases {
		if got := StripSiteSuffix(c.in); got != c.want {
			t.Errorf("StripSiteSuffix(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// ⚠️ 去重**没有任何档位入口**：`-dedup` / `-dedup-threshold` 两个开关与 `off` 档
// 都已从源码删除（2026-09-19 连 url/balanced/loose 三个内部预设一并删除）。
// 现在连"传给 Run 一个档位"这件事都做不到 —— 想加回旁路，先过 docs/decisions.md
// 「为什么没有去重开关」那一节。
