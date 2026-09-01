// rollout 反查：辨識「是哪個 session 在呼叫 consult_advisor」。
// 呼叫工具的那個 response 必然已落盤（工具執行晚於 response 完成），
// 因此在最近活躍的非 subagent rollout 檔中，找「最後一行帶有 question 完全一致的
// consult_advisor toolCall」的檔，以其 sessionId 做辨識——不猜 mtime、不依賴環境變數，
// 多個 ZCode session 並行時也能正確歸屬。
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	rolloutDir  = envOr("ADVISOR_ROLLOUT_DIR", defaultRolloutDir())
	rolloutTail = envInt("ADVISOR_ROLLOUT_TAIL", 48000) // 對話帶入上限（字元）；0 = 關閉
)

func defaultRolloutDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".zcode", "cli", "rollout")
}

type rolloutMatch struct {
	SessionID string
	Dialog    string // 壓縮、截尾後的對話文字（role: content）
	Preamble  string // executor 呼叫工具前的當前輪獨白（response.text）
	Path      string
}

func findCallingSession(question string) (rolloutMatch, bool) {
	if rolloutTail == 0 {
		return rolloutMatch{}, false
	}
	entries, err := os.ReadDir(rolloutDir)
	if err != nil {
		return rolloutMatch{}, false
	}
	type cand struct {
		path string
		mod  time.Time
	}
	var files []cand
	cutoff := time.Now().Add(-10 * time.Minute)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "model-io-sess_") || !strings.HasSuffix(name, ".jsonl") ||
			strings.Contains(name, "subagent") {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().After(cutoff) {
			files = append(files, cand{filepath.Join(rolloutDir, name), fi.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })

	for i, f := range files {
		if i >= 10 { // 防禦性上限：只查最近活躍的 10 個檔
			break
		}
		line, ok := lastCompleteLine(f.path)
		if !ok {
			continue
		}
		var rec struct {
			SessionID string `json:"sessionId"`
			Request   struct {
				Messages json.RawMessage `json:"messages"`
			} `json:"request"`
			Response struct {
				Text string `json:"text"`
				ToolCalls []struct {
					Name  string         `json:"name"`
					Input map[string]any `json:"input"`
				} `json:"toolCalls"`
			} `json:"response"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		matched := false
		for _, c := range rec.Response.ToolCalls {
			if strings.Contains(c.Name, "consult_advisor") {
				if q, _ := c.Input["question"].(string); q == question {
					matched = true
					break
				}
			}
		}
		if !matched {
			continue
		}
		return rolloutMatch{
			SessionID: rec.SessionID,
			Dialog:    condenseMessages(rec.Request.Messages, rolloutTail),
			Preamble:  truncate(strings.TrimSpace(rec.Response.Text), 4000),
			Path:      f.path,
		}, true
	}
	return rolloutMatch{}, false
}

// lastCompleteLine 讀檔尾最多 16MB，從尾端往回找第一個完整可解析的行；
// 視窗開頭被截斷的段落會因 json.Valid 失敗被自然跳過。
func lastCompleteLine(path string) ([]byte, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	const window = 16 << 20
	size := fi.Size()
	w := size
	if w > window {
		w = window
	}
	buf := make([]byte, w)
	if _, err := f.ReadAt(buf, size-w); err != nil && err != io.EOF {
		return nil, false
	}
	segs := bytes.Split(buf, []byte("\n"))
	for i := len(segs) - 1; i >= 0; i-- {
		s := bytes.TrimSpace(segs[i])
		if len(s) == 0 || !json.Valid(s) {
			continue
		}
		return s, true
	}
	return nil, false
}

// condenseMessages 把 request.messages 壓成純文字對話；跳過 system（體積大、對顧問
// 幫助小），tool_use 只留名稱與截斷的參數，最後保留尾端（最新內容）不超過 capChars。
func condenseMessages(raw json.RawMessage, capChars int) string {
	if len(raw) == 0 {
		return ""
	}
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msgs) != nil {
		return ""
	}
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		text := contentText(m.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		b.WriteString(m.Role + ": " + text + "\n")
	}
	s := b.String()
	if len(s) > capChars {
		s = s[len(s)-capChars:]
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:] // 對齊行首，避免半行開頭
		}
	}
	return s
}

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if t, _ := blk["text"].(string); strings.TrimSpace(t) != "" {
			b.WriteString(t + "\n")
		} else if name, _ := blk["name"].(string); name != "" {
			in, _ := json.Marshal(blk["input"])
			b.WriteString("[tool_use " + name + " " + truncate(string(in), 200) + "]\n")
		}
	}
	return b.String()
}
