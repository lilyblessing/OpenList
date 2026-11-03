package teldrive_rest

import (
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// Folder 对应 GET /api/folders 的条目。
// ID 是 16 位小写十六进制字符串；根目录条目的 parent_id 为 null，故用 *string。
type Folder struct {
	ID        string     `json:"id"`
	ParentID  *string    `json:"parent_id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// File 对应 GET /api/files 的条目。
// 根目录文件的 folder_id 为 null，故用 *string；size 是 int64。
type File struct {
	ID                 string     `json:"id"`
	FolderID           *string    `json:"folder_id"`
	Name               string     `json:"name"`
	Size               int64      `json:"size"`
	MimeType           string     `json:"mime_type"`
	TelegramMessageID  int64      `json:"telegram_message_id"`
	TelegramFileID     string     `json:"telegram_file_id"`
	TelegramAccessHash string     `json:"telegram_access_hash"`
	SHA256             string     `json:"sha256"`
	IsEncrypted        int        `json:"is_encrypted"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	DeletedAt          *time.Time `json:"deleted_at,omitempty"`
}

func folderToObj(f Folder) *model.Object {
	return &model.Object{
		ID:       f.ID,
		Name:     f.Name,
		Size:     0,
		Ctime:    f.CreatedAt,
		Modified: f.UpdatedAt,
		IsFolder: true,
	}
}

func fileToObj(f File) *model.Object {
	return &model.Object{
		ID:       f.ID,
		Name:     f.Name,
		Size:     f.Size,
		Ctime:    f.CreatedAt,
		Modified: f.UpdatedAt,
		IsFolder: false,
	}
}

// UploadInitResp 是 POST /api/upload/init 的响应。
// 注意 ChunkSize 是 HTTP 分块大小（5 MB），TotalParts 是 MTProto 内部分包数（ceil(size/512KB)），
// 两者含义不同：HTTP 分块总数必须用 ceil(size / ChunkSize) 计算，不能用 TotalParts。
type UploadInitResp struct {
	ID         string `json:"id"`
	ChunkSize  int64  `json:"chunk_size"`
	TotalParts int    `json:"total_parts"`
}
