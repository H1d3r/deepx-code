package codegraph

import (
	"os"
	"path/filepath"
	"testing"
)

// 写一个临时 Go 文件,索引后验证符号 / 定义 / 引用 / 文件结构都对。
func TestGoIndex(t *testing.T) {
	dir := t.TempDir()
	src := `package demo

import "fmt"

// Greeter 打招呼。
type Greeter struct {
	Name string
}

func (g *Greeter) Hello() string {
	return fmt.Sprintf("hi %s", g.Name)
}

func New(name string) *Greeter {
	return &Greeter{Name: name}
}

const Version = "1.0"
`
	if err := os.WriteFile(filepath.Join(dir, "demo.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := NewIndex(dir).Graph()
	if err != nil {
		t.Fatal(err)
	}

	// def: 方法用限定名定位
	if defs := g.Def("Greeter.Hello"); len(defs) != 1 || defs[0].Kind != KindMethod {
		t.Fatalf("Greeter.Hello def = %+v, 期望 1 个 method", defs)
	}
	// def: 类型
	if defs := g.Def("Greeter"); len(defs) != 1 || defs[0].Kind != KindType {
		t.Fatalf("Greeter def = %+v, 期望 1 个 type", defs)
	}
	// symbols + kind 过滤:应能找到 const Version
	if hits, _ := g.FindSymbols("Version", KindConst, 10); len(hits) != 1 {
		t.Fatalf("FindSymbols Version/const = %+v, 期望 1 条", hits)
	}
	// refs:Greeter 至少被引用 3 次(receiver、New 返回、复合字面量)
	if refs, total := g.Refs("Greeter", 100); total < 3 {
		t.Fatalf("Greeter refs total = %d (%+v), 期望 ≥3", total, refs)
	}
	// outline:demo.go 至少含 type/method/func/const/field 多个符号
	if out := g.Outline("demo.go"); len(out) < 5 {
		t.Fatalf("outline demo.go = %d 符号, 期望 ≥5", len(out))
	}

	// callees:New 调用了 ... 其实 New 没调别人;改测 Hello 调用了 Sprintf 和 get
	callees, _ := g.Callees("Greeter.Hello", 50)
	var callsSprintf bool
	for _, e := range callees {
		if e.To == "Sprintf" {
			callsSprintf = true
		}
	}
	if !callsSprintf {
		t.Fatalf("Greeter.Hello 的 callees 应含 Sprintf, got %+v", callees)
	}
	// callers:Sprintf 被 Hello 调用
	callers, total := g.Callers("Sprintf", 50)
	if total < 1 {
		t.Fatalf("Sprintf 应至少被调用 1 次, got %+v", callers)
	}
	// imports:demo.go import 了 fmt
	imps := g.Imports("demo.go")
	if len(imps) != 1 || imps[0].To != "fmt" {
		t.Fatalf("demo.go imports 应为 [fmt], got %+v", imps)
	}
}
