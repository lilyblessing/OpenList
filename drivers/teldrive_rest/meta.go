package teldrive_rest

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootID
	Address  string `json:"address" required:"true" default:"http://127.0.0.1:8080" help:"TeleDrive 服务地址，如 http://127.0.0.1:8080"`
	Password string `json:"password" required:"true" help:"TeleDrive 管理密码（对应 TELEDRIVE_ADMIN_PASSWORD）"`
}

var config = driver.Config{
	Name:        "TeleDrive REST",
	DefaultRoot: "",
	LocalSort:   true,
	CheckStatus: true,
	Alert:       "info|TeleDrive 通过私有 REST API 接入。文件名唯一性由使用方自行保证（服务端无唯一约束，同目录可重名）。单文件上限受 Telegram 限制：2 GB（免费）/ 4 GB（Premium）。",
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &TeleDriveREST{}
	})
}
