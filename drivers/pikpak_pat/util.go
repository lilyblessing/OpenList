package pikpak_pat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	netutil "github.com/OpenListTeam/OpenList/v4/internal/net"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/go-resty/resty/v2"
	"github.com/pkg/errors"
)

const (
	OSSUserAgent               = "aliyun-sdk-android/2.9.13(Linux/Android 14/M2004j7ac;UKQ1.231108.001)"
	OssSecurityTokenHeaderName = "X-OSS-Security-Token"
	ThreadsNum                 = 10
)

func (d *PikPak) request(url string, method string, callback base.ReqCallback, resp interface{}) ([]byte, error) {
	req := base.RestyClient.R()
	req.SetHeaders(map[string]string{
		"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	})
	if d.AccessToken != "" {
		req.SetHeader("Authorization", "Bearer "+d.AccessToken)
	}

	if callback != nil {
		callback(req)
	}
	if resp != nil {
		req.SetResult(resp)
	}
	var e ErrResp
	req.SetError(&e)

	var res *resty.Response
	var err error
	// error_code 10（操作频繁）最多重试 3 次，间隔 1s / 2s / 4s + 随机抖动
	delays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	for attempt := 0; ; attempt++ {
		// 关键：每次请求前清零 e 并重新 SetError。resty 在 2xx 成功响应时会把
		// res.Request.Error 置 nil 且不再反序列化错误体，局部变量 e 不会被自动清空；
		// 若不重置，上一轮遗留的 ErrorCode（如 10）会在重试成功后误判为失败。
		e = ErrResp{}
		req.SetError(&e)
		res, err = req.Execute(method, url)
		if err != nil {
			return nil, err
		}
		// 先看 HTTP 状态：>= 400 且 error_code 还是 0，说明响应体不是这套 PikPak JSON 错误结构，
		// 不能把错误响应当成功返回（否则 PAT 失效时 Init 可能误判通过）
		if res.StatusCode() >= 400 && e.ErrorCode == 0 {
			return nil, errors.Errorf("PikPak API 返回 HTTP %d: %s", res.StatusCode(), res.String())
		}
		if e.ErrorCode == 10 && attempt < len(delays) {
			time.Sleep(delays[attempt] + time.Duration(rand.Int63n(300))*time.Millisecond)
			continue
		}
		break
	}

	switch e.ErrorCode {
	case 0:
		return res.Body(), nil
	case 4122, 4121, 16:
		return nil, errors.New("PikPak PAT 已过期或被撤销，请重新生成个人访问令牌并更新配置")
	case 9:
		return nil, errors.New("PikPak 要求人机验证，请稍后重试或重新生成 PAT")
	case 10:
		return nil, errors.New("PikPak 操作过于频繁，请稍后重试")
	case 1, 2, 3, 4, 5, 6, 7, 8:
		// 业务错误：权限类要翻译成人话
		return nil, translateAPIError(e)
	default:
		return nil, errors.New(e.Error())
	}
}

// tokenExpiry 从 JWT 中解析过期时间。不是 JWT / 解析失败时返回 ok=false。
// 不引入 JWT 库，标准库 encoding/base64 足够。
// 注意：禁止把 token / sub / sid 打到日志。
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, false
	}
	if claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// translateAPIError 把权限类业务错误翻译成用户可读的中文提示
func translateAPIError(e ErrResp) error {
	text := strings.ToLower(e.ErrorDescription + " " + e.ErrorMsg)
	switch {
	case strings.Contains(text, "write") || strings.Contains(text, "upload") || strings.Contains(text, "create"):
		return errors.New("当前 PAT 缺少 files:write 权限（" + e.ErrorDescription + "）")
	case strings.Contains(text, "delete") || strings.Contains(text, "trash") || strings.Contains(text, "manage"):
		return errors.New("当前 PAT 缺少 files:manage 权限（" + e.ErrorDescription + "）")
	default:
		return errors.New(e.Error())
	}
}

func (d *PikPak) getFiles(id string) ([]File, error) {
	res := make([]File, 0)
	pageToken := "first"
	for page := 1; pageToken != ""; page++ {
		if pageToken == "first" {
			pageToken = ""
		}
		// 防止 API 返回的 NextPageToken 不变导致死循环
		if page > 200 {
			return nil, errors.New("目录过大，分页超过 200 页上限")
		}
		query := map[string]string{
			"parent_id":      id,
			"thumbnail_size": "SIZE_LARGE",
			"with_audit":     "true",
			"limit":          "100",
			"filters":        `{"phase":{"eq":"PHASE_TYPE_COMPLETE"},"trashed":{"eq":false}}`,
			"page_token":     pageToken,
		}
		var resp Files
		_, err := d.request("https://api-drive.mypikpak.net/drive/v1/files", http.MethodGet, func(req *resty.Request) {
			req.SetQueryParams(query)
		}, &resp)
		if err != nil {
			return nil, err
		}
		pageToken = resp.NextPageToken
		res = append(res, resp.Files...)
	}
	return res, nil
}

func (d *PikPak) UploadByOSS(ctx context.Context, params *S3Params, s model.FileStreamer, up driver.UpdateProgress) error {
	ossClient, err := netutil.NewOSSClient(params.Endpoint, params.AccessKeyID, params.AccessKeySecret)
	if err != nil {
		return err
	}
	bucket, err := ossClient.Bucket(params.Bucket)
	if err != nil {
		return err
	}

	err = bucket.PutObject(params.Key, driver.NewLimitedUploadStream(ctx, &driver.ReaderUpdatingProgress{
		Reader:         s,
		UpdateProgress: up,
	}), OssOption(params)...)
	if err != nil {
		return err
	}
	return nil
}

func (d *PikPak) UploadByMultipart(ctx context.Context, params *S3Params, fileSize int64, s model.FileStreamer, up driver.UpdateProgress) error {
	tmpF, err := s.CacheFullAndWriter(&up, nil)
	if err != nil {
		return err
	}

	var (
		chunks    []oss.FileChunk
		parts     []oss.UploadPart
		imur      oss.InitiateMultipartUploadResult
		ossClient *oss.Client
		bucket    *oss.Bucket
	)

	if ossClient, err = netutil.NewOSSClient(params.Endpoint, params.AccessKeyID, params.AccessKeySecret); err != nil {
		return err
	}

	if bucket, err = ossClient.Bucket(params.Bucket); err != nil {
		return err
	}

	ticker := time.NewTicker(time.Hour * 12)
	defer ticker.Stop()
	// 设置超时
	timeout := time.NewTimer(time.Hour * 24)

	if chunks, err = SplitFile(fileSize); err != nil {
		return err
	}

	if imur, err = bucket.InitiateMultipartUpload(params.Key,
		oss.SetHeader(OssSecurityTokenHeaderName, params.SecurityToken),
		oss.UserAgentHeader(OSSUserAgent),
	); err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	wg.Add(len(chunks))

	chunksCh := make(chan oss.FileChunk)
	errCh := make(chan error)
	UploadedPartsCh := make(chan oss.UploadPart)
	quit := make(chan struct{})

	// producer
	go chunksProducer(chunksCh, chunks)
	go func() {
		wg.Wait()
		quit <- struct{}{}
	}()

	completedNum := atomic.Int32{}
	// consumers
	for i := 0; i < ThreadsNum; i++ {
		go func(threadId int) {
			defer func() {
				if r := recover(); r != nil {
					errCh <- fmt.Errorf("recovered in %v", r)
				}
			}()
			for chunk := range chunksCh {
				var part oss.UploadPart // 出现错误就继续尝试，共尝试3次
				for retry := 0; retry < 3; retry++ {
					if utils.IsCanceled(ctx) {
						err = ctx.Err()
						break
					}
					select {
					case <-ticker.C:
						errCh <- errors.Wrap(err, "ossToken 过期")
					default:
					}

					buf := make([]byte, chunk.Size)
					if _, err = tmpF.ReadAt(buf, chunk.Offset); err != nil && !errors.Is(err, io.EOF) {
						continue
					}

					b := driver.NewLimitedUploadStream(ctx, bytes.NewReader(buf))
					if part, err = bucket.UploadPart(imur, b, chunk.Size, chunk.Number, OssOption(params)...); err == nil {
						break
					}
				}
				if err != nil {
					errCh <- errors.Wrap(err, fmt.Sprintf("上传 %s 的第%d个分片时出现错误：%v", s.GetName(), chunk.Number, err))
				} else {
					num := completedNum.Add(1)
					up(float64(num) * 100.0 / float64(len(chunks)))
				}
				UploadedPartsCh <- part
			}
		}(i)
	}

	go func() {
		for part := range UploadedPartsCh {
			parts = append(parts, part)
			wg.Done()
		}
	}()
LOOP:
	for {
		select {
		case <-ticker.C:
			// ossToken 过期
			return err
		case <-quit:
			break LOOP
		case <-errCh:
			return err
		case <-timeout.C:
			return fmt.Errorf("time out")
		}
	}

	// EOF错误是xml的Unmarshal导致的，响应其实是json格式，所以实际上上传是成功的
	if _, err = bucket.CompleteMultipartUpload(imur, parts, OssOption(params)...); err != nil && !errors.Is(err, io.EOF) {
		// 当文件名含有 &< 这两个字符之一时响应的xml解析会出现错误，实际上上传是成功的
		if filename := filepath.Base(s.GetName()); !strings.ContainsAny(filename, "&<") {
			return err
		}
	}
	return nil
}

func chunksProducer(ch chan oss.FileChunk, chunks []oss.FileChunk) {
	for _, chunk := range chunks {
		ch <- chunk
	}
}

func SplitFile(fileSize int64) (chunks []oss.FileChunk, err error) {
	for i := int64(1); i < 10; i++ {
		if fileSize < i*utils.GB { // 文件大小小于iGB时分为i*100片
			if chunks, err = SplitFileByPartNum(fileSize, int(i*100)); err != nil {
				return
			}
			break
		}
	}
	if fileSize > 9*utils.GB { // 文件大小大于9GB时分为1000片
		if chunks, err = SplitFileByPartNum(fileSize, 1000); err != nil {
			return
		}
	}
	// 单个分片大小不能小于1MB
	if chunks[0].Size < 1*utils.MB {
		if chunks, err = SplitFileByPartSize(fileSize, 1*utils.MB); err != nil {
			return
		}
	}
	return
}

// SplitFileByPartNum splits big file into parts by the num of parts.
// Split the file with specified parts count, returns the split result when error is nil.
func SplitFileByPartNum(fileSize int64, chunkNum int) ([]oss.FileChunk, error) {
	if chunkNum <= 0 || chunkNum > 10000 {
		return nil, errors.New("chunkNum invalid")
	}

	if int64(chunkNum) > fileSize {
		return nil, errors.New("oss: chunkNum invalid")
	}

	var chunks []oss.FileChunk
	chunk := oss.FileChunk{}
	chunkN := (int64)(chunkNum)
	for i := int64(0); i < chunkN; i++ {
		chunk.Number = int(i + 1)
		chunk.Offset = i * (fileSize / chunkN)
		if i == chunkN-1 {
			chunk.Size = fileSize/chunkN + fileSize%chunkN
		} else {
			chunk.Size = fileSize / chunkN
		}
		chunks = append(chunks, chunk)
	}

	return chunks, nil
}

// SplitFileByPartSize splits big file into parts by the size of parts.
// Splits the file by the part size. Returns the FileChunk when error is nil.
func SplitFileByPartSize(fileSize int64, chunkSize int64) ([]oss.FileChunk, error) {
	if chunkSize <= 0 {
		return nil, errors.New("chunkSize invalid")
	}

	chunkN := fileSize / chunkSize
	if chunkN >= 10000 {
		return nil, errors.New("Too many parts, please increase part size")
	}

	var chunks []oss.FileChunk
	chunk := oss.FileChunk{}
	for i := int64(0); i < chunkN; i++ {
		chunk.Number = int(i + 1)
		chunk.Offset = i * chunkSize
		chunk.Size = chunkSize
		chunks = append(chunks, chunk)
	}

	if fileSize%chunkSize > 0 {
		chunk.Number = len(chunks) + 1
		chunk.Offset = int64(len(chunks)) * chunkSize
		chunk.Size = fileSize % chunkSize
		chunks = append(chunks, chunk)
	}

	return chunks, nil
}

// OssOption get options
func OssOption(params *S3Params) []oss.Option {
	options := []oss.Option{
		oss.SetHeader(OssSecurityTokenHeaderName, params.SecurityToken),
		oss.UserAgentHeader(OSSUserAgent),
	}
	return options
}
