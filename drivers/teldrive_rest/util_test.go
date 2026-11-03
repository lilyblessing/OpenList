package teldrive_rest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
)

// fakeTeleDrive 是 TeleDrive 私有 REST API 的内存假实现，行为对照真服务端：
//   - POST /login：密码正确返回 303 + Set-Cookie: teledrive_session=...，错误返回 200 且无 Set-Cookie
//   - 所有 /api/* 必须带 teledrive_session Cookie，否则 401
//   - GET /api/files 空目录返回字面量 null
type fakeTeleDrive struct {
	t             *testing.T
	mu            sync.Mutex
	password      string
	chunkSize     int64
	totalParts    int
	chunks        map[int][]byte
	initCalls     int
	chunkCalls    int
	completeCalls int

	failChunk    bool   // 置位后，下一个 /api/upload/chunk 请求返回 500（一次性）
	folderName   string // 最近一次 POST /api/folders 收到的 name
	folderParent string // 最近一次 POST /api/folders 收到的 parent_id（null 解码为空串）

	// 原始 JSON 值捕获：用于断言「根目录必须发 null 而不是空串」。
	// present 表示字段是否出现在请求体里；raw 为该字段的原始解码值（null → nil）。
	folderParentPresent bool
	folderParentRaw     interface{}
	initFolderPresent   bool
	initFolderRaw       interface{}

	// —— 401 重登 / rename / move / batchMove 用例新增 ——
	loginCalls   int    // 收到的 POST /login 请求总数
	sessionToken string // 当前有效会话 token；携带不匹配 token 的 /api/* 请求视为会话失效（401）
	force401     bool   // 置位后所有 /api/* 一律 401（重登也不放行），用于验证「只重试一次」

	// 最近一次 rename/move/batchMove 类 JSON 请求的捕获（方法、路径、解码后的请求体）
	lastMethod string
	lastPath   string
	lastBody   map[string]interface{}
}

func newFakeTeleDrive(t *testing.T, password string, chunkSize int64, totalParts int) (*fakeTeleDrive, *httptest.Server) {
	f := &fakeTeleDrive{
		t:            t,
		password:     password,
		chunkSize:    chunkSize,
		totalParts:   totalParts,
		chunks:       make(map[int][]byte),
		sessionToken: "faketoken",
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeTeleDrive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Path == "/login" {
		f.loginCalls++
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
			return
		}
		if r.FormValue("password") == f.password {
			w.Header().Add("Set-Cookie", sessionCookieName+"="+f.sessionToken+"; Path=/")
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		// 密码错误：200 且不带 Set-Cookie
		w.WriteHeader(http.StatusOK)
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	if f.force401 {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	// 会话校验：不仅要带 Cookie，token 还必须与服务端当前有效 token 一致，
	// 否则视为会话失效返回 401（TestEnsureCookieReloginOn401 靠换 token 模拟失效）。
	if c, err := r.Cookie(sessionCookieName); err != nil || c.Value != f.sessionToken {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	switch {
	case r.URL.Path == "/api/upload/init":
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.initCalls++
		if v, ok := req["folder_id"]; ok {
			f.initFolderPresent = true
			f.initFolderRaw = v
		} else {
			f.initFolderPresent = false
			f.initFolderRaw = nil
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(UploadInitResp{
			ID:         "upload-session-1",
			ChunkSize:  f.chunkSize,
			TotalParts: f.totalParts,
		})
	case r.URL.Path == "/api/upload/chunk":
		if f.failChunk {
			f.failChunk = false
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = r.FormValue("session_id")
		idx, err := strconv.Atoi(r.FormValue("chunk_index"))
		if err != nil {
			http.Error(w, "bad chunk_index: "+err.Error(), http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("chunk")
		if err != nil {
			http.Error(w, "missing chunk file: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.chunks[idx] = data
		f.chunkCalls++
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == "/api/upload/complete":
		f.completeCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(File{ID: "file-1", Name: "completed"})
	case r.URL.Path == "/api/folders" && r.Method == http.MethodPost:
		// 用 map 解码以保留 null 与空串的区别（struct 的 string 字段会把 null 吞成 ""）
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if v, ok := body["name"].(string); ok {
			f.folderName = v
		}
		if v, ok := body["parent_id"]; ok {
			f.folderParentPresent = true
			f.folderParentRaw = v
			if s, ok := v.(string); ok {
				f.folderParent = s
			} else {
				f.folderParent = ""
			}
		} else {
			f.folderParentPresent = false
			f.folderParentRaw = nil
			f.folderParent = ""
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Folder{ID: "folder-1", Name: f.folderName})
	case r.URL.Path == "/api/files" && r.Method == http.MethodGet:
		// 空目录时真服务端返回字面量 null（不是 []）
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("null"))
	case strings.HasPrefix(r.URL.Path, "/api/files/") && r.Method == http.MethodPut:
		// renameFile / moveFile 共用 PUT /api/files/{id}
		if !f.captureJSON(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(r.URL.Path, "/api/folders/") && r.Method == http.MethodPut:
		// renameFolder / moveFolder 共用 PUT /api/folders/{id}
		if !f.captureJSON(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/api/batch/move" && r.Method == http.MethodPost:
		if !f.captureJSON(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{})
	case strings.HasPrefix(r.URL.Path, "/api/files/") && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// newTestDriver 构造一个不经过 Init 的驱动实例，client 照抄 Init 里的
// 「不跟随重定向」策略（http.ErrUseLastResponse），否则 /login 的 303 会被
// 自动跟随、读不到 Set-Cookie。
func newTestDriver(srv *httptest.Server, password string) *TeleDriveREST {
	d := &TeleDriveREST{}
	d.Addition.Address = srv.URL
	d.Addition.Password = password
	d.client = resty.New().
		SetRedirectPolicy(resty.RedirectPolicyFunc(func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		})).
		SetBaseURL(srv.URL)
	return d
}

func (d *TeleDriveREST) currentSession() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.session
}

func (f *fakeTeleDrive) snapshot() (chunks map[int][]byte, chunkCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(map[int][]byte, len(f.chunks))
	for k, v := range f.chunks {
		cp[k] = v
	}
	return cp, f.chunkCalls
}

// captureJSON 读取并解码 JSON 请求体到 lastBody，供 rename/move/batchMove 用例断言。
// 返回 false 表示已写出错误响应，handler 应直接 return。
// 仅在 ServeHTTP（已持 f.mu）内调用。
func (f *fakeTeleDrive) captureJSON(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	f.lastMethod = r.Method
	f.lastPath = r.URL.Path
	f.lastBody = m
	return true
}

// lastRequest 返回最近一次捕获的 JSON 请求（方法、路径、请求体）。
func (f *fakeTeleDrive) lastRequest() (method, path string, body map[string]interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastMethod, f.lastPath, f.lastBody
}

func (f *fakeTeleDrive) loginCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginCalls
}

func TestChunkCount(t *testing.T) {
	cases := []struct {
		name      string
		size      int64
		chunkSize int64
		want      int64
	}{
		{"size=0", 0, 4, 0},
		{"size<chunkSize", 3, 4, 1},
		{"size==chunkSize", 4, 4, 1},
		{"size=chunkSize+1", 5, 4, 2},
		{"negative size", -1, 4, 0},
		{"chunkSize=0", 10, 0, 0},
		{"3MB/5MB", 3 * 1024 * 1024, 5 * 1024 * 1024, 1},
		{"12MB/5MB", 12 * 1024 * 1024, 5 * 1024 * 1024, 3},
		{"100MB/5MB", 100 * 1024 * 1024, 5 * 1024 * 1024, 20},
	}
	for _, c := range cases {
		if got := chunkCount(c.size, c.chunkSize); got != c.want {
			t.Errorf("%s: chunkCount(%d, %d) = %d, want %d", c.name, c.size, c.chunkSize, got, c.want)
		}
	}
}

func TestLoginSuccessAndFailure(t *testing.T) {
	_, srv := newFakeTeleDrive(t, "admin123", 4, 1)
	ctx := context.Background()

	// 正确密码：session 非空
	d := newTestDriver(srv, "admin123")
	if err := d.login(ctx); err != nil {
		t.Fatalf("正确密码登录失败: %v", err)
	}
	if token := d.currentSession(); token == "" {
		t.Fatalf("正确密码登录后 session 为空")
	} else if token != "faketoken" {
		t.Errorf("session token = %q, want %q", token, "faketoken")
	}

	// 错误密码：返回 error 且 session 为空
	d2 := newTestDriver(srv, "wrong-password")
	if err := d2.login(ctx); err == nil {
		t.Fatalf("错误密码登录应返回 error，实际 nil")
	}
	if token := d2.currentSession(); token != "" {
		t.Errorf("错误密码登录后 session = %q, want 空", token)
	}
}

func TestCreateFolder(t *testing.T) {
	f, srv := newFakeTeleDrive(t, "admin123", 4, 1)
	d := newTestDriver(srv, "admin123")
	if err := d.createFolder(context.Background(), "新目录", ""); err != nil {
		t.Fatalf("createFolder 失败: %v", err)
	}
	f.mu.Lock()
	name, parent := f.folderName, f.folderParent
	f.mu.Unlock()
	if name != "新目录" {
		t.Errorf("服务端收到 name = %q, want %q", name, "新目录")
	}
	if parent != "" {
		t.Errorf("服务端收到 parent_id = %q, want 空串", parent)
	}
}

func TestUploadSingleChunk(t *testing.T) {
	f, srv := newFakeTeleDrive(t, "admin123", 4, 1)
	d := newTestDriver(srv, "admin123")
	if err := d.uploadChunk(context.Background(), "upload-session-1", 0, []byte("abc")); err != nil {
		t.Fatalf("uploadChunk 失败: %v", err)
	}
	chunks, chunkCalls := f.snapshot()
	data, ok := chunks[0]
	if !ok {
		t.Fatalf("假服务端未收到 chunk 0")
	}
	if string(data) != "abc" {
		t.Errorf("chunks[0] = %q, want %q", data, "abc")
	}
	if chunkCalls != 1 {
		t.Errorf("chunkCalls = %d, want 1", chunkCalls)
	}
}

func TestUploadMultiChunkLastPartial(t *testing.T) {
	f, srv := newFakeTeleDrive(t, "admin123", 4, 1)
	d := newTestDriver(srv, "admin123")

	// 10 字节、chunkSize=4 → 3 块（4+4+2）
	if got := chunkCount(10, 4); got != 3 {
		t.Fatalf("chunkCount(10, 4) = %d, want 3", got)
	}
	data := []byte("0123456789")
	for i := 0; i < 3; i++ {
		start := int64(i) * 4
		end := start + 4
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		if err := d.uploadChunk(context.Background(), "upload-session-1", i, data[start:end]); err != nil {
			t.Fatalf("uploadChunk 第 %d 块失败: %v", i, err)
		}
	}
	chunks, _ := f.snapshot()
	if len(chunks) != 3 {
		t.Errorf("共收到 %d 块, want 3", len(chunks))
	}
	last, ok := chunks[2]
	if !ok {
		t.Fatalf("假服务端未收到 chunk 2")
	}
	if len(last) != 2 {
		t.Errorf("第 3 块长度 = %d, want 2", len(last))
	}
	if string(last) != "89" {
		t.Errorf("第 3 块内容 = %q, want %q", last, "89")
	}
}

func TestUploadChunkServerError(t *testing.T) {
	f, srv := newFakeTeleDrive(t, "admin123", 4, 1)
	d := newTestDriver(srv, "admin123")
	f.mu.Lock()
	f.failChunk = true
	f.mu.Unlock()
	// 单次语义：500 应直接返回 error。重试逻辑在 Put 里，这里不断言重试。
	if err := d.uploadChunk(context.Background(), "upload-session-1", 0, []byte("abc")); err == nil {
		t.Fatalf("服务端返回 500 时 uploadChunk 应返回 error，实际 nil")
	}
}

func TestListFilesNullBody(t *testing.T) {
	_, srv := newFakeTeleDrive(t, "admin123", 4, 1)
	d := newTestDriver(srv, "admin123")
	files, err := d.listFiles(context.Background(), "")
	if err != nil {
		t.Fatalf("listFiles 对字面量 null 响应不应报错: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("listFiles 返回 %d 个文件, want 0", len(files))
	}
}

// 根目录建目录时 parent_id 必须发 JSON null（字段存在且为 nil），不能是空串——
// 真服务端 folders.parent_id 有外键约束，空串实测 500 FOREIGN KEY constraint failed。
func TestCreateFolderAtRootSendsNull(t *testing.T) {
	f, srv := newFakeTeleDrive(t, "admin123", 4, 1)
	d := newTestDriver(srv, "admin123")
	if err := d.createFolder(context.Background(), "根目录", ""); err != nil {
		t.Fatalf("createFolder 失败: %v", err)
	}
	f.mu.Lock()
	present, raw := f.folderParentPresent, f.folderParentRaw
	f.mu.Unlock()
	if !present {
		t.Fatalf("服务端未收到 parent_id 字段，请求体应包含 \"parent_id\": null")
	}
	if raw != nil {
		t.Errorf("根目录 createFolder 的 parent_id = %v (%T), want nil（JSON null）", raw, raw)
	}
}

// 根目录上传初始化时 folder_id 必须发 JSON null（字段存在且为 nil），不能是空串——
// 真服务端 upload_sessions.folder_id 有外键约束，空串实测 500 FOREIGN KEY constraint failed。
func TestUploadInitAtRootSendsNull(t *testing.T) {
	f, srv := newFakeTeleDrive(t, "admin123", 4, 1)
	d := newTestDriver(srv, "admin123")
	if _, err := d.uploadInit(context.Background(), "test.bin", 10, "application/octet-stream", ""); err != nil {
		t.Fatalf("uploadInit 失败: %v", err)
	}
	f.mu.Lock()
	present, raw := f.initFolderPresent, f.initFolderRaw
	f.mu.Unlock()
	if !present {
		t.Fatalf("服务端未收到 folder_id 字段，请求体应包含 \"folder_id\": null")
	}
	if raw != nil {
		t.Errorf("根目录 uploadInit 的 folder_id = %v (%T), want nil（JSON null）", raw, raw)
	}
}

// 会话失效后驱动应自动重登一次并成功：先把服务端有效 token 换掉（旧 token 一律 401），
// 再调 listFolders，断言调用成功且 /login 被请求了至少两次（首次 + 自动重登）。
func TestEnsureCookieReloginOn401(t *testing.T) {
	f, srv := newFakeTeleDrive(t, "admin123", 4, 1)
	d := newTestDriver(srv, "admin123")
	ctx := context.Background()

	if err := d.login(ctx); err != nil {
		t.Fatalf("首次登录失败: %v", err)
	}

	// 模拟服务端会话失效：换发新 token，旧 token（faketoken）从此 401
	f.mu.Lock()
	f.sessionToken = "rotated-token"
	f.mu.Unlock()

	folders, err := d.listFolders(ctx, "")
	if err != nil {
		t.Fatalf("旧 token 401 后 listFolders 应自动重登成功: %v", err)
	}
	if len(folders) != 0 {
		t.Errorf("listFolders 返回 %d 个目录, want 0", len(folders))
	}
	if calls := f.loginCallCount(); calls < 2 {
		t.Errorf("loginCalls = %d, want >= 2（应包含 401 后的自动重登）", calls)
	}
}

// 重登一次后仍 401 则必须放弃返回 error，不能无限重试。
// 用 context.WithTimeout 兜底：若实现变成死循环，测试在 5 秒后因 ctx 超时报错，
// 而不是挂住整个 CI。
func TestEnsureCookieRetryOnlyOnce(t *testing.T) {
	f, srv := newFakeTeleDrive(t, "admin123", 4, 1)
	d := newTestDriver(srv, "admin123")
	f.mu.Lock()
	f.force401 = true
	f.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := d.listFolders(ctx, ""); err == nil {
		t.Fatalf("重登后仍 401 时 listFolders 应返回 error，实际 nil")
	}
	if calls := f.loginCallCount(); calls > 2 {
		t.Errorf("loginCalls = %d, want <= 2（首次登录 + 重登一次，不允许无限重试）", calls)
	}
}

// 改文件名：必须发出 PUT /api/files/{id} 且 body.name 正确；
// 空名字驱动侧应主动拒绝（服务端会静默忽略空名），且不发任何请求。
func TestRenameFile(t *testing.T) {
	f, srv := newFakeTeleDrive(t, "admin123", 4, 1)
	d := newTestDriver(srv, "admin123")
	ctx := context.Background()

	if err := d.renameFile(ctx, "file-9", "新名字.bin"); err != nil {
		t.Fatalf("renameFile 失败: %v", err)
	}
	method, path, body := f.lastRequest()
	if method != http.MethodPut || path != "/api/files/file-9" {
		t.Errorf("请求 %s %s, want PUT /api/files/file-9", method, path)
	}
	if got, _ := body["name"].(string); got != "新名字.bin" {
		t.Errorf("body.name = %q, want %q", got, "新名字.bin")
	}

	// 空名字：返回 error，且 lastRequest 不变（没有发出新请求）
	if err := d.renameFile(ctx, "file-9", ""); err == nil {
		t.Fatalf("空名字 renameFile 应返回 error，实际 nil")
	}
	m2, p2, b2 := f.lastRequest()
	if m2 != method || p2 != path || b2["name"] != "新名字.bin" {
		t.Errorf("空名字请求不应发出，lastRequest 变为 %s %s %v", m2, p2, b2)
	}
}

// 改目录名：必须发出 PUT /api/folders/{id} 且 body.name 正确；空名字驱动侧拒绝。
func TestRenameFolder(t *testing.T) {
	f, srv := newFakeTeleDrive(t, "admin123", 4, 1)
	d := newTestDriver(srv, "admin123")
	ctx := context.Background()

	if err := d.renameFolder(ctx, "folder-7", "新目录名"); err != nil {
		t.Fatalf("renameFolder 失败: %v", err)
	}
	method, path, body := f.lastRequest()
	if method != http.MethodPut || path != "/api/folders/folder-7" {
		t.Errorf("请求 %s %s, want PUT /api/folders/folder-7", method, path)
	}
	if got, _ := body["name"].(string); got != "新目录名" {
		t.Errorf("body.name = %q, want %q", got, "新目录名")
	}

	if err := d.renameFolder(ctx, "folder-7", ""); err == nil {
		t.Fatalf("空名字 renameFolder 应返回 error，实际 nil")
	}
	m2, p2, b2 := f.lastRequest()
	if m2 != method || p2 != path || b2["name"] != "新目录名" {
		t.Errorf("空名字请求不应发出，lastRequest 变为 %s %s %v", m2, p2, b2)
	}
}

// 非根移动文件：必须发出 PUT /api/files/{id} 且 body.folder_id 为目标 ID 字符串；
// 空 dstFolderID 应返回 error（约定：移到根必须走 batchMove），且不发请求。
func TestMoveFileNonRoot(t *testing.T) {
	f, srv := newFakeTeleDrive(t, "admin123", 4, 1)
	d := newTestDriver(srv, "admin123")
	ctx := context.Background()

	if err := d.moveFile(ctx, "file-9", "folder-dst"); err != nil {
		t.Fatalf("moveFile 失败: %v", err)
	}
	method, path, body := f.lastRequest()
	if method != http.MethodPut || path != "/api/files/file-9" {
		t.Errorf("请求 %s %s, want PUT /api/files/file-9", method, path)
	}
	if got, _ := body["folder_id"].(string); got != "folder-dst" {
		t.Errorf("body.folder_id = %q, want %q", got, "folder-dst")
	}

	if err := d.moveFile(ctx, "file-9", ""); err == nil {
		t.Fatalf("空 dstFolderID 的 moveFile 应返回 error，实际 nil")
	}
	m2, p2, b2 := f.lastRequest()
	if m2 != method || p2 != path || b2["folder_id"] != "folder-dst" {
		t.Errorf("空 dstFolderID 请求不应发出，lastRequest 变为 %s %s %v", m2, p2, b2)
	}
}

// 移到根：POST /api/batch/move 的 body 里 target_folder_id 必须存在且为 JSON null
// （不是空串——服务端 folder_id 是 *string，null 才表示根），且 file_ids / folder_ids 存在。
func TestBatchMoveToRootSendsNull(t *testing.T) {
	f, srv := newFakeTeleDrive(t, "admin123", 4, 1)
	d := newTestDriver(srv, "admin123")

	if err := d.batchMove(context.Background(), []string{"file-1"}, []string{"folder-1"}, ""); err != nil {
		t.Fatalf("batchMove 失败: %v", err)
	}
	method, path, body := f.lastRequest()
	if method != http.MethodPost || path != "/api/batch/move" {
		t.Fatalf("请求 %s %s, want POST /api/batch/move", method, path)
	}
	raw, present := body["target_folder_id"]
	if !present {
		t.Fatalf("请求体缺少 target_folder_id 字段，应包含 \"target_folder_id\": null")
	}
	if raw != nil {
		t.Errorf("移到根的 target_folder_id = %v (%T), want nil（JSON null，不是空串）", raw, raw)
	}
	if _, ok := body["file_ids"]; !ok {
		t.Errorf("请求体缺少 file_ids 字段")
	}
	if _, ok := body["folder_ids"]; !ok {
		t.Errorf("请求体缺少 folder_ids 字段")
	}
}
