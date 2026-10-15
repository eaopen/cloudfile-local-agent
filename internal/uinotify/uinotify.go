// Package uinotify carries one-way notifications from the Agent to the resident
// task-centre UI ("已本地打开 xxx.docx" 这类提示).
//
// 为什么用一个追加式文件，而不是命名管道 / 本地端口：
// Agent 是 Chrome 每条 native message 起一个**新进程**的 native host，而 UI 常驻
// 在托盘里。两者没有一个能长期复用的连接，也没有现成的权限协商通道。最省事又最稳
// 的公共点是一个小文件：Agent 追加一行 JSON，UI 每 3 秒（本来就在轮询任务列表）
// 读一次增量。没有端口、没有句柄泄漏、双方重启都不丢，权限也天然是每用户的。
//
// 刻意保留的取舍：
//   - UI 启动时**跳过历史**（只从当前文件末尾往后读），否则攒了一晚上的记录会在
//     开机时一次炸出一串气泡；
//   - 文件超过上限就地压缩（只留最新一半），所以它是通知队列，不是审计日志。
package uinotify

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
)

// Kind 是通知类型。
type Kind string

const (
	// KindOpened 表示本地查看/编辑已经把文件打开给用户了。
	KindOpened Kind = "opened"
)

// Event 是一条通知。字段刻意保持字符串、平铺，方便 UI 直接展示。
type Event struct {
	At   int64  `json:"at"`             // Unix 毫秒
	Kind Kind   `json:"kind"`           //
	Mode string `json:"mode,omitempty"` // local-view / local-edit
	File string `json:"file,omitempty"` // 文件名（镜像已拍平，只有文件名）
	Dir  string `json:"dir,omitempty"`  // 本地目录：用户最需要知道的就是文件落在哪
	Note string `json:"note,omitempty"` // 附加说明
}

const (
	fileName    = "ui-notify.jsonl"
	markerName  = "ui-notify-shown"
	maxBytes    = 64 << 10
	compactKeep = maxBytes / 2
)

// Path 返回通知队列的路径（与 config.json 同目录）。
func Path() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fileName), nil
}

// Post 追加一条通知。它是**尽力而为**的：调用方不该因为提示发不出去而让会话失败。
func Post(event Event) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if event.At == 0 {
		event.At = time.Now().UnixMilli()
	}
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	// 超过上限就压缩，别让它无限长。压缩失败不影响投递。
	if info, statErr := os.Stat(path); statErr == nil && info.Size() > maxBytes {
		_ = compact(path)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	// 一次 Write 写完一行：多条短写的原子性没有保证，一行一次是这里能接受的最小粒度。
	_, err = file.Write(line)
	return err
}

// Size 是队列当前的字节长度；UI 启动时用它跳过历史。
func Size() (int64, error) {
	path, err := Path()
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return info.Size(), nil
}

// Read 返回偏移 from 之后新增的通知，以及下一次该从哪个偏移继续读。
// 只处理完整的行：写入方是整行追加的，读到半行时不动偏移，等下一次。
func Read(from int64) ([]Event, int64, error) {
	path, err := Path()
	if err != nil {
		return nil, from, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, from, err
	}
	if from < 0 || from > int64(len(data)) {
		// 文件被压缩/轮转过了，从头读一遍。
		from = 0
	}
	chunk := data[from:]
	end := bytes.LastIndexByte(chunk, '\n')
	if end < 0 {
		return nil, from, nil
	}
	consumed := int64(end) + 1

	events := make([]Event, 0, 4)
	for _, line := range bytes.Split(chunk[:consumed], []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event Event
		// 坏行跳过：一行坏数据不该让整批提示消失。
		if json.Unmarshal(line, &event) != nil || event.Kind == "" {
			continue
		}
		events = append(events, event)
	}
	return events, from + consumed, nil
}

// ClaimStartupNotice 只在**第一次**返回 true：UI 用它决定要不要弹那条
// 「已在托盘运行」的说明气泡。标记文件删掉就会再弹一次。
func ClaimStartupNotice() (bool, error) {
	path, err := Path()
	if err != nil {
		return false, err
	}
	marker := filepath.Join(filepath.Dir(path), markerName)
	if _, err := os.Stat(marker); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		return false, err
	}
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		// 并发启动时抢输了：那就不是"第一次"。
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	file.Close()
	return true, nil
}

// compact 只保留文件的后半段，并从行首开始，避免留下半行。
func compact(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) <= compactKeep {
		return nil
	}
	cut := len(data) - compactKeep
	if index := bytes.IndexByte(data[cut:], '\n'); index >= 0 {
		cut += index + 1
	} else {
		return errors.New("uinotify: no line boundary to compact at")
	}
	temporary := path + ".compact"
	if err := os.WriteFile(temporary, data[cut:], 0600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
