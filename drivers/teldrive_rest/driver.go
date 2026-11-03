package teldrive_rest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/go-resty/resty/v2"
)

// TeleDriveREST 通过 TeleDrive 的私有 REST API（Cookie 会话鉴权）接入。
type TeleDriveREST struct {
	model.Storage
	Addition
	client  *resty.Client
	mu      sync.RWMutex
	session string
}

func (d *TeleDriveREST) Config() driver.Config {
	return config
}

func (d *TeleDriveREST) GetStorage() *model.Storage {
	return &d.Storage
}

func (d *TeleDriveREST) SetStorage(s model.Storage) {
	d.Storage = s
}

func (d *TeleDriveREST) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *TeleDriveREST) Init(ctx context.Context) error {
	if d.Addition.Address == "" {
		return fmt.Errorf("TeleDrive 服务地址不能为空")
	}
	if d.Addition.Password == "" {
		return fmt.Errorf("TeleDrive 管理密码不能为空")
	}
	d.client = resty.New().
		SetRedirectPolicy(resty.RedirectPolicyFunc(func(req *http.Request, via []*http.Request) error {
			// 不跟随重定向，但把 303 响应原样交回，且不产生 error。
			// teledrive 登录成功时返回 303 + Set-Cookie，必须读到这个响应头。
			return http.ErrUseLastResponse
		})).
		SetBaseURL(strings.TrimRight(d.Addition.Address, "/"))
	// 登录换取会话 Cookie
	if err := d.ensureCookie(ctx); err != nil {
		return err
	}
	// 用列根目录探测服务与鉴权真实可用，失败直接报错，不 panic
	if _, err := d.listFolders(ctx, ""); err != nil {
		return err
	}
	return nil
}

func (d *TeleDriveREST) Drop(ctx context.Context) error {
	d.mu.Lock()
	d.session = ""
	d.mu.Unlock()
	return nil
}

func (d *TeleDriveREST) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	// dir.GetID() 为空串即根目录，服务端把空 folder_id 当作根
	fid := dir.GetID()
	folders, err := d.listFolders(ctx, fid)
	if err != nil {
		return nil, err
	}
	files, err := d.listFiles(ctx, fid)
	if err != nil {
		return nil, err
	}
	objs := make([]model.Obj, 0, len(folders)+len(files))
	// 目录在前，文件在后
	for _, f := range folders {
		objs = append(objs, folderToObj(f))
	}
	for _, f := range files {
		objs = append(objs, fileToObj(f))
	}
	return objs, nil
}

func (d *TeleDriveREST) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	d.mu.RLock()
	token := d.session
	d.mu.RUnlock()
	if token == "" {
		return nil, fmt.Errorf("TeleDrive 会话为空，请重新挂载以登录")
	}
	return &model.Link{
		URL: strings.TrimRight(d.Addition.Address, "/") + "/api/files/" + file.GetID() + "/stream",
		Header: http.Header{
			"Cookie": {sessionCookieName + "=" + token},
		},
	}, nil
}

func (d *TeleDriveREST) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	// parentDir.GetID() 为空串时即根目录，服务端把空 parent_id 当根
	return d.createFolder(ctx, dirName, parentDir.GetID())
}
func (d *TeleDriveREST) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	dst := dstDir.GetID()
	if dst == "" {
		// 目标为空 ID 即根目录。
		// 注意：PUT /api/files/{id} 的 folder_id 传 null 会被服务端当作「不修改」，
		// 所以移到根必须走 POST /api/batch/move（其 target_folder_id 为 null 时服务端当作根）。
		if srcObj.IsDir() {
			return d.batchMove(ctx, nil, []string{srcObj.GetID()}, "")
		}
		return d.batchMove(ctx, []string{srcObj.GetID()}, nil, "")
	}
	if srcObj.IsDir() {
		return d.moveFolder(ctx, srcObj.GetID(), dst)
	}
	return d.moveFile(ctx, srcObj.GetID(), dst)
}

func (d *TeleDriveREST) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	if srcObj.IsDir() {
		return d.renameFolder(ctx, srcObj.GetID(), newName)
	}
	return d.renameFile(ctx, srcObj.GetID(), newName)
}

func (d *TeleDriveREST) Copy(ctx context.Context, srcObj, dstDir model.Obj) error { return errs.NotSupport }

// Remove 是软删（进 teledrive 虚拟回收站），数据可恢复。
func (d *TeleDriveREST) Remove(ctx context.Context, obj model.Obj) error {
	if obj.IsDir() {
		return d.deleteFolder(ctx, obj.GetID())
	}
	return d.deleteFile(ctx, obj.GetID())
}

func (d *TeleDriveREST) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	name := stream.GetName()
	size := stream.GetSize()
	mime := stream.GetMimetype()
	fid := dstDir.GetID() // "" = 根

	// 服务端要求 size > 0，否则 400 invalid upload init payload
	if size <= 0 {
		return fmt.Errorf("TeleDrive 上传要求文件大小大于 0（实际 %d）", size)
	}

	// 覆盖模式：先软删同名旧文件（进回收站）
	if d.Addition.Overwrite {
		old, err := d.listFilesByName(ctx, fid, name)
		if err != nil {
			return err
		}
		for _, f := range old {
			if err := d.deleteFile(ctx, f.ID); err != nil {
				return err
			}
		}
	}

	init, err := d.uploadInit(ctx, name, size, mime, fid)
	if err != nil {
		return err
	}

	chunkSize := init.ChunkSize
	// ★ HTTP 分块数 = ceil(size / chunk_size)。
	// 不要用 init.TotalParts：那是 MTProto 内部分包数（ceil(size/512KB)），
	// 比HTTP 分块数大得多，误用会导致服务端静默丢弃多余分块。
	if chunkSize <= 0 {
		return fmt.Errorf("TeleDrive 返回非法 chunk_size: %d", chunkSize)
	}
	totalChunks := chunkCount(size, chunkSize)

	for i := 0; i < int(totalChunks); i++ {
		// 本块应有的大小（最后一块可能不满）
		want := chunkSize
		if remain := size - int64(i)*chunkSize; remain < want {
			want = remain
		}
		buf := make([]byte, want)
		if _, err := io.ReadFull(stream, buf); err != nil {
			return fmt.Errorf("读取第 %d 块失败（共 %d 块）: %w", i, totalChunks, err)
		}
		// 服务端对同一 chunk_index 重复上传是幂等的（按索引覆盖），可安全重试
		var uploadErr error
		for attempt := 0; attempt < 3; attempt++ {
			uploadErr = d.uploadChunk(ctx, init.ID, i, buf)
			if uploadErr == nil {
				break
			}
		}
		if uploadErr != nil {
			return fmt.Errorf("上传第 %d 块失败（共 %d 块）: %w", i, totalChunks, uploadErr)
		}
		if up != nil {
			up(float64(i+1) / float64(totalChunks) * 100)
		}
	}

	_, err = d.uploadComplete(ctx, init.ID)
	return err
}

var _ driver.Driver = (*TeleDriveREST)(nil)
