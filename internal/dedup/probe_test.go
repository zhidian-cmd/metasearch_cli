//go:build probe

package dedup

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/zhidian-cmd/metasearch_cli/internal/config"
	"github.com/zhidian-cmd/metasearch_cli/internal/model"
)

// loadProbeHits 读一份结果 JSON，并把每条的 `MergedFrom` 清空。
//
// ⚠️ **必须清**：CLI 默认输出的是"已去重"的结果，里面每条都带着**上一轮**写下的 merged_from。
// 拿它再跑一次去重，会把历史记录混进本轮结果 ——
// 实测踩过：一条 bilibili 的尾斜杠 URL 被误报成"本轮多删了"。
// `MergedFrom` 只写不读、不参与任何判据，清空安全。
func loadProbeHits(t *testing.T, path string) []model.Hit {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	var doc struct {
		Results []model.Hit `json:"results"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}
	for i := range doc.Results {
		doc.Results[i].MergedFrom = nil
	}
	return doc.Results
}

// TestProbeRealData 拿真实检索结果离线跑去重 —— 不联网、不惊动任何引擎，
// 专门用来回答"某个 query 的结果里到底有几条是真重复、被合成了什么、判据耗时多少"。
//
// 用法（JSON 由 `metasearch_cli <词> -format=json > x.json` 产出）：
//
//	DEDUP_PROBE_JSON=/path/to/x.json go test -tags probe -run TestProbeRealData -v ./internal/dedup/
//
// 没有档位参数：判据强度固定（见 DefaultOptions），本探针只回答"这份数据里到底有几条真重复"。
//
// ⚠️ **口径**：CLI 已恒开去重且无任何档位开关，新产出的 JSON 都是"已去重"的 —— 拿它再跑会
// **低估**合并量（被吞的条目已不在 results 里，光清 merged_from 救不回来）。要复现历史基线，
// 请用去重开关还在时留下的存档（`D:/桌面/core_search/data/raw_*.json`）。
func TestProbeRealData(t *testing.T) {
	path := os.Getenv("DEDUP_PROBE_JSON")
	if path == "" {
		t.Skip("未设 DEDUP_PROBE_JSON，跳过")
	}
	hits := loadProbeHits(t, path)

	start := time.Now()
	kept, stat := Run(hits, config.TrackingDefault)
	elapsed := time.Since(start)

	t.Logf("去重模式 %s：%d → %d（合并 %d），判据耗时 %v",
		stat.Mode, stat.Raw, stat.Final, stat.TotalMerged, elapsed)
	for _, s := range stat.Stages {
		t.Logf("  阶段 %-16s %3d → %3d（合并 %d）", s.Stage, s.Before, s.After, s.Merged)
	}
	if len(stat.Veto) > 0 {
		t.Logf("  被否决: %v", stat.Veto)
	}
	for _, k := range kept {
		for u, alt := range k.MergedFrom {
			t.Logf("  保留: %s\n     └ 备用: %s  《%s》  [%v]  %s",
				k.URL, u, alt.Title, alt.Engine, alt.Reason)
		}
	}
}
