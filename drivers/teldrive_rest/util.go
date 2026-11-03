package teldrive_rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	if err != nil {
		return fmt.Errorf("TeleDrive 登录请求失败: %w", err)
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
