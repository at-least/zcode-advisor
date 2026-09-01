// hook 模式：advisor-server hook <Event>，由 ZCode 的 hooks 機制以 stdin 餵入事件 JSON。
// 只在規則抓得住的關鍵時刻諮詢顧問——任務開場（有份量的 prompt）與連續工具失敗（卡關），
// 其餘事件一律靜默放行；顧問失敗也靜默放行，絕不擋工作。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	reminderBudget      = 3                // 每個 session 最多提醒幾次
	openPromptMinRunes  = 40               // prompt 低於此長度視為瑣碎請求，不打擾
	stuckFailThreshold  = 2                // 連續失敗幾次視為卡關
	stuckCooldown       = 5 * time.Minute  // 兩次卡關診斷的最小間隔
	stuckBudget         = 5                // 每個 session 最多幾次卡關診斷
	stateStaleAfter     = 30 * time.Minute // 拿不到 session id 時的計數有效期
)

func runHook(event string) {
	log.SetFlags(0)
	log.SetPrefix("advisor-hook: ")

	stdin, _ := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	debugLog(event, stdin) // 首次上線後可由此檢視 ZCode 實際餵入的欄位

	var m map[string]any
	_ = json.Unmarshal(stdin, &m)

	switch event {
	case "UserPromptSubmit":
		hookUserPromptSubmit(m)
	case "PostToolUseFailure":
		hookPostToolUseFailure(m)
	case "PostToolUseOK": // 工具成功：重置連續失敗計數（零成本，不打 API）
		st := loadState(sessionKey(m))
		st.Fail = 0
		saveState(sessionKey(m), st)
	default:
		log.Printf("unknown hook event: %s", event)
	}
}

// hookUserPromptSubmit：任務開場提醒——原版 nudge 的對應物。prompt 有份量且本 session
// 尚未用過 consult_advisor 時注入一行提醒，不打 API、不等顧問；要不要問、何時問由主模型自己決定。
func hookUserPromptSubmit(m map[string]any) {
	prompt, _ := m["prompt"].(string)
	if utf8.RuneCountInString(strings.TrimSpace(prompt)) < openPromptMinRunes {
		return
	}
	sess := sessionKey(m)
	st := loadState(sess)
	if st.Consulted || st.Open >= reminderBudget {
		return
	}
	st.Open++
	saveState(sess, st)
	// 文字仿原版 nudge：事實開頭＋條件式判準（不明的設計取捨／未排除的失敗模式）＋
	// timing 教育（定位不算實質工作、定下做法前要問）。我們在 turn 0 注入，
	// 靠文字教模型「先定位再問」，補原版 turn-2 nudge 的時點優勢。
	emitContext("UserPromptSubmit",
		"【advisor 提醒】你還沒諮詢過 advisor（consult_advisor 工具：更強的顧問模型，"+
			"呼叫時會自動附上你目前的完整對話）。定位工作可以先做——讀檔、搜尋、了解現況之後再問不遲；"+
			"但如果任務有不明的設計取捨、或你尚未排除的失敗模式，請在定下做法、開始修改之前諮詢。"+
			"卡住、考慮換方向、或自認完成時，也值得再問一次。")
}

// hookPostToolUseFailure：連續工具失敗 = 卡關訊號。達門檻才諮詢，有冷卻與預算。
func hookPostToolUseFailure(m map[string]any) {
	sess := sessionKey(m)
	st := loadState(sess)
	st.Fail++
	if st.Fail < stuckFailThreshold {
		saveState(sess, st)
		return
	}
	if st.Stuck >= stuckBudget {
		saveState(sess, st)
		return
	}
	if time.Since(time.Unix(st.StuckAt, 0)) < stuckCooldown {
		saveState(sess, st)
		return
	}
	st.Fail = 0
	st.Stuck++
	st.StuckAt = time.Now().Unix()
	saveState(sess, st)

	payload, _ := json.Marshal(m)
	q := "A coding agent's tool calls keep failing; it appears stuck. Latest failed tool event (JSON):\n" +
		truncate(string(payload), 2000) +
		"\n\nDiagnose likely causes and advise: what to check, what to try next, and when to stop and report to the user. Under 150 words, plain text."
	advice, err := askAdvisor(q, "")
	if err != nil {
		log.Printf("stuck advice skipped: %v", err)
		return
	}
	emitContext("PostToolUseFailure", "【advisor 卡關建議 · "+model+"】\n"+advice)
}

// emitContext 輸出 ZCode hook 的 additionalContext 格式；事件名錯了會被嚴格 schema 拒掉（無害）。
func emitContext(event, text string) {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     event,
			"additionalContext": text,
		},
	}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	fmt.Println(string(b))
}

// ---- session 狀態（計數用，壞了也只影響節流） ----

type hookState struct {
	TS        int64 `json:"ts"`        // 最後寫入時間；default 案例靠它判斷新鮮度
	Open      int   `json:"open"`      // 提醒已發出次數
	Fail      int   `json:"fail"`      // 連續工具失敗次數
	Stuck     int   `json:"stuck"`     // 卡關診斷已用次數
	StuckAt   int64 `json:"stuck_at"`  // 上次卡關診斷時間（unix 秒）
	Consulted bool  `json:"consulted"` // 本 session 是否已用過 consult_advisor（提醒收聲）
}

func statePath(sess string) string {
	return filepath.Join(stateDir(), sess+".state.json")
}

func stateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".zcode", "zcode-mcp-advisor", "state")
}

func loadState(sess string) hookState {
	var st hookState
	b, err := os.ReadFile(statePath(sess))
	if err == nil {
		_ = json.Unmarshal(b, &st)
	}
	// 新鮮度重置只適用於拿不到 session id 的 "default" 案例；
	// 真實 session id 每次都是新的，計數不該跨 session 被誤清。
	if sess == "default" && time.Since(time.Unix(st.TS, 0)) > stateStaleAfter {
		st = hookState{}
	}
	return st
}

func saveState(sess string, st hookState) {
	st.TS = time.Now().Unix()
	_ = os.MkdirAll(stateDir(), 0o700)
	b, _ := json.Marshal(st)
	_ = os.WriteFile(statePath(sess), b, 0o600)
}

// sessionKey：env 的 CLAUDE_SESSION_ID 優先，其次 stdin 的 session_id，
// 都沒有就用 "default"（靠 stateStaleAfter 避免跨 session 累計）。
func sessionKey(m map[string]any) string {
	s := os.Getenv("CLAUDE_SESSION_ID")
	if s == "" {
		s, _ = m["session_id"].(string)
	}
	if s == "" {
		return "default"
	}
	return sanitizeSession(s)
}

// markConsulted：MCP 工具被呼叫過即標記，開場提醒就此收聲。
// 諮詢成敗都算數——提醒的目的是確認主模型知道工具存在，呼叫過就達成了。
func markConsulted(sessionID string) {
	if sessionID == "" {
		return
	}
	k := sanitizeSession(sessionID)
	st := loadState(k)
	st.Consulted = true
	saveState(k, st)
}

func sanitizeSession(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 64 {
			break
		}
	}
	return b.String()
}

// debugLog：把每次 hook 的原始輸入留檔，供第一次上線時確認實際欄位名；超過 2MB 就重開。
func debugLog(event string, stdin []byte) {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".zcode", "zcode-mcp-advisor", "hooks-debug.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > 2<<20 {
		_ = os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "=== %s %s ===\n%s\n", event, time.Now().Format(time.RFC3339), stdin)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
