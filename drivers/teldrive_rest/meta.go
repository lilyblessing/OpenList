package teldrive_rest

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootID
	Address  string `json:"address" required:"true" default:"http://127.0.0.1:8080" help:"TeleDrive 服务地址，如 http://127.0.0.1:8080"`
	Password string `json:"password" required:"true" help:"TeleDrive 管理密码（对应 TELEDRIVE_ADMIN_PASSWORD）"`
	Overwrite bool `json:"overwrite" default:"false" help:"开启后上传同名文件会先删除旧文件再上传（服务端没有唯一约束，默认会产生重名副本）"`
}

var config = driver.Config{
	Name:        "TeleDrive REST",
	DefaultRoot: "",
	LocalSort:   true,
	CheckStatus: true,
	// teledrive 用 Cookie 鉴权，直链（302）无法把 Cookie 交给客户端，必然 401；
	// 必须强制走 OpenList 代理，由服务端带上 Cookie 转发。实测 /p/ 代理下 Range 正常（206）。
	OnlyProxy: true,
	Alert:     "info|TeleDrive 通过私有 REST API 接入。文件名唯一性由使用方自行保证（服务端无唯一约束，同目录可重名）。单文件上限受 Telegram 限制：2 GB（免费）/ 4 GB（Premium）。",
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &TeleDriveREST{}
	})
}
