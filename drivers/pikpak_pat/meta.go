package pikpak_pat

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootID
	AccessToken      string `json:"access_token" required:"true" help:"PikPak 个人访问令牌（PAT），JWT 格式。默认 90 天过期，到期需重新生成。创建时勾选 files:read / files:write / files:manage 可获得上传与删除权限。"`
	DisableMediaLink bool   `json:"disable_media_link" default:"true" help:"开启时不缓存媒体文件到 PikPak 中转，直接使用原始链接"`
}

var config = driver.Config{
	Name:      "PikPak_PAT",
	LocalSort: true,
	Alert:     "info|PAT 默认 90 天过期，到期需手动更换。本驱动支持上传和回收站删除；不支持永久删除，也不支持 OpenList 离线下载。",
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &PikPak{}
	})
}
