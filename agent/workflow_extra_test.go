package agent

import "testing"

func TestExtractJSONBlock_BalancedAmongProse(t *testing.T) {
	cases := []struct{ in, want string }{
		// 前后都有含 }/] 的解释文字 → 只取中间配平的对象
		{`解释一下:{"a":1,"b":[2,3]} 完毕}]`, `{"a":1,"b":[2,3]}`},
		// 字符串字面量内的括号不参与计数
		{`{"msg":"a} b]"}`, `{"msg":"a} b]"}`},
		// ```json 围栏
		{"```json\n{\"x\":1}\n```", `{"x":1}`},
		// 数组
		{`prefix [1,2,{"k":"v"}] suffix`, `[1,2,{"k":"v"}]`},
	}
	for _, c := range cases {
		if got := extractJSONBlock(c.in); got != c.want {
			t.Errorf("extractJSONBlock(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWorkflowJournal_SaltIsolatesNestedCalls(t *testing.T) {
	home := t.TempDir()
	src := `export const meta={name:"child"};`
	args := []byte(`{"x":1}`)
	// 同名+同 args,但不同 salt(模拟父里两次嵌套调用同一子 workflow)→ 各自独立 journal。
	a, _, _ := loadWorkflowJournal(home, "child", src, args, "c0")
	a.Put(0, "from-first")
	b, cached, _ := loadWorkflowJournal(home, "child", src, args, "c1")
	if cached != 0 {
		t.Fatalf("不同 salt 应独立(cached=0),却命中了 %d 条缓存", cached)
	}
	if _, ok := b.Get(0); ok {
		t.Fatal("第二次嵌套调用不应命中第一次的缓存")
	}
}
