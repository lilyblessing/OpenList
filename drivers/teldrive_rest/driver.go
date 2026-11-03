package teldrive_rest

import (
	"context"
	"fmt"
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
		SetRedirectPolicy(resty.NoRedirectPolicy()).
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

func (d *TeleDriveREST) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error { return errs.NotSupport }
func (d *TeleDriveREST) Move(ctx context.Context, srcObj, dstDir model.Obj) error               { return errs.NotSupport }
func (d *TeleDriveREST) Rename(ctx context.Context, srcObj model.Obj, newName string) error     { return errs.NotSupport }
func (d *TeleDriveREST) Copy(ctx context.Context, srcObj, dstDir model.Obj) error               { return errs.NotSupport }
func (d *TeleDriveREST) Remove(ctx context.Context, obj model.Obj) error                        { return errs.NotSupport }
func (d *TeleDriveREST) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	return errs.NotSupport
}

var _ driver.Driver = (*TeleDriveREST)(nil)
