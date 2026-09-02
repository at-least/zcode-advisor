# zcode-advisor

ZCode 版的 [Anthropic advisor tool](https://platform.claude.com/docs/en/agents-and-tools/tool-use/advisor-tool)：讓便宜快速的執行模型（glm-5.3-flash）在關鍵時刻向更強的顧問模型（glm-5.3）徵詢戰略建議，用 Go 實作的單一 binary、純標準庫、零外部依賴。

## 系統組成

一個 binary，兩種模式，三個觸發點：

| 觸發點 | 機制 | 時機由誰決定 |
|---|---|---|
| `consult_advisor` 工具（MCP） | 主模型呼叫，自動附完整對話視野＋當前輪獨白 | 主模型自己 |
| 諮詢提醒（`UserPromptSubmit` hook） | prompt 有份量且尚未諮詢過時注入一行提醒，**不打 API** | 規則（原版 nudge 的對應物） |
| 卡關診斷（`PostToolUseFailure` hook） | 連續失敗 ≥2 次才諮詢，5 分鐘冷卻、每 session 上限 5 次 | 規則 |

另有 `PostToolUse` hook（`hook PostToolUseOK`）只重置連續失敗計數，零成本。

設計鐵律：**顧問缺席不能耽誤正事**——API 失敗、缺 key、額度用盡時，MCP 工具回 `isError` 訊息、hook 靜默放行，任何路徑都不擋任務。

## UUID 級 session 歸屬（核心技巧）

Anthropic 原版的 advisor 在 provider 竊聽不到的地方跑，自動拿到完整對話；MCP server 只看得到 stdio。本專案的解法：**rollout 反查**。

ZCode 為每個 session 在 `~/.zcode/cli/rollout/model-io-sess_<uuid>.jsonl` 記錄每一輪完整的模型請求/回應。模型決定呼叫 `consult_advisor` 的那個 response **必然在工具執行前落盤**，因此 server 掃最近活躍的非 subagent 檔，找「最後一行 `response.toolCalls` 裡有 `consult_advisor` 且 `input.question` 與本次收到的完全一致」的檔——它的 `sessionId` 就是呼叫端。多個 ZCode session 並行不會誤判（不靠 mtime 猜測）。

命中後自動組裝顧問 context：

1. **當前輪獨白**（`response.text`，截 4000 字）置頂——executor 來問之前的想法
2. **完整對話視野**（`request.messages`＝executor 模型實際看到的內容；跳過 system prompt、tool_use 只留名稱與截斷參數、保留尾端 48K 字元）
3. 顧問回覆附 `| session <uuid 前8碼>` 標註歸屬

`question` 參數保留作「聚焦透鏡」——比原版的空輸入多了焦點。

## 建置與安裝

```bash
cd ~/.zcode/zcode-advisor
go build -o zcode-advisor .   # Go 1.27+，無需任何依賴下載
```

在 `~/.zcode/cli/config.json` 註冊（注意：config-file hooks 必須 `hooks.enabled: true` 才會跑）：

```json
{
  "mcp": {
    "servers": {
      "zcode-advisor": {
        "type": "stdio",
        "command": "/Users/你的名字/.zcode/zcode-advisor/zcode-advisor",
        "timeoutMs": 120000
      }
    }
  },
  "hooks": {
    "enabled": true,
    "events": {
      "UserPromptSubmit": [
        { "hooks": [{ "type": "command",
            "command": "/Users/你的名字/.zcode/zcode-advisor/zcode-advisor hook UserPromptSubmit",
            "timeoutMs": 10000, "statusMessage": "advisor 諮詢提醒" }] }
      ],
      "PostToolUseFailure": [
        { "hooks": [{ "type": "command",
            "command": "/Users/你的名字/.zcode/zcode-advisor/zcode-advisor hook PostToolUseFailure",
            "timeoutMs": 120000, "statusMessage": "advisor 卡關診斷…" }] }
      ],
      "PostToolUse": [
        { "hooks": [{ "type": "command",
            "command": "/Users/你的名字/.zcode/zcode-advisor/zcode-advisor hook PostToolUseOK",
            "timeoutMs": 10000 }] }
      ]
    }
  }
}
```

重啟 ZCode session 生效。工具名為 `mcp__zcode-advisor__consult_advisor`，參數 `question`（必填）＋ `context`（選填，只補對話裡還沒有的材料）。

建議一併在使用者指示檔（`~/.zcode/AGENTS.md`）加入顧問使用守則——「定位不算實質工作、定下做法前與宣稱完成前各問一次、建議為強先驗、衝突帶回顧問裁決」，完整版見 dotfiles 內的實作。

## API Key 解析

優先順序（hook 進程拿不到 MCP config 的 env，故有多層回退）：

1. 環境變數 `ZAI_API_KEY`
2. `~/.secrets/secrets.env`（支援 `export` 前綴、引號、前導空白、註解），候選順序：`ADVISOR_KEY_NAME` 指定名 → `Z_AI_API_KEY_PRO` → `Z_AI_API_KEY_MAX` → `ZAI_API_KEY`
3. config 的 `mcp.servers.zcode-advisor.env.ZAI_API_KEY`

**注意：config 的 `env` 不要放佔位符**（如 `sk-REPLACE-ME`）——config 的 env 會注入 server 進程、在解析鏈中享最高優先，實測曾因此遮蔽 secrets 鏈導致 401。現已雙重防護：程式碼會把含 REPLACE/YOUR_ 字樣的值視同未設定，但別依賴它。key 永遠不進 git。

預設端點是 **GLM Coding Plan 專用**：`https://api.z.ai/api/coding/paas/v4/chat/completions`（Coding Plan 的 key 打按量計費的 `api/paas/v4` 只會得到「餘額不足」）。key 永遠不進 git。

## 環境變數

| 變數 | 預設 | 說明 |
|---|---|---|
| `ZAI_API_KEY` | – | 直接指定 key（最高優先） |
| `ADVISOR_KEY_NAME` | – | 指定 secrets.env 裡優先使用的變數名 |
| `ADVISOR_API_URL` | coding plan 端點 | 改打其他 OpenAI 相容端點 |
| `ADVISOR_MODEL` | `glm-5.3` | 顧問模型 |
| `ADVISOR_MAX_TOKENS` | `2048` | 顧問輸出上限 |
| `ADVISOR_MAX_USES` | `0`（不限） | MCP 工具每 session 呼叫上限 |
| `ADVISOR_ROLLOUT_TAIL` | `48000` | 對話帶入上限（字元）；`0` 關閉自動附對話 |
| `ADVISOR_ROLLOUT_DIR` | `~/.zcode/cli/rollout` | 測試用覆蓋 |

節流常數（改程式碼中的 `hooks.go` 常數區）：提醒上限 3 次/session、prompt ≥40 字才提醒、卡關門檻連續 2 次失敗、冷卻 5 分鐘、卡關預算 5 次/session。

## 檔案

- `main.go` — MCP server（JSON-RPC 子集：initialize / ping / tools/list / tools/call）、`askAdvisor`（顧問 API 呼叫）、key 解析鏈
- `hooks.go` — 三個 hook 處理器、session 狀態檔（`state/<sess>.state.json`：提醒/失敗/卡關/已諮詢計數）、提醒文字
- `rollout.go` — UUID 反查、對話壓縮、當前輪獨白抽取

執行期產物（皆已 gitignore）：`zcode-advisor` binary、`state/`、`hooks-debug.log`（記錄每次 hook 的原始輸入，供驗證 ZCode 實際欄位名）。

## 與 Anthropic 原版的差異

- **對齊**：executor 自主決定時機、完整對話自動附上（含當前輪輸出）、降級哲學、輸出上限、nudge（逐字仿作＋timing 教育）、max_uses
- **不可能**（用戶端天險）：生成中途暫停續寫（原版在 provider 推理迴圈內）、`tool_choice` 強制諮詢
- **超出原版**：卡關自動求診（連續失敗探測）、諮詢後提醒收聲、跨 session 並行的 UUID 歸屬、顧問無狀態（原版的記憶只是重讀 transcript 的副產品；衝突靠「帶回去裁決」的守則解決）

## 煙霧測試

```bash
# 協議
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}' \
  | ./zcode-advisor

# 端到端（會真的打 API，max_tokens 太小會因思考段擠掉可見文字而回空內容）
echo '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"consult_advisor","arguments":{"question":"回覆OK"}}}' \
  | ADVISOR_MAX_TOKENS=512 ./zcode-advisor

# hook（不打 API）
echo '{"prompt":"…40字以上的任務描述…","session_id":"s1"}' | ./zcode-advisor hook UserPromptSubmit
```

疑難排解：hook 沒觸發 → 先確認 `hooks.enabled: true`，再看 `hooks-debug.log` 有無記錄（有 config 問題參照 ZCode 的 diagnosing-hooks 指南）；MCP 連不上 → Settings → MCP 看狀態，config-file server 的 schema 是嚴格的（未知欄位會整個被丟棄）、路徑必須絕對。
