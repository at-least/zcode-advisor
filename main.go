// advisor 是一個極簡的 MCP stdio server：向 ZCode 的主模型（executor）提供
// consult_advisor 工具，背後打 GLM API，讓更強的顧問模型給出戰略建議。
// 對應 Anthropic advisor tool 的精神：顧問失敗時降級放行、輸出有上限、時機由主模型自己決定。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	serverName    = "zcode-mcp-advisor"
	serverVersion = "1.0.0"
	defaultModel  = "glm-5.3"
	// Coding Plan 專用端點（GLM Coding Plan 的 key 不走 api/paas/v4 的按量計費端點）
	defaultAPIURL = "https://api.z.ai/api/coding/paas/v4/chat/completions"
)

var (
	apiKey  = resolveAPIKey() // hook 模式拿不到 mcp.servers 的 env，需從 config 回退讀取
	apiURL  = envOr("ADVISOR_API_URL", defaultAPIURL)
	model   = envOr("ADVISOR_MODEL", defaultModel)
	maxTok  = envInt("ADVISOR_MAX_TOKENS", 2048)
	maxUses = envInt("ADVISOR_MAX_USES", 0) // 0 = 不限次數

	useCount atomic.Int32 // MCP 工具的 max_uses 計數（server 為單執行緒迴圈，atomic 僅示意）

	httpClient = &http.Client{Timeout: 100 * time.Second}
)

// resolveAPIKey：環境變數 ZAI_API_KEY 優先（MCP server 模式由 config 的 env 提供），
// 其次解析 ~/.secrets/secrets.env（候選名可用 ADVISOR_KEY_NAME 指定，預設找
// Z_AI_API_KEY_PRO/_MAX、ZAI_API_KEY），最後回 config.json。
// 佔位符視同未設定——config 留著 sk-REPLACE-ME 之類的值不會遮蔽 secrets 鏈。
// key 只有這幾個地方，永遠不進 git。
func resolveAPIKey() string {
	if v := os.Getenv("ZAI_API_KEY"); isRealKey(v) {
		return v
	}
	if m := parseSecretsEnv(); len(m) > 0 {
		for _, name := range keyCandidates() {
			if v := m[name]; isRealKey(v) {
				return v
			}
		}
	}
	if v := apiKeyFromConfig(); isRealKey(v) {
		return v
	}
	return ""
}

// isRealKey：非空且不是佔位符/範本字樣才算有效 key。
func isRealKey(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	lv := strings.ToUpper(v)
	return !strings.Contains(lv, "REPLACE") && !strings.Contains(lv, "YOUR_") &&
		!strings.Contains(lv, "YOUR-")
}

func keyCandidates() []string {
	// Coding Plan 實測：_PRO 現在有額度、_MAX 待重置、ZAI_API_KEY 的值已過期（換新值後可往前調）
	names := []string{"Z_AI_API_KEY_PRO", "Z_AI_API_KEY_MAX", "ZAI_API_KEY"}
	if n := os.Getenv("ADVISOR_KEY_NAME"); n != "" {
		names = append([]string{n}, names...)
	}
	return names
}

// parseSecretsEnv 解析 KEY=VALUE 格式的 secrets 檔；容許 export 前綴、
// 前導/尾端空白、單雙引號值與註解行。
func parseSecretsEnv() map[string]string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(home, ".secrets", "secrets.env"))
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		i := strings.Index(line, "=")
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		v := strings.TrimSpace(line[i+1:])
		v = strings.Trim(v, `"'`)
		if k != "" && v != "" {
			m[k] = v
		}
	}
	return m
}

func apiKeyFromConfig() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	var cfg struct {
		Mcp struct {
			Servers map[string]struct {
				Env map[string]string `json:"env"`
			} `json:"servers"`
		} `json:"mcp"`
	}
	b, err := os.ReadFile(filepath.Join(home, ".zcode", "cli", "config.json"))
	if err != nil {
		return ""
	}
	if json.Unmarshal(b, &cfg) == nil {
		return cfg.Mcp.Servers["zcode-mcp-advisor"].Env["ZAI_API_KEY"]
	}
	return ""
}

// ---- MCP wire types（只用得到的子集）----

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // nil 代表 notification，不需回應
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type toolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callToolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var tools = []toolDefinition{{
	Name: "consult_advisor",
	Description: "Consult a stronger advisor model (" + model + ") for strategic guidance. " +
		"Use it when starting a complex or unfamiliar task, before a large/risky change, " +
		"when stuck after failed attempts, or when unsure about the approach. " +
		"Your current conversation is attached automatically — focus the question on what you need decided, " +
		"and use the optional context field only for material not yet in the conversation.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{
				"type":        "string",
				"description": "What you want the advisor to decide or advise on.",
			},
			"context": map[string]any{
				"type":        "string",
				"description": "Optional supporting material: relevant code, error messages, or a summary of attempts so far.",
			},
		},
		"required": []string{"question"},
	},
}}

func main() {
	if len(os.Args) > 2 && os.Args[1] == "hook" {
		runHook(os.Args[2])
		return
	}
	runServer()
}

func runServer() {
	log.SetFlags(0)
	log.SetPrefix("advisor: ")

	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for {
		line, err := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if resp := handleLine(line); resp != nil {
				writeJSON(out, resp)
				out.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("stdin error: %v", err)
			}
			return // client 斷線，正常結束
		}
	}
}

// handleLine 回傳要寫回 client 的回應；notification 或可忽略的訊息回傳 nil。
func handleLine(line []byte) map[string]any {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return rpcErrorReply(nil, -32700, "parse error: "+err.Error())
	}
	if req.Method == "" { // 不是 request 也不是已知 notification，忽略
		return nil
	}

	switch {
	case req.Method == "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		v := params.ProtocolVersion
		if v == "" {
			v = "2025-06-18"
		}
		return resultReply(req.ID, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		})

	case strings.HasPrefix(req.Method, "notifications/"):
		return nil // initialized 等 notification，一律不回應

	case req.Method == "ping":
		return resultReply(req.ID, map[string]any{})

	case req.Method == "tools/list":
		return resultReply(req.ID, map[string]any{"tools": tools})

	case req.Method == "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return rpcErrorReply(req.ID, -32602, "invalid params: "+err.Error())
		}
		if params.Name != "consult_advisor" {
			return rpcErrorReply(req.ID, -32602, "unknown tool: "+params.Name)
		}
		question, _ := params.Arguments["question"].(string)
		contextStr, _ := params.Arguments["context"].(string)
		if strings.TrimSpace(question) == "" {
			return resultReply(req.ID, callToolResult{
				Content: []textContent{{Type: "text", Text: "error: 'question' is required and must be non-empty"}},
				IsError: true,
			})
		}
		return resultReply(req.ID, consult(question, contextStr))

	default:
		if req.ID == nil {
			return nil // 未知的 notification，安全忽略
		}
		return rpcErrorReply(req.ID, -32601, "method not found: "+req.Method)
	}
}

// consult 呼叫顧問模型。任何失敗都以 isError 的工具結果回傳（主模型看得到、可自行繼續），
// 絕不讓整個 server 掛掉——對應原版 advisor「顧問失敗不得讓任務失敗」的設計。
func consult(question, contextStr string) callToolResult {
	if maxUses > 0 && int(useCount.Add(1)) > maxUses {
		return callToolResult{
			Content: []textContent{{Type: "text", Text: fmt.Sprintf("error: advice budget exhausted (max %d consults for this session); proceed on your own", maxUses)}},
			IsError: true,
		}
	}
	if apiKey == "" {
		return callToolResult{
			Content: []textContent{{Type: "text", Text: "error: ZAI_API_KEY is not set; fill it in ~/.zcode/cli/config.json (mcp.servers.zcode-mcp-advisor.env) and restart the session"}},
			IsError: true,
		}
	}
	// 反查呼叫端 session（UUID 級）：自動帶入該 session 的對話作為顧問 context
	adviceContext := contextStr
	sessionNote := ""
	if m, ok := findCallingSession(question); ok {
		log.Printf("rollout match: session=%s file=%s", m.SessionID, m.Path)
		markConsulted(m.SessionID) // 已諮詢：開場提醒就此收聲
		if m.Dialog != "" || m.Preamble != "" {
			var b strings.Builder
			if m.Preamble != "" { // 當前輪獨白：executor 來問之前的想法，顧問最該先看
				b.WriteString("The agent's words immediately before calling you:\n" + m.Preamble + "\n\n")
			}
			if m.Dialog != "" {
				b.WriteString("The calling agent's current conversation (system prompt omitted, oldest first, may be truncated):\n" + m.Dialog)
			}
			header := b.String()
			if strings.TrimSpace(contextStr) != "" {
				adviceContext = header + "\n--- additional context from the agent ---\n" + contextStr
			} else {
				adviceContext = header
			}
		}
		if len(m.SessionID) > 8 {
			m.SessionID = m.SessionID[:8]
		}
		sessionNote = " | session " + m.SessionID
	}
	advice, err := askAdvisor(question, adviceContext)
	if err != nil {
		return adviceError("error: " + err.Error())
	}
	return callToolResult{
		Content: []textContent{{Type: "text", Text: "[advisor · " + model + sessionNote + "]\n" + advice}},
	}
}

// askAdvisor 打顧問 API，MCP 工具與 hook 模式共用。任何錯誤以 error 回傳，由呼叫端決定呈現方式。
func askAdvisor(question, contextStr string) (string, error) {
	if apiKey == "" {
		return "", fmt.Errorf("ZAI_API_KEY is not set")
	}
	userMsg := question
	if strings.TrimSpace(contextStr) != "" {
		userMsg += "\n\n--- context ---\n" + contextStr
	}
	payload := map[string]any{
		"model":       model,
		"max_tokens":  maxTok,
		"temperature": 0.3,
		"messages": []map[string]string{
			{
				"role": "system",
				"content": "You are a senior engineering advisor consulted by a coding agent mid-task. " +
					"Answer with concise, actionable advice: key risks, recommended approach, and how to verify. " +
					"Under 300 words. Do not restate the question. Plain text only.",
			},
			{"role": "user", "content": userMsg},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("advisor API unreachable: %w", err)
	}
	defer resp.Body.Close()

	var data struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		return "", fmt.Errorf("advisor API returned HTTP %d with unreadable body: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if data.Error != nil && data.Error.Message != "" {
			msg += ": " + data.Error.Message
		}
		return "", fmt.Errorf("advisor API failed: %s", msg)
	}
	if len(data.Choices) == 0 || data.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("advisor returned an empty response")
	}
	return data.Choices[0].Message.Content, nil
}

func adviceError(msg string) callToolResult {
	return callToolResult{Content: []textContent{{Type: "text", Text: msg}}, IsError: true}
}

// ---- 回應組裝 ----

func resultReply(id json.RawMessage, result any) map[string]any {
	if id == nil {
		return nil
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

// rpcErrorReply 不檢查 id 是否為 nil：parse error 依規範要回 id:null，
// 其餘呼叫端已先過濾 notification。
func rpcErrorReply(id json.RawMessage, code int, msg string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": rpcError{Code: code, Message: msg}}
}

func writeJSON(w *bufio.Writer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("marshal response: %v", err)
		return
	}
	w.Write(b)
	w.WriteByte('\n')
}

// ---- env helpers ----

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("invalid %s=%q, using %d", key, v, fallback)
	}
	return fallback
}
