package agent

import (
	"crypto/sha256"
	"deepx/workflow"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// openWorkflowJournal 按 (script 名 + args + 脚本内容 + salt) 打开/新建 resume journal,
// 返回它、已缓存步骤数、以及是否成功落盘(persisted=false 表示目录不可写,本次无法 resume)。
// 总返回非 nil journal(不可写时退化为「只在内存」,不报错、不 panic)。
//
// salt 用于区分「同一父 workflow 里对同名+同 args 子 workflow 的多次嵌套调用」——否则它们会
// 共用同一 journal 文件、第二次命中第一次的缓存。顶层传 ""。
func openWorkflowJournal(script *workflow.Script, args any, salt string) (*workflowJournal, int, bool) {
	home, _ := os.UserHomeDir()
	argsJSON, _ := json.Marshal(args)
	return loadWorkflowJournal(home, script.Name, script.Source, argsJSON, salt)
}

// workflowJournal 是 resume 的磁盘存储:把每个成功 agent 的结果按执行序号(seq)落盘成 JSONL,
// 中断/失败后重跑同一 workflow(同名 + 同 args + 同脚本)时,已完成的 seq 命中缓存不再真跑。
//
// 文件:~/.deepx/workflows/.journal/<name>.<argsHash>.jsonl
//   - 第一行 header:{"script_hash":"…"}(脚本变了则旧 journal 作废,从头来)
//   - 其后每行一条:{"seq":N,"result":"…"}(追加写,O(1)/条,中断安全)
//
// 实现 workflow.ResultStore 接口。Get/Put 并发安全(parallel 会并发回调)。
type workflowJournal struct {
	path string
	mu   sync.Mutex
	m    map[int]string
}

// jsonl 行类型
type wfJournalEntry struct {
	Seq    int    `json:"seq"`
	Result string `json:"result"`
}

// loadWorkflowJournal 打开/新建一个 journal,返回它、「已缓存的步骤数」(>0 表示这是一次 resume)、
// 以及是否成功落盘(persisted)。脚本变化(scriptHash 不匹配)→ 旧结果作废、文件重置。
func loadWorkflowJournal(home, name, scriptSource string, argsJSON []byte, salt string) (*workflowJournal, int, bool) {
	dir := filepath.Join(home, ".deepx", "workflows", ".journal")
	argsSum := sha256.Sum256(argsJSON)
	scriptHash := hashHex(scriptSource, 16)
	fname := fmt.Sprintf("%s.%s", name, hex.EncodeToString(argsSum[:])[:8])
	if salt != "" {
		fname += "." + salt // 区分同名+同 args 的多次嵌套调用
	}
	path := filepath.Join(dir, fname+".jsonl")

	j := &workflowJournal{path: path, m: map[int]string{}}
	header := `{"script_hash":"` + scriptHash + `"}`

	resumed := false
	if data, err := os.ReadFile(path); err == nil {
		lines := strings.Split(string(data), "\n")
		var h struct {
			ScriptHash string `json:"script_hash"`
		}
		if len(lines) > 0 && json.Unmarshal([]byte(lines[0]), &h) == nil && h.ScriptHash == scriptHash {
			for _, ln := range lines[1:] {
				if strings.TrimSpace(ln) == "" {
					continue
				}
				var e wfJournalEntry
				if json.Unmarshal([]byte(ln), &e) == nil {
					j.m[e.Seq] = e.Result
				}
			}
			resumed = true
		}
	}
	if resumed {
		return j, len(j.m), true // 已有可用文件,落盘可用
	}
	// 全新运行或脚本已变:重置文件,只写 header。写失败 → persisted=false(本次无法 resume)。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return j, 0, false
	}
	if err := os.WriteFile(path, []byte(header+"\n"), 0o644); err != nil {
		return j, 0, false
	}
	return j, 0, true
}

func (j *workflowJournal) Get(seq int) (string, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.m[seq]
	return r, ok
}

func (j *workflowJournal) Put(seq int, result string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.m[seq] = result
	line, err := json.Marshal(wfJournalEntry{Seq: seq, Result: result})
	if err != nil {
		return
	}
	// 追加写(O_APPEND):中断也不丢已写的;每条一行。
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

// Discard 删除整个 journal(workflow 成功跑完后调用,使下次是全新运行)。
func (j *workflowJournal) Discard() {
	if j == nil {
		return
	}
	_ = os.Remove(j.path)
}

func hashHex(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	h := hex.EncodeToString(sum[:])
	if n > 0 && n < len(h) {
		return h[:n]
	}
	return h
}
