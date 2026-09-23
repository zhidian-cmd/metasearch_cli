package provider

import (
	"strings"
	"testing"
)

// 媒体页 / 无正文卡片页判定，必须覆盖三个位置。
// 用户 2026-09-16 两次报告漏网（先是 page.sm.cn 视频页，后是 p.quark.cn 字词卡片），
// 这个表就是防它俩回潮。
func TestIsQuarkMedia(t *testing.T) {
	mustDrop := []string{
		// 1) m.quark.cn 媒体垂直
		"https://m.quark.cn/vsearch/picture?q=%E7%8C%AB",
		"https://m.quark.cn/vsearch/video?q=%E7%8C%AB",
		"https://m.quark.cn/s?q=%E7%8C%AB&qtab=image",
		"https://m.quark.cn/s?q=%E7%8C%AB&qtab=video",
		"https://m.quark.cn/s?q=%E7%8C%AB&qtab=picture",
		// 2) page.sm.cn 媒体页（第一次漏网）
		"https://page.sm.cn/blm/video-page-710/video?h=www.bilibili.com&q=%E7%8C%AB",
		"https://page.sm.cn/blm/image-page-710/image?q=%E7%8C%AB",
		"https://page.sm.cn/blm/picture-page-710/pic?q=%E7%8C%AB",
		"https://page.sm.cn/blm/audio-page-710/audio?q=%E7%8C%AB",
		"https://page.sm.cn/blm/some-page/x?h=v1.user_auth_video.quark.cn",
		// 用户 2026-09-16 贴出的两条真实 URL（同一路径的两个 h= 变体）
		"https://page.sm.cn/blm/video-page-710/video?h=www.bilibili.com&id=26_2a7e3c",
		"https://page.sm.cn/blm/video-page-710/video?h=v1.user_auth_video.quark.cn&id=27_cecb56",
		"https://page.sm.cn/blm/some-page/x?h=www.douyin.com/xxx",
		// 3) p.quark.cn 字词/实体卡片（第二次漏网）
		"https://p.quark.cn/d146ffa7/char?entity=%E7%8C%AB&force_uc_biz_str=1&content_id=490659623706034176",
		"https://p.quark.cn/d146ffa7/word?entity=%E9%A3%8E%E6%99%AF&force_uc_biz_str=1&content_id=490649280571244544",
		"https://p.quark.cn/char",
		"https://p.quark.cn/word",
	}
	for _, u := range mustDrop {
		if !isQuarkMedia(u) {
			t.Errorf("应剔除但放过了: %s", u)
		}
	}

	mustKeep := []string{
		// 真百科内容：必须保留
		"https://baike.quark.cn/baike?id=93bc4b12c385495c9696e73fbcedc3af",
		"https://baike.baidu.com/item/%E5%BE%AE%E7%94%9F%E7%89%A9/147527",
		// page.sm.cn 的文本聚合页（midpage）——混合域里的非媒体页
		"https://page.sm.cn/blm/midpage-317/index?q=%E7%8C%AB",
		// 外部站，一律不动
		"https://www.im.cas.cn/",
		"https://www.yixue.com/%E5%BE%AE%E7%94%9F%E7%89%A9",
		"https://www.bilibili.com/video/BV1xx",
		// p.quark.cn 上非 char/word 的路径（未知类型默认保留）
		"https://p.quark.cn/d146ffa7/detail?id=1",
		// m.quark.cn 普通搜索页（无媒体标识）
		"https://m.quark.cn/s?q=%E7%8C%AB",
	}
	for _, u := range mustKeep {
		if isQuarkMedia(u) {
			t.Errorf("应保留但误杀了: %s", u)
		}
	}
}

// 中转壳丢弃 + 外部 URL 永不改动（"宁放过不杀错"的硬约束）。
func TestIsTransitShellAndCleanURL(t *testing.T) {
	shells := []string{
		"https://m.quark.cn/s/FXMQWSU78A23QjucKg?q=%E7%8C%AB",
	}
	for _, u := range shells {
		if !isTransitShell(u) {
			t.Errorf("应判为中转壳: %s", u)
		}
	}
	keep := []string{
		"https://m.quark.cn/s?q=%E7%8C%AB", // 普通搜索页，不是 /s/<id>
		"https://www.example.com/s/abc",    // 外部站，不碰
	}
	for _, u := range keep {
		if isTransitShell(u) {
			t.Errorf("误判为中转壳: %s", u)
		}
	}

	// cleanQuarkShellURL 只剥自家域跟踪参数；外部 URL 一个字节都不许动。
	ext := "https://www.example.com/p?uc_param_str=xxx&from=yyy"
	if got := cleanQuarkShellURL(ext); got != ext {
		t.Errorf("外部 URL 被改动了: %s -> %s", ext, got)
	}
	shell := "https://baike.quark.cn/baike?id=abc&uc_param_str=xxx&from=yyy&bucket=z"
	got := cleanQuarkShellURL(shell)
	if got != "https://baike.quark.cn/baike?id=abc" {
		t.Errorf("自家域跟踪参数未剥干净: %s", got)
	}

	// ⚠️ `sid` 是**翻页会话 id**：同一张卡片在 p1/p2/p3 里只差它。不剥会让同批结果
	// 重复 2~3 次，并让"某页零新增就停翻页"失效（2026-09-19 实测，单引擎 6.91s→2.80s）。
	doc := "https://vt.quark.cn/blm/quark-doc-ssr-293/preview?id=ABC123&sid=deadbeef&q=xxx&doc_title=yyy"
	got = cleanQuarkShellURL(doc)
	if got != "https://vt.quark.cn/blm/quark-doc-ssr-293/preview?id=ABC123&q=xxx&doc_title=yyy" {
		t.Errorf("sid 未被剥掉（同卡片会重复计入）: %s", got)
	}
}

// 摘要里的 markdown 噪声必须清掉（exa/tavily 实测会带）。
// 真实案例来自用户 2026-09-16 报告：某条结果的摘要中间插了
// `!\n[](./W020231010319429517399.png)`。
func TestCleanSnippet(t *testing.T) {
	in := "会上，白莉对全国食品微生物检验工作目的进行讲解。\n!\n[](./W020231010319429517399.png)\n会后，与会专家进行了探讨。"
	got := cleanSnippet(in)
	if strings.Contains(got, "![") || strings.Contains(got, "W020231010319517399") {
		t.Errorf("图片语法未清干净: %q", got)
	}
	if !strings.Contains(got, "白莉") || !strings.Contains(got, "与会专家") {
		t.Errorf("正文被误删: %q", got)
	}

	// markdown 链接降级为纯文本，保留可读文字
	if got := cleanSnippet("见[夸克百科](https://baike.quark.cn/x)说明"); got != "见夸克百科说明" {
		t.Errorf("链接降级不正确: %q", got)
	}

	// 无 markdown 的普通摘要只做空白归一，内容不变
	if got := cleanSnippet("  普通  摘要  "); got != "普通 摘要" {
		t.Errorf("空白归一出错: %q", got)
	}
	if got := cleanSnippet(""); got != "" {
		t.Errorf("空串应原样返回: %q", got)
	}
}
