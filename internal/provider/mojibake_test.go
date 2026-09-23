package provider

import "testing"

// mojibake 修复：只接受无损往返，失败必须原样返回。
//
// 真实语料取自 2026-09-17 直连 api.exa.ai 的响应（query「福建兄妹」）：
// 同一响应里 title 正常、contents.text 坏，全引擎扫描确认只有 exa 中招。
func TestRepairMojibake(t *testing.T) {
	// ① 可无损还原：UTF-8 被当 Latin-1 解码的典型形态。
	//    "现象级..." 的 UTF-8 字节 E7 8E B0 ... 被当作 Latin-1 后成为
	//    "çŽ°è±¡..." 这类字符；这里用能完整往返的例子。
	lossless := []struct{ in, want string }{
		// "中文" = E4 B8 AD E6 96 87 → Latin-1 解读为 "ä¸­æ\u0096\u0087"
		{"ä¸­æ\u0096\u0087", "中文"},
		// "测试" = E6 B5 8B E8 AF 95 → "æµ\u008bè¯\u0095"
		{"æµ\u008bè¯\u0095", "测试"},
		// "福建兄妹" = E7 A6 8F E5 BB BA E5 85 84 E5 A6 B9
		{"ç¦\u008få»ºå\u0085\u0084å¦¹", "福建兄妹"},
		// 混排：ASCII + 中文
		{"abc ä¸­æ\u0096\u0087 xyz", "abc 中文 xyz"},
	}
	for _, c := range lossless {
		if got := repairMojibake(c.in); got != c.want {
			t.Errorf("应还原: %q -> 期望 %q，实得 %q", c.in, c.want, got)
		}
	}

	// ② 必须原样返回（不可还原 / 本就不是 mojibake / 已损坏）。
	mustKeep := []struct {
		name, in string
	}{
		{"纯 ASCII", "hello world"},
		{"正常中文", "福建兄妹的搜索结果"},
		{"正常中英混排", "Apple 苹果公司 2026 年财报"},
		{"空串", ""},
		{"已含替换符（上游已做过有损替换，不可逆）", "abc \uFFFD def"},
		{"非 Latin-1 字符（不像 mojibake）", "日本語テキスト"},
		{"单个高位字符但无 UTF-8 前导", "\u00b7"},
		// exa 实测的**真丢字节**样本：`â uu` 处少了 0xC3，解码必失败。
		// 这类必须原样保留，绝不能用替换符凑字。
		{"真丢字节的 exa 样本", "ç°è±¡çº§ççº¢ï¼âç¦å»ºåå¦¹âuuç»å"},
	}
	for _, c := range mustKeep {
		if got := repairMojibake(c.in); got != c.in {
			t.Errorf("%s: 必须原样返回 %q，但被改成了 %q", c.name, c.in, got)
		}
	}
}

// repairMojibake 在 cleanSnippet 里被调用，必须保证：
// 修复不引入 markdown 清洗的回归，且失败时是恒等变换。
func TestCleanSnippetWithMojibake(t *testing.T) {
	// 正常文本仍走原路径：markdown 图片被删、链接降级。
	if got := cleanSnippet("见[夸克百科](https://baike.quark.cn/x)说明"); got != "见夸克百科说明" {
		t.Errorf("链接降级回归: %q", got)
	}
	if got := cleanSnippet("正文![alt](./a.png)结束"); got != "正文结束" {
		t.Errorf("图片清理回归: %q", got)
	}
	// ASCII 摘要不受影响（走快速路径）。
	if got := cleanSnippet("  plain   text  "); got != "plain text" {
		t.Errorf("ASCII 空白归一回归: %q", got)
	}
}
