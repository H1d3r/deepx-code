package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIFunctionSpec_RawParameters(t *testing.T) {
	// RawParameters 非空 → 原样作为 parameters。
	f := OpenAIFunctionSpec{
		Name: "structured_output", Description: "d",
		RawParameters: json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array"}}}`),
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"parameters":{"type":"object","properties":{"findings":{"type":"array"}}}`) {
		t.Fatalf("RawParameters 应原样作 parameters,实际: %s", s)
	}
}

func TestOpenAIFunctionSpec_TypedParameters(t *testing.T) {
	// 无 RawParameters → 按 ToolParam 序列化(现有工具不变)。
	f := OpenAIFunctionSpec{
		Name: "Read", Description: "d",
		Parameters: ToolParam{Type: "object", Properties: map[string]PropDef{"path": {Type: "string"}}, Required: []string{"path"}},
	}
	b, _ := json.Marshal(f)
	s := string(b)
	if !strings.Contains(s, `"parameters":{"type":"object"`) || !strings.Contains(s, `"path"`) {
		t.Fatalf("ToolParam 应正常序列化,实际: %s", s)
	}
}
