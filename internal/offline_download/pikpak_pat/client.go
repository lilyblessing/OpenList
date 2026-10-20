package pikpak_pat

import (
	"context"
	"fmt"
	"strconv"

	pikpakpat "github.com/OpenListTeam/OpenList/v4/drivers/pikpak_pat"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/offline_download/tool"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
)

// offlineListLimit 与驱动侧 OfflineList 的 limit 保持一致（原驱动为 10000，
// 见 drivers/pikpak/driver.go）。返回条数达到该值时说明可能还有更多页。
const offlineListLimit = 10000

type PikPakPAT struct {
}

func (p *PikPakPAT) Name() string {
	return "PikPakPAT"
}

func (p *PikPakPAT) Items() []model.SettingItem {
	return nil
}

func (p *PikPakPAT) Run(task *tool.DownloadTask) error {
	return errs.NotSupport
}

func (p *PikPakPAT) Init() (string, error) {
	return "ok", nil
}

func (p *PikPakPAT) IsReady() bool {
	tempDir := setting.GetStr(conf.PikPakPATTempDir)
	if tempDir == "" {
		return false
	}
	storage, _, err := op.GetStorageAndActualPath(tempDir)
	if err != nil {
		return false
	}
	if _, ok := storage.(*pikpakpat.PikPak); !ok {
		return false
	}
	return true
}

func (p *PikPakPAT) AddURL(args *tool.AddUrlArgs) (string, error) {
	storage, actualPath, err := op.GetStorageAndActualPath(args.TempDir)
	if err != nil {
		return "", err
	}
	d, ok := storage.(*pikpakpat.PikPak)
	if !ok {
		return "", fmt.Errorf("unsupported storage driver for offline download, only PikPak_PAT is supported")
	}

	if err := op.MakeDir(args.Ctx, storage, actualPath); err != nil {
		return "", err
	}

	parentDir, err := op.GetUnwrap(args.Ctx, storage, actualPath)
	if err != nil {
		return "", err
	}

	// fileName 必须传空字符串：磁力/ed2k 链接让 PikPak 自己取名，
	// 用 URL 最后一段会带 query 得到脏名字。
	t, err := d.OfflineDownload(args.Ctx, args.Url, parentDir, "")
	if err != nil {
		return "", fmt.Errorf("failed to add offline download task: %w", err)
	}

	return t.ID, nil
}

func (p *PikPakPAT) Remove(task *tool.DownloadTask) error {
	storage, _, err := op.GetStorageAndActualPath(task.TempDir)
	if err != nil {
		return err
	}
	d, ok := storage.(*pikpakpat.PikPak)
	if !ok {
		return fmt.Errorf("unsupported storage driver for offline download, only PikPak_PAT is supported")
	}

	// deleteFiles=false：用户取消只表示不要这个离线任务。
	// PikPak 可能已把文件放进网盘，true 会把已保存的文件一并删掉，且不是回收站语义。
	err = d.DeleteOfflineTasks(context.Background(), []string{task.GID}, false)
	if err != nil {
		return err
	}
	return nil
}

func (p *PikPakPAT) Status(task *tool.DownloadTask) (*tool.Status, error) {
	storage, _, err := op.GetStorageAndActualPath(task.TempDir)
	if err != nil {
		return nil, err
	}
	d, ok := storage.(*pikpakpat.PikPak)
	if !ok {
		return nil, fmt.Errorf("unsupported storage driver for offline download, only PikPak_PAT is supported")
	}

	// 翻页策略：驱动 OfflineList(ctx, nextPageToken, phase) 内部消费掉
	// next_page_token，只返回一页 tasks，调用方拿不到下一页 token，无法真正翻页。
	// 但其 limit 为 10000（照抄原驱动），一页足以容纳几百条任务，
	// 因此调用时传 "" 与 nil（默认全 phase），一页拿全量列表。
	// 兜底：若返回条数 == limit，说明可能还有更多页，此时不能断言任务已删除，
	// 只能如实报告"未能在第一页找到"。
	tasks, err := d.OfflineList(context.Background(), "", nil)
	if err != nil {
		return nil, err
	}

	s := &tool.Status{
		Progress:  0,
		NewGID:    "",
		Completed: false,
		Status:    "the task has been deleted",
		Err:       nil,
	}

	for _, t := range tasks {
		if t.ID != task.GID {
			continue
		}
		switch t.Phase {
		case "PHASE_TYPE_PENDING":
			s.Status = "等待中"
			s.Completed = false
		case "PHASE_TYPE_RUNNING":
			s.Status = t.Message
			s.Completed = false
			s.Progress = float64(t.Progress)
		case "PHASE_TYPE_COMPLETE":
			s.Status = "已完成"
			s.Completed = true
			s.Progress = 100
			s.TotalBytes, err = strconv.ParseInt(t.FileSize, 10, 64)
			if err != nil {
				s.TotalBytes = 0
			}
		case "PHASE_TYPE_ERROR":
			s.Status = t.Message
			s.Completed = false
			s.Err = fmt.Errorf("%s", t.Message)
		default:
			s.Status = t.Phase
			s.Completed = false
		}
		return s, nil
	}

	if len(tasks) == offlineListLimit {
		// 列表已达单页上限，目标可能位于后续页（驱动签名拿不到下一页 token），
		// 不能误判为"已删除"。
		s.Status = "task not found in the first offline list page (page limit reached)"
		s.Err = nil
		return s, nil
	}

	// 翻完（一页即全量）仍找不到：任务已从 PikPak 列表消失。
	s.Err = fmt.Errorf("任务已从 PikPak 列表消失")
	return s, nil
}

var _ tool.Tool = (*PikPakPAT)(nil)

func init() {
	tool.Tools.Add(&PikPakPAT{})
}
