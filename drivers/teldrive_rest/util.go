package teldrive_rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const sessionCookieName = "teledrive_session"

// login 用密码 POST /login 换取会话 Cookie。
// 服务端成功返回 HTTP 303 + Set-Cookie，失败返回 HTTP 200 且没有 Set-Cookie，
// 因此只能靠响应头里有没有 teledrive_session 判断成功，绝不能用状态码。
func (d *TeleDriveREST) login(ctx context.Context) error {
	resp, err := d.client.R().
		SetContext(ctx).
		SetHeader("Content-Type", "application/x-www-form-urlencoded").
		SetFormData(map[string]string{"password": d.Addition.Password}).
		Post("/login")
	// 只要拿到了响应，就先尝试从 Set-Cookie 里取 token——
	// 某些 HTTP 客户端在「不跟随重定向」时会同时返回响应与一个非致命 error。
	if resp == nil {
		return fmt.Errorf("TeleDrive 登录请求失败（无响应）: %w", err)
	}
	token := ""
	for _, c := range resp.Header().Values("Set-Cookie") {
		if !strings.Contains(c, sessionCookieName+"=") {
			continue
		}
		pair := strings.SplitN(c, ";", 2)[0]
		token = strings.TrimSpace(strings.TrimPrefix(pair, sessionCookieName+"="))
		break
	}
	if token == "" {
		if err != nil {
			return fmt.Errorf("TeleDrive 登录失败：响应中没有 %s Cookie（HTTP %d）: %w，密码错误或 teledrive 版本不兼容", sessionCookieName, resp.StatusCode(), err)
		}
		return fmt.Errorf("TeleDrive 登录失败：响应中没有 %s Cookie（HTTP %d），密码错误或 teledrive 版本不兼容", sessionCookieName, resp.StatusCode())
	}
	d.mu.Lock()
	d.session = token
	d.mu.Unlock()
	return nil
}

// ensureCookie 保证当前有会话 token；没有则登录一次。
func (d *TeleDriveREST) ensureCookie(ctx context.Context) error {
	d.mu.RLock()
	token := d.session
	d.mu.RUnlock()
	if token != "" {
		return nil
	}
	return d.login(ctx)
}

// listFolders 列出 folderID 下的子目录。folderID 为空串表示根目录（服务端实测空串等价于根）。
// 遇到 401 清空会话重登一次后重试，只重试一次。
func (d *TeleDriveREST) listFolders(ctx context.Context, folderID string) ([]Folder, error) {
	retried := false
	for {
		if err := d.ensureCookie(ctx); err != nil {
			return nil, err
		}
		d.mu.RLock()
		token := d.session
		d.mu.RUnlock()
		resp, err := d.client.R().
			SetContext(ctx).
			SetHeader("Cookie", sessionCookieName+"="+token).
			SetQueryParam("folder_id", folderID).
			Get("/api/folders")
		if err != nil {
			return nil, err
		}
		if resp.StatusCode() == http.StatusUnauthorized {
			if retried {
				return nil, fmt.Errorf("TeleDrive /api/folders 鉴权失败（401），重新登录后仍然无效")
			}
			retried = true
			d.mu.Lock()
			d.session = ""
			d.mu.Unlock()
			continue
		}
		if resp.StatusCode() != http.StatusOK {
			return nil, fmt.Errorf("TeleDrive GET /api/folders 返回 HTTP %d: %s", resp.StatusCode(), resp.String())
		}
		// 空目录时服务端返回字面量 null（不是 []），json.Unmarshal 对 null 不报错且不改动切片，
		// 因此这里天然按空列表处理。
		folders := make([]Folder, 0)
		if err := json.Unmarshal(resp.Body(), &folders); err != nil {
			return nil, fmt.Errorf("TeleDrive /api/folders 响应解析失败: %w", err)
		}
		return folders, nil
	}
}

// listFiles 列出 folderID 下的文件，重试策略同 listFolders。
func (d *TeleDriveREST) listFiles(ctx context.Context, folderID string) ([]File, error) {
	retried := false
	for {
		if err := d.ensureCookie(ctx); err != nil {
			return nil, err
		}
		d.mu.RLock()
		token := d.session
		d.mu.RUnlock()
		resp, err := d.client.R().
			SetContext(ctx).
			SetHeader("Cookie", sessionCookieName+"="+token).
			SetQueryParam("folder_id", folderID).
			Get("/api/files")
		if err != nil {
			return nil, err
		}
		if resp.StatusCode() == http.StatusUnauthorized {
			if retried {
				return nil, fmt.Errorf("TeleDrive /api/files 鉴权失败（401），重新登录后仍然无效")
			}
			retried = true
			d.mu.Lock()
			d.session = ""
			d.mu.Unlock()
			continue
		}
		if resp.StatusCode() != http.StatusOK {
			return nil, fmt.Errorf("TeleDrive GET /api/files 返回 HTTP %d: %s", resp.StatusCode(), resp.String())
		}
	// 空目录时服务端返回字面量 null（不是 []），按空列表处理。
	files := make([]File, 0)
	if err := json.Unmarshal(resp.Body(), &files); err != nil {
		return nil, fmt.Errorf("TeleDrive /api/files 响应解析失败: %w", err)
	}
		return files, nil
	}
}

// createFolder 建目录。parentID 为空串表示根。
// 重试策略同 listFolders：401 清会话重登一次后重试，只重试一次。
func (d *TeleDriveREST) createFolder(ctx context.Context, name, parentID string) error {
	// folders.parent_id 有外键约束指向 folders(id)，空串不是合法外键值（实测 500），
	// 根目录必须发 JSON null。map[string]string 表达不了 null，故用 any + nil。
	var parent any
	if parentID != "" {
		parent = parentID
	}
	retried := false
	for {
		if err := d.ensureCookie(ctx); err != nil {
			return err
		}
		d.mu.RLock()
		token := d.session
		d.mu.RUnlock()
		resp, err := d.client.R().
			SetContext(ctx).
			SetHeader("Cookie", sessionCookieName+"="+token).
			SetHeader("Content-Type", "application/json").
			SetBody(map[string]interface{}{"name": name, "parent_id": parent}).
			Post("/api/folders")
		if err != nil {
			return err
		}
		if resp.StatusCode() == http.StatusUnauthorized {
			if retried {
				return fmt.Errorf("TeleDrive /api/folders 鉴权失败（401），重新登录后仍然无效")
			}
			retried = true
			d.mu.Lock()
			d.session = ""
			d.mu.Unlock()
			continue
		}
		if resp.StatusCode() < 200 || resp.StatusCode() > 299 {
			return fmt.Errorf("TeleDrive POST /api/folders 返回 HTTP %d: %s", resp.StatusCode(), resp.String())
		}
		return nil
	}
}

// uploadInit 初始化上传会话。folderID 为空串表示根。
// 注意响应里 ChunkSize 是 HTTP 分块大小（5 MB），TotalParts 是 MTProto 内部分包数，
// HTTP 分块总数必须用 ceil(size / ChunkSize) 计算，不能用 TotalParts。
func (d *TeleDriveREST) uploadInit(ctx context.Context, name string, size int64, mimeType, folderID string) (*UploadInitResp, error) {
	// upload_sessions.folder_id 有外键约束指向 folders(id)，空串不是合法外键值（实测 500），
	// 根目录必须发 JSON null，故用 any + nil。
	var folder any
	if folderID != "" {
		folder = folderID
	}
	retried := false
	for {
		if err := d.ensureCookie(ctx); err != nil {
			return nil, err
		}
		d.mu.RLock()
		token := d.session
		d.mu.RUnlock()
		resp, err := d.client.R().
			SetContext(ctx).
			SetHeader("Cookie", sessionCookieName+"="+token).
			SetHeader("Content-Type", "application/json").
			SetBody(map[string]interface{}{"name": name, "size": size, "mime_type": mimeType, "folder_id": folder}).
			Post("/api/upload/init")
		if err != nil {
			return nil, err
		}
		if resp.StatusCode() == http.StatusUnauthorized {
			if retried {
				return nil, fmt.Errorf("TeleDrive /api/upload/init 鉴权失败（401），重新登录后仍然无效")
			}
			retried = true
			d.mu.Lock()
			d.session = ""
			d.mu.Unlock()
			continue
		}
		if resp.StatusCode() < 200 || resp.StatusCode() > 299 {
			return nil, fmt.Errorf("TeleDrive POST /api/upload/init 返回 HTTP %d: %s", resp.StatusCode(), resp.String())
		}
		var initResp UploadInitResp
		if err := json.Unmarshal(resp.Body(), &initResp); err != nil {
			return nil, fmt.Errorf("TeleDrive /api/upload/init 响应解析失败: %w", err)
		}
		return &initResp, nil
	}
}

// uploadChunk 上传一个 HTTP 分块（chunkIndex 从 0 开始）。
// 分块是 multipart/form-data，不是 JSON：session_id / chunk_index 为表单字段，
// chunk 为文件字段（文件名随意，这里用 blob）。
func (d *TeleDriveREST) uploadChunk(ctx context.Context, sessionID string, chunkIndex int, data []byte) error {
	retried := false
	for {
		if err := d.ensureCookie(ctx); err != nil {
			return err
		}
		d.mu.RLock()
		token := d.session
		d.mu.RUnlock()
		resp, err := d.client.R().
			SetContext(ctx).
			SetHeader("Cookie", sessionCookieName+"="+token).
			SetMultipartFormData(map[string]string{
				"session_id":  sessionID,
				"chunk_index": strconv.Itoa(chunkIndex),
			}).
			SetMultipartField("chunk", "blob", "application/octet-stream", bytes.NewReader(data)).
			Post("/api/upload/chunk")
		if err != nil {
			return err
		}
		if resp.StatusCode() == http.StatusUnauthorized {
			if retried {
				return fmt.Errorf("TeleDrive /api/upload/chunk 鉴权失败（401），重新登录后仍然无效")
			}
			retried = true
			d.mu.Lock()
			d.session = ""
			d.mu.Unlock()
			continue
		}
		if resp.StatusCode() < 200 || resp.StatusCode() > 299 {
			return fmt.Errorf("TeleDrive POST /api/upload/chunk 返回 HTTP %d: %s", resp.StatusCode(), resp.String())
		}
		return nil
	}
}

// uploadComplete 完成上传，返回服务端生成的 File。
func (d *TeleDriveREST) uploadComplete(ctx context.Context, sessionID string) (*File, error) {
	retried := false
	for {
		if err := d.ensureCookie(ctx); err != nil {
			return nil, err
		}
		d.mu.RLock()
		token := d.session
		d.mu.RUnlock()
		resp, err := d.client.R().
			SetContext(ctx).
			SetHeader("Cookie", sessionCookieName+"="+token).
			SetHeader("Content-Type", "application/json").
			SetBody(map[string]string{"session_id": sessionID}).
			Post("/api/upload/complete")
		if err != nil {
			return nil, err
		}
		if resp.StatusCode() == http.StatusUnauthorized {
			if retried {
				return nil, fmt.Errorf("TeleDrive /api/upload/complete 鉴权失败（401），重新登录后仍然无效")
			}
			retried = true
			d.mu.Lock()
			d.session = ""
			d.mu.Unlock()
			continue
		}
		if resp.StatusCode() < 200 || resp.StatusCode() > 299 {
			return nil, fmt.Errorf("TeleDrive POST /api/upload/complete 返回 HTTP %d: %s", resp.StatusCode(), resp.String())
		}
		var file File
		if err := json.Unmarshal(resp.Body(), &file); err != nil {
			return nil, fmt.Errorf("TeleDrive /api/upload/complete 响应解析失败: %w", err)
		}
		return &file, nil
	}
}

// deleteFile 删除文件（服务端是软删，进回收站）。
func (d *TeleDriveREST) deleteFile(ctx context.Context, id string) error {
	retried := false
	for {
		if err := d.ensureCookie(ctx); err != nil {
			return err
		}
		d.mu.RLock()
		token := d.session
		d.mu.RUnlock()
		resp, err := d.client.R().
			SetContext(ctx).
			SetHeader("Cookie", sessionCookieName+"="+token).
			Delete("/api/files/" + id)
		if err != nil {
			return err
		}
		if resp.StatusCode() == http.StatusUnauthorized {
			if retried {
				return fmt.Errorf("TeleDrive /api/files 鉴权失败（401），重新登录后仍然无效")
			}
			retried = true
			d.mu.Lock()
			d.session = ""
			d.mu.Unlock()
			continue
		}
		if resp.StatusCode() < 200 || resp.StatusCode() > 299 {
			return fmt.Errorf("TeleDrive DELETE /api/files/%s 返回 HTTP %d: %s", id, resp.StatusCode(), resp.String())
		}
		return nil
	}
}

// listFilesByName 在 folderID 下按名字找文件，用于 overwrite 查重。folderID 为空串表示根。
// 复用 listFiles 拿全量后在 Go 侧过滤；不用服务端的 ?search=，那是全局跨目录搜索，语义不同。
func (d *TeleDriveREST) listFilesByName(ctx context.Context, folderID, name string) ([]File, error) {
	files, err := d.listFiles(ctx, folderID)
	if err != nil {
		return nil, err
	}
	matched := make([]File, 0)
	for _, f := range files {
		if f.Name == name {
			matched = append(matched, f)
		}
	}
	return matched, nil
}

// chunkCount 返回 HTTP 分块总数。
// 必须用 chunkSize（5 MB）计算，不能用服务端返回的 total_parts（那是 MTProto 分包数，ceil(size/512KB)）。
func chunkCount(size, chunkSize int64) int64 {
	if chunkSize <= 0 || size <= 0 {
		return 0
	}
	return (size + chunkSize - 1) / chunkSize
}
