package codegraph

import (
	"strings"
	"testing"
	"time"

	gts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// === issue #233 回归 ===
//
// 解析被预算掐断时,gotreesitter 返回的是**残缺但看起来正常**的树:Parse 的 err 是 nil、
// RootNode().HasError() 也是 false。旧实现只检查这两样,于是把半棵树当完整结果收下 ——
// 8739 行的 Rust 文件只索引出 1 个符号(恰好是文件里第一个定义),而且毫无迹象。
//
// 半份符号比没有更糟:CodeGraph 会自信地回答"查不到这个函数",模型据此以为它不存在;
// 而返回空结果时模型会退回 Grep,至少能找到。所以残缺树一律整份丢弃。

const truncTestSrc = `pub struct Alpha { a: u32 }

pub fn beta() -> u32 { 1 }

pub enum Gamma { A, B }

fn delta(x: &Receipt, json: bool) -> Result<()> {
    if json {
        match x.operation {
            Op::List => println!("{}", to_pretty(&x.records)?),
            Op::Status => match x.records.first() {
                Some(r) => println!("{}", to_pretty(r)?),
                None if x.is_error() => {}
                None => println!("{}", to_pretty(x)?),
            },
            _ => println!("{}", to_pretty(x)?),
        }
    }
    Ok(())
}
`

// parseRustWith 用指定预算解析,返回 definition 数和库报告的停止原因。
func parseRustWith(t *testing.T, src string, micros uint64) (defs int, rt gts.ParseRuntime) {
	t.Helper()
	e := grammars.DetectLanguage("x.rs")
	if e == nil {
		t.Skip("rust 语法未注册")
	}
	lang := e.Language()
	p := gts.NewParser(lang)
	if micros > 0 {
		p.SetTimeoutMicros(micros)
	}
	tree, err := p.Parse([]byte(src))
	if err != nil || tree == nil {
		t.Fatalf("Parse 失败: err=%v tree=%v", err, tree)
	}
	tg, err := gts.NewTagger(lang, grammars.ResolveTagsQuery(*e))
	if err != nil {
		t.Fatalf("NewTagger 失败: %v", err)
	}
	for _, tag := range tg.TagTree(tree) {
		if strings.HasPrefix(tag.Kind, "definition.") {
			defs++
		}
	}
	return defs, tree.ParseRuntime()
}

// TestTruncatedParse_IsSilent 钉住"截断是静默的"这个前提 —— 修复正是冲它去的。
// 这条一旦失败(库开始返回 err 或标 HasError),说明可以简化上面的判断。
func TestTruncatedParse_IsSilent(t *testing.T) {
	defsFull, rtFull := parseRustWith(t, truncTestSrc, 0)
	if defsFull < 3 {
		t.Fatalf("不设预算时应解析出全部定义, got %d", defsFull)
	}
	if rtFull.StopReason != gts.ParseStopAccepted {
		t.Fatalf("不设预算时应正常结束, got %v", rtFull.StopReason)
	}

	// 掐到极小 → 必然截断
	defsCut, rtCut := parseRustWith(t, truncTestSrc, 50)
	if rtCut.StopReason == gts.ParseStopAccepted {
		t.Skip("50µs 未能触发截断(机器太快),跳过")
	}
	if defsCut >= defsFull {
		t.Errorf("截断后符号数应减少: 完整 %d, 截断 %d", defsFull, defsCut)
	}
	// 这才是坑所在:err 和 HasError 都干净,只有 StopReason 说了实话
	t.Logf("截断时 StopReason=%v Truncated=%v —— 而 Parse 的 err 为 nil",
		rtCut.StopReason, rtCut.Truncated)
}

// TestParse_RejectsTruncatedTree 核心断言:解析被掐断时,tsParser.Parse 必须返回空结果,
// 而不是残缺的符号表。
func TestParse_RejectsTruncatedTree(t *testing.T) {
	if grammars.DetectLanguage("x.rs") == nil {
		t.Skip("rust 语法未注册")
	}
	p := parserFor("x.rs")
	if p == nil {
		t.Skip("rust parser 未注册")
	}

	// 正常预算:应拿到全部符号
	res, err := p.Parse("x.rs", []byte(truncTestSrc))
	if err != nil {
		t.Fatalf("正常解析不应报错: %v", err)
	}
	if len(res.Symbols) < 3 {
		t.Fatalf("正常预算下应索引出全部符号, got %d", len(res.Symbols))
	}
	full := len(res.Symbols)

	// 把预算压到必然截断:期望"整份丢弃"而不是"残缺入库"
	before := TruncatedParses()
	restore := setParseBudgetForTest(50 * time.Microsecond)
	res2, err2 := p.Parse("x.rs", []byte(truncTestSrc))
	restore()

	if err2 != nil {
		t.Fatalf("截断不该以 error 形式返回(与既有超时处置一致): %v", err2)
	}
	if len(res2.Symbols) != 0 {
		t.Errorf("解析被掐断时必须整份丢弃,got %d 个符号(完整时 %d)——"+
			"半份符号会让 CodeGraph 自信地答『查不到』,比返回空更糟", len(res2.Symbols), full)
	}
	if got := TruncatedParses(); got <= before {
		t.Errorf("被跳过的文件应计入 TruncatedParses(排查靠它),before=%d after=%d", before, got)
	}
}

// TestParseTimeout_ScalesWithSize 预算随体量放宽 —— 固定 1 秒对几百 KiB 的真实源文件不够,
// 而撞线的后果是静默残缺(issue #233)。
func TestParseTimeout_ScalesWithSize(t *testing.T) {
	cases := []struct{ size int }{{0}, {50 << 10}, {100 << 10}, {317 << 10}, {1 << 20}}
	prev := time.Duration(0)
	for _, c := range cases {
		got := parseTimeout(c.size)
		if got < baseParseTimeout {
			t.Errorf("%d 字节: 预算 %v 低于起步值 %v", c.size, got, baseParseTimeout)
		}
		if got < prev {
			t.Errorf("%d 字节: 预算 %v 小于更小文件的 %v(应单调不减)", c.size, got, prev)
		}
		prev = got
	}
	// 报告人那个 317 KiB 的文件:1 秒不够,放宽后应有明显余量
	if got := parseTimeout(317 << 10); got <= baseParseTimeout {
		t.Errorf("317 KiB 应比起步预算宽裕, got %v", got)
	}
	// 上限有 maxFileSize(1 MiB)兜着,最坏预算不该失控
	if got := parseTimeout(maxFileSize); got > 15*time.Second {
		t.Errorf("最大文件的预算 %v 过大,后台索引会被单文件拖太久", got)
	}
}

// setParseBudgetForTest 临时把解析预算改成固定值,返回还原函数。
func setParseBudgetForTest(d time.Duration) func() {
	prev := parseBudgetOverride.Load()
	parseBudgetOverride.Store(int64(d))
	return func() { parseBudgetOverride.Store(prev) }
}
