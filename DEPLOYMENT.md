# inputd 部署與整合文件

## 目錄
1. [系統概覽](#系統概覽)
2. [硬體設定](#硬體設定)
3. [快速啟動](#快速啟動)
4. [系統架構](#系統架構)
5. [設定檔](#設定檔)
6. [管理介面](#管理介面)
7. [Control API](#control-api)
8. [事件格式](#事件格式)
9. [APP 整合](#app-整合)
10. [鍵盤按鍵對照](#鍵盤按鍵對照)
11. [故障排除](#故障排除)
12. [開發說明](#開發說明)

---

## 系統概覽

`inputd` 是一個執行在 Ubuntu 24.04 Desktop NUC 上的 Go 常駐程式，負責：

- 識別兩把實體鍵盤，分別對應 `primary_input`（主要操作員）與 `secondary_input`（次要操作員）兩個固定角色
- 自動偵測並加入新出現的鍵盤至 `secondary_input`，支援遠端操作（RustDesk 虛擬鍵盤）不需任何設定
- 將按鍵事件標準化後透過 Unix socket 推送給 APP
- 提供 Web UI 讓現場設定人員管理鍵盤綁定，無需操作底層

APP 端只需連接 event socket，不直接存取 `/dev/input`。若後端服務封裝在 Docker container 內，建議仍由 host `inputd` 負責硬體輸入層，container 後端只掛載並讀取 `/run/inputd/input.sock`。

不建議 Docker 後端直接掛載並讀取 `/dev/input`，理由如下：

- container 需要額外處理 device cgroup、Linux input 裝置權限與 group id 對應。
- 若使用 `privileged: true` 會放大 container 權限邊界，能看到所有輸入裝置。
- `/dev/input/eventX` 會因重開機、USB 拔插或換 port 改變，後端必須自行處理 udev symlink 與 reconnect。
- 後端會被迫實作 raw evdev 解析、keycode normalize、鍵盤角色辨識、熱插拔與防止誤抓滑鼠/一般鍵盤。
- 多個服務同時讀 raw device 時，若任一方使用 `EVIOCGRAB`，可能造成其他 consumer 收不到事件。

因此 production 建議採用：host `inputd` 負責硬體層，Docker 後端負責業務層。後端只需要連 socket、解析 JSON line、處理斷線重連。

---

## 硬體設定

### 已接入裝置

| 角色 | 識別方式 | 穩定路徑 |
|---|---|---|
| `primary_input` | 名稱含 `XFFP` 或 `XFKEY`（glob 匹配） | `/dev/input/primary_keypad` |
| `secondary_input` | 具備鍵盤功能且名稱不含 `XFFP`/`XFKEY`（fallback） | `/dev/input/secondary_keypad` |
| `secondary_input`（動態） | 任何新出現的鍵盤，且未被其他 role 的 symlink 佔用 | 自動偵測（`/dev/input/eventX`） |

> 硬體型號與 VID:PID 不影響識別，可直接更換同類硬體而無需修改任何設定。

### udev 規則（零接觸自動綁定 Zero-Touch Auto-Binding）

位於 `/etc/udev/rules.d/`，系統開機自動建立穩定 symlink。為了實現遠端部署而無需現場人員手動綁定，採用名稱模糊匹配與 Fallback 機制。

> **注意：** 檔名必須大於 `60`（例如 `99-`），確保在系統內建的 `60-input-id.rules` 執行之後再執行，否則 `ENV{ID_INPUT_KEYBOARD}` 會是空值而導致規則無效。

**`99-primary_keypad.rules`** (主要操作員專用鍵盤)
```
SUBSYSTEM=="input", KERNEL=="event*", ENV{ID_INPUT_KEYBOARD}=="1", ATTRS{name}=="*XFFP*", \
  SYMLINK+="input/primary_keypad", GROUP="ops", MODE="0660"
SUBSYSTEM=="input", KERNEL=="event*", ENV{ID_INPUT_KEYBOARD}=="1", ATTRS{name}=="*XFKEY*", \
  SYMLINK+="input/primary_keypad", GROUP="ops", MODE="0660"
```

> 只要裝置名稱包含 "XFFP" 或 "XFKEY" 且具備鍵盤功能（`ENV{ID_INPUT_KEYBOARD}=="1"` 對應於 `Handlers=kbd`），即綁定為主要操作員鍵盤。換接 USB port 不受影響。

**`99-secondary_keypad.rules`** (次要操作員鍵盤 fallback)
```
SUBSYSTEM=="input", KERNEL=="event*", ENV{ID_INPUT_KEYBOARD}=="1", ATTRS{name}!="*XFFP*", ATTRS{name}!="*XFKEY*", \
  SYMLINK+="input/secondary_keypad", GROUP="ops", MODE="0660"
```

> 利用 `!=` 排除主要操作員鍵盤。這意味著系統上任何「具備鍵盤功能且名稱不包含 XFFP / XFKEY」的 USB 裝置（例如 Logitech K400 Plus 或 SEM Keyboard），都會自動被歸類為次要操作員鍵盤。

### 裝置權限

兩個裝置節點屬於 `ops` group（mode 0660）。daemon 以 root 執行，可直接讀取。

---

## 快速啟動

### 安裝（首次或升級）

```bash
cd /root/inputd
bash deploy/install.sh
```

腳本會自動完成：build、udev rules、journald 設定、config（首次）、systemd service 啟動。

### 檢查服務狀態

```bash
systemctl status inputd
```

### 確認裝置健康

```bash
# 全部裝置 online → HTTP 200；任一離線 → HTTP 503
curl -s http://127.0.0.1:17888/v1/health | python3 -m json.tool
```

### 確認角色綁定

```bash
curl -s http://127.0.0.1:17888/v1/roles | python3 -m json.tool
```

正常回應：兩個 role 的 `online` 均為 `true`。

### 開啟管理介面

在 NUC 本機或同網段瀏覽器開啟：

```
http://127.0.0.1:17888         # 本機
http://<NUC-IP>:17888          # 區域網路 / RustDesk 遠端
```

---

## 系統架構

```mermaid
graph TD
    subgraph 實體硬體
        K1[主要操作員實體鍵盤<br>XFFP XFKEY]
        K2[次要操作員實體鍵盤<br>SEM Keyboard]
        K3[遠端鍵盤<br>RustDesk Virtual / 任意鍵盤]
    end

    subgraph Linux OS
        U1[/udev: /dev/input/primary_keypad/]
        U2[/udev: /dev/input/secondary_keypad/]
        U3[/udev: /dev/input/eventX<br>動態裝置]
        UW[NETLINK_KOBJECT_UEVENT<br>kernel uevent]
    end

    subgraph Daemon
        ID((inputd daemon))
        RD[udev watcher<br>goroutine]
    end

    subgraph 介面與應用程式
        S1[Unix Socket<br>/run/inputd/input.sock]
        S2[Unix Socket<br>/run/inputd/control.sock]
        W1[Web UI<br>127.0.0.1:17888]
        APP[APP<br>Node.js 訂閱事件流]
        CLI[CLI / 內部設定腳本]
        ADM[現場人員管理介面]
    end

    K1 --> U1
    K2 --> U2
    K3 --> U3

    U1 -->|evdev read| ID
    U2 -->|evdev read| ID
    UW -->|add/remove event| RD
    U3 -->|evdev read| RD
    RD -->|自動加入 secondary_input| ID

    ID -->|發佈 JSON 鍵盤事件| S1
    ID <-->|設定管理 API| S2
    ID -->|提供前端頁面| W1

    S1 -->|即時監聽| APP
    S2 <--> CLI
    W1 <--> ADM
```

### 檔案路徑

| 項目 | 路徑 |
|---|---|
| Binary | `/usr/local/bin/inputd` |
| 設定檔 | `/etc/inputd/config.yaml` |
| Event socket | `/run/inputd/input.sock` |
| Control socket | `/run/inputd/control.sock` |
| Web UI | `http://127.0.0.1:17888` |
| systemd service | `/etc/systemd/system/inputd.service` |
| Logs | `journalctl -u inputd` |

---

## 設定檔

路徑：`/etc/inputd/config.yaml`

```yaml
version: 1

transport:
  event_socket: /run/inputd/input.sock
  control_socket: /run/inputd/control.sock

roles:
  primary_input:
    stable_path: /dev/input/primary_keypad
    device_id: primary_keypad
    grab: false
  secondary_input:
    stable_path: /dev/input/secondary_keypad
    device_id: secondary_keypad
    grab: false
    auto_discover: true
```

### 欄位說明

| 欄位 | 說明 |
|---|---|
| `stable_path` | daemon 開啟的裝置路徑，應使用 udev 建立的 symlink |
| `device_id` | 應用層識別值，出現在每個事件的 `device_id` 欄位 |
| `grab` | `true` 啟用 EVIOCGRAB 獨占，按鍵事件不再流入桌面；預設 `false` |
| `auto_discover` | `true` 啟用 udev 熱插拔自動發現；所有新出現且未被其他 role 佔用的鍵盤，都會自動加入此 role；`grab` 設定同樣套用於自動發現的裝置 |

修改設定後通知 daemon 重載：

```bash
curl -s -X POST http://127.0.0.1:17888/v1/config/reload
# 或
kill -HUP $(systemctl show -p MainPID inputd | cut -d= -f2)
```

---

## 管理介面

開啟 `http://127.0.0.1:17888`，畫面顯示兩個角色卡片。

### 重新綁定鍵盤（Learn Mode）

1. 點擊欲更換的角色卡片上的「**重新綁定**」
2. 出現 15 秒倒數提示
3. 在目標鍵盤上按任意鍵
4. 顯示「✓ 綁定成功」，設定自動存檔

> **注意**：若同一把鍵盤原本綁定在另一個角色，系統會自動解除舊綁定再套用新角色。

### 狀態指示

| 顏色 | 意義 |
|---|---|
| 🟢 綠點 | 裝置連線中 |
| 🔴 紅點 | 裝置離線（已綁定但拔除） |
| ⚪ 灰點 | 未綁定 |
| 🔵 藍點閃爍 | Learn mode 進行中 |

---

## Control API

Base URL：`http://127.0.0.1:17888` 或 Unix socket `--unix-socket /run/inputd/control.sock http://localhost`

### 端點總覽

| Method | 路徑 | 說明 |
|---|---|---|
| `GET` | `/v1/status` | daemon 狀態與版本 |
| `GET` | `/v1/health` | 裝置健康狀態；全 online → 200，任一離線 → 503 |
| `GET` | `/v1/devices` | 已設定的裝置清單與連線狀態 |
| `GET` | `/v1/roles` | 目前角色綁定狀態 |
| `POST` | `/v1/roles/{role}/assign` | 指定裝置綁定到角色 |
| `DELETE` | `/v1/roles/{role}` | 清除角色綁定 |
| `POST` | `/v1/learn/{role}/start` | 啟動 learn mode |
| `POST` | `/v1/learn/stop` | 中止 learn mode |
| `POST` | `/v1/config/reload` | 重新載入設定檔 |
| `GET` | `/v1/events` | SSE 即時事件流（瀏覽器 / curl -N） |
| `GET` | `/v1/autodiscover` | 目前 auto-discover 綁定的裝置清單 |

### `GET /v1/roles` 回應範例

```json
{
  "roles": [
    {
      "role": "primary_input",
      "device_id": "primary_keypad",
      "stable_path": "/dev/input/primary_keypad",
      "grab": false,
      "online": true
    },
    {
      "role": "secondary_input",
      "device_id": "secondary_keypad",
      "stable_path": "/dev/input/secondary_keypad",
      "grab": false,
      "online": true
    }
  ]
}
```

### `POST /v1/learn/{role}/start` 回應範例

```json
{
  "ok": true,
  "role": "primary_input",
  "timeout_sec": 15
}
```

### `POST /v1/roles/{role}/assign` 請求與回應

```json
// 請求
{ "device_id": "primary_keypad" }

// 回應
{
  "ok": true,
  "role": "primary_input",
  "device_id": "primary_keypad",
  "removed_from": ""
}
```

`removed_from` 非空時表示此裝置已從舊角色移轉。

### `GET /v1/autodiscover` 回應範例

```json
{
  "auto_discovered": [
    {
      "path": "/dev/input/event17",
      "name": "RustDesk UInput Keyboard",
      "role": "secondary_input",
      "online": true
    }
  ]
}
```

RustDesk 未連線時 `auto_discovered` 為空陣列。Web UI 每 3 秒輪詢此端點，並在 `secondary_input` 卡片下方顯示「Remote / Auto-discovered」區塊。

---

## 事件格式

Event socket 路徑：`/run/inputd/input.sock`

協定：Unix domain socket，每行一個 JSON 物件（JSON Lines）。

### 按鍵事件（`kind: "key"`）

```json
{
  "kind": "key",
  "ts": 1715040000123,
  "role": "primary_input",
  "device_id": "primary_keypad",
  "device_path": "/dev/input/primary_keypad",
  "device_name": "XFFP XFKEY",
  "event_type": "EV_KEY",
  "code": 96,
  "code_name": "KEY_KPENTER",
  "value": 1
}
```

### `value` 語意

| value | 意義 |
|---|---|
| `1` | 按下（key down） |
| `2` | 持續按住（auto-repeat） |
| 缺少此欄位 | 放開（key up，JSON omitempty 不輸出 0） |

### 狀態事件

```json
{ "kind": "device_connected",    "ts": 1715040000123, "role": "primary_input", "device_id": "primary_keypad", "device_path": "/dev/input/primary_keypad", "device_name": "XFFP XFKEY" }
{ "kind": "device_disconnected", "ts": 1715040005123, "role": "primary_input", "device_id": "primary_keypad", "device_path": "/dev/input/primary_keypad", "device_name": "" }
{ "kind": "role_changed",        "ts": 1715040010123, "role": "primary_input", "device_id": "primary_keypad" }
{ "kind": "health",              "ts": 1715040000000 }
```

`health` 事件每 30 秒發送一次，可用來偵測 daemon 是否存活。

---

## APP 整合

後端服務應把 `inputd` 視為硬體輸入邊界：`inputd` 在 NUC host 讀 `/dev/input`，後端只讀 `/run/inputd/input.sock` 的角色化事件。Docker 後端只需掛載 runtime socket 目錄：

```yaml
services:
  backend:
    volumes:
      - /run/inputd:/run/inputd
    environment:
      INPUTD_SOCKET: /run/inputd/input.sock
```

### Node.js 基本範例

```js
const net = require('net')
const readline = require('readline')

const PRIMARY_KEYS = new Set([50, 96, 55, 98])  // primary_input 的 4 個按鍵

function connect() {
  const sock = net.createConnection('/run/inputd/input.sock')
  const rl = readline.createInterface({ input: sock })

  sock.on('connect', () => console.log('inputd connected'))

  rl.on('line', line => {
    let ev
    try { ev = JSON.parse(line) } catch { return }

    switch (ev.kind) {
      case 'key':
        if (ev.value !== 1) return  // 只處理 key down，忽略 repeat 和 up

        if (ev.role === 'primary_input' && PRIMARY_KEYS.has(ev.code)) {
          handlePrimaryKey(ev.code)
        } else if (ev.role === 'secondary_input') {
          handleSecondaryKey(ev.code)
        }
        break

      case 'device_disconnected':
        console.warn(`裝置離線：${ev.role}`)
        // 顯示 UI 提示，要求現場人員重新接上鍵盤
        break

      case 'device_connected':
        console.log(`裝置恢復：${ev.role}`)
        break
    }
  })

  sock.on('close', () => {
    console.warn('inputd 連線中斷，3 秒後重連')
    setTimeout(connect, 3000)
  })

  sock.on('error', err => console.error('socket error:', err.message))
}

function handlePrimaryKey(code) {
  const actions = {
    50: 'DEAL',          // KEY_M
    96: 'CONFIRM',       // KEY_KPENTER
    55: 'CANCEL',        // KEY_KPASTERISK
    98: 'NEXT',          // KEY_KPSLASH
  }
  console.log('primary action:', actions[code])
}

function handleSecondaryKey(code) {
  console.log('secondary key:', code)
}

connect()
```

### Go 後端基本範例

```go
package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"os"
	"time"
)

type InputEvent struct {
	Kind       string `json:"kind"`
	TS         int64  `json:"ts"`
	Role       string `json:"role"`
	DeviceID   string `json:"device_id"`
	DevicePath string `json:"device_path"`
	DeviceName string `json:"device_name"`
	EventType  string `json:"event_type"`
	Code       uint16 `json:"code"`
	CodeName   string `json:"code_name"`
	Value      int32  `json:"value"`
}

var gameKeys = map[uint16]string{
	50: "DEAL",
	96: "CONFIRM",
	55: "CANCEL",
	98: "NEXT",
}

func main() {
	socketPath := os.Getenv("INPUTD_SOCKET")
	if socketPath == "" {
		socketPath = "/run/inputd/input.sock"
	}

	for {
		if err := consume(socketPath); err != nil {
			log.Printf("inputd disconnected: %v; reconnecting in 3s", err)
			time.Sleep(3 * time.Second)
		}
	}
}

func consume(socketPath string) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	log.Printf("inputd connected: %s", socketPath)
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		var ev InputEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			log.Printf("skip invalid inputd event: %v", err)
			continue
		}
		handleInputEvent(ev)
	}
	return scanner.Err()
}

func handleInputEvent(ev InputEvent) {
	switch ev.Kind {
	case "key":
		if ev.Value != 1 {
			return // 只處理 key down，忽略 key up 和 auto-repeat
		}
		switch ev.Role {
		case "primary_input":
			action, ok := gameKeys[ev.Code]
			if !ok {
				return
			}
			log.Printf("primary action=%s code=%d name=%s", action, ev.Code, ev.CodeName)
		case "secondary_input":
			log.Printf("secondary key code=%d name=%s", ev.Code, ev.CodeName)
		}
	case "device_connected":
		log.Printf("input device connected role=%s device=%s", ev.Role, ev.DeviceName)
	case "device_disconnected":
		log.Printf("input device disconnected role=%s path=%s", ev.Role, ev.DevicePath)
	case "health":
		log.Printf("inputd heartbeat ts=%d", ev.TS)
	}
}
```

---

## 鍵盤按鍵對照

### primary_input（XFFP XFKEY，4 個按鍵）

| 按鍵編號 | code | code_name | 建議對應動作 |
|---|---|---|---|
| 1 | 50 | KEY_M | DEAL |
| 2 | 96 | KEY_KPENTER | CONFIRM |
| 3 | 55 | KEY_KPASTERISK | CANCEL |
| 4 | 98 | KEY_KPSLASH | NEXT |

> code 28（KEY_ENTER）為鍵盤韌體副產物，APP 應忽略。

### secondary_input（SEM USB Keyboard）

標準鍵盤佈局，按鍵 code 為標準 Linux keycode，視業務需求對應。

---

## 故障排除

### daemon 未啟動

```bash
systemctl start inputd
journalctl -u inputd -n 20
```

### 鍵盤無事件輸出

1. 確認 symlink 存在：
   ```bash
   ls -la /dev/input/primary_keypad /dev/input/secondary_keypad
   ```
2. 若 symlink 消失（換接 USB port 後）：
   ```bash
   udevadm trigger --subsystem-match=input
   ```
3. 確認 daemon 偵測到裝置：
   ```bash
   curl -s http://127.0.0.1:17888/v1/roles | python3 -m json.tool
   ```

### Learn mode 無法綁定

常見原因：daemon 未在 15 秒內收到目標鍵盤的按鍵。

```bash
# 查看 learn mode 結果
journalctl -u inputd -n 10
```

若看到 `learn.timeout`，重新執行並在倒數結束前按鍵。

### 鍵盤換型號後無法識別（secondary）

採用「自動 Fallback 機制」後，只要更換的硬體具備鍵盤功能（`Handlers=kbd`），且名稱不包含主要操作員的 `XFFP`，就會自動被識別為主鍵盤。除非遇到特例（例如系統接了第三把鍵盤，或者新鍵盤名稱也包含了 XFFP），否則通常不需要再手動更新 secondary udev 規則。

如需手動除錯：

```bash
# 使用 udevadm info -a 查詢完整屬性（不建議使用 -q property，因為會遺漏硬體物理屬性）
udevadm info -a -n /dev/input/eventX | grep name

# 若需動態重新綁定 USB 鍵盤時，為了避免凍結 event loop，必須先刪除舊的規則與 symlink
rm -f /etc/udev/rules.d/99-secondary_keypad.rules  # 如果檔名有變更
rm -f /dev/input/secondary_keypad
udevadm control --reload-rules && udevadm trigger --subsystem-match=input
systemctl restart inputd
```

### 同時接了多顆非 XFFP 鍵盤

啟用 `auto_discover: true` 後，daemon 會自動把所有新出現的鍵盤（未被其他 role 佔用）都加入 `secondary_input`，包含遠端操作工具（如 RustDesk）建立的虛擬鍵盤。

udev symlink（`/dev/input/secondary_keypad`）依然只指向一顆鍵盤，其餘鍵盤透過動態發現機制直接以 `/dev/input/eventX` 路徑讀取。

```bash
# 查看目前所有鍵盤裝置與名稱
for f in /dev/input/event*; do
  name=$(cat /sys/class/input/$(basename $f)/device/name 2>/dev/null)
  [ -n "$name" ] && echo "$f  $name"
done

# 確認 daemon 實際綁定路徑（static + auto-discovered）
curl -s http://127.0.0.1:17888/v1/roles | python3 -m json.tool
```

### 即時監聽事件（測試用）

```bash
nc -U /run/inputd/input.sock
```

按鍵後應看到 JSON 輸出。

---

## 開發說明

### 建置

```bash
cd /root/inputd
go build -o /usr/local/bin/inputd ./cmd/inputd
systemctl restart inputd
```

### 啟動參數

```
inputd --config /etc/inputd/config.yaml --web-addr 127.0.0.1:17888 --log-level info
```

| 參數 | 預設值 | 說明 |
|---|---|---|
| `--config` | `/etc/inputd/config.yaml` | 設定檔路徑 |
| `--web-addr` | `127.0.0.1:17888` | Web UI 監聽位址，空字串停用 |
| `--log-level` | `info` | 日誌層級：`debug`、`info`、`warn`、`error` |
