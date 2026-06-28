package tui

import (
	"reflect"
	"testing"
)

// reconcileAttachedImages 按输入框残留的 [Image #N] 占位符裁剪待发图片列表(issue #146:删不掉)。
func TestReconcileAttachedImages(t *testing.T) {
	imgs := []string{"/a.png", "/b.png", "/c.png"} // #1 #2 #3

	cases := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "全部保留",
			text: "看看 [Image #1] [Image #2] [Image #3]",
			want: []string{"/a.png", "/b.png", "/c.png"},
		},
		{
			name: "删中间一张",
			text: "看看 [Image #1] [Image #3]",
			want: []string{"/a.png", "/c.png"},
		},
		{
			name: "删首张",
			text: "[Image #2] [Image #3]",
			want: []string{"/b.png", "/c.png"},
		},
		{
			name: "全删光",
			text: "纯文字,占位符都删了",
			want: []string{},
		},
		{
			name: "顺序无关,按编号对应",
			text: "[Image #3] 在前 [Image #1] 在后",
			want: []string{"/a.png", "/c.png"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reconcileAttachedImages(c.text, imgs)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("reconcileAttachedImages(%q) = %v, want %v", c.text, got, c.want)
			}
		})
	}
}

func TestReconcileAttachedImages_Empty(t *testing.T) {
	// 没有附件时原样返回,不 panic。
	if got := reconcileAttachedImages("随便什么 [Image #1]", nil); len(got) != 0 {
		t.Fatalf("空附件应返回空,got %v", got)
	}
}
