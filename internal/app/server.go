package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"qvmconsole-manager/webassets"
)

type Server struct {
	cfg      Config
	jobs     *JobManager
	accounts *AccountStore
	csrf     string
	http     *http.Server
}

func NewServer(cfg Config) (*Server, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	server := &Server{
		cfg:      cfg,
		jobs:     NewJobManager(cfg),
		accounts: NewAccountStore(cfg),
		csrf:     hex.EncodeToString(token),
	}
	server.http = &http.Server{
		Handler:           server.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	return server, nil
}

func (s *Server) Run() error {
	var listener net.Listener
	var err error
	if s.cfg.DevMode {
		listener, err = net.Listen("tcp", s.cfg.DevAddress)
	} else {
		_ = os.Remove(s.cfg.SocketPath)
		if err := os.MkdirAll(filepath.Dir(s.cfg.SocketPath), 0o755); err != nil {
			return err
		}
		listener, err = net.Listen("unix", s.cfg.SocketPath)
		if err == nil {
			err = os.Chmod(s.cfg.SocketPath, 0o660)
		}
	}
	if err != nil {
		return err
	}
	defer listener.Close()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.http.Shutdown(ctx)
	}()

	err = s.http.Serve(listener)
	if !s.cfg.DevMode {
		_ = os.Remove(s.cfg.SocketPath)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/session", s.handleSession)
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/channels", s.handleChannels)
	mux.HandleFunc("GET /api/v1/jobs", s.handleJobs)
	mux.HandleFunc("POST /api/v1/jobs", s.handleStartJob)
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.handleJob)
	mux.HandleFunc("GET /api/v1/jobs/{id}/events", s.handleJobEvents)
	mux.HandleFunc("GET /api/v1/jobs/{id}/log", s.handleJobLog)
	mux.HandleFunc("DELETE /api/v1/cache", s.handleClearCache)
	mux.HandleFunc("POST /api/v1/service/{action}", s.handleService)
	mux.HandleFunc("GET /api/v1/logs", s.handleServiceLogs)
	mux.HandleFunc("GET /api/v1/users", s.handleUsers)
	mux.HandleFunc("POST /api/v1/users/default-admin/reset", s.handleResetAdmin)
	mux.HandleFunc("POST /api/v1/users/{id}/totp/clear", s.handleClearTOTP)
	mux.HandleFunc("POST /api/v1/users/{id}/password", s.handleChangePassword)
	mux.HandleFunc("PUT /api/v1/config/port", s.handleChangePort)
	mux.Handle("/", s.staticHandler())

	admin := s.requireAdmin(mux)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "same-origin")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'self'")
		path := request.URL.Path
		if path == s.cfg.GatewayPrefix {
			clone := request.Clone(request.Context())
			clone.URL.Path = "/"
			admin.ServeHTTP(writer, clone)
			return
		}
		if strings.HasPrefix(path, s.cfg.GatewayPrefix+"/") {
			clone := request.Clone(request.Context())
			clone.URL.Path = strings.TrimPrefix(path, s.cfg.GatewayPrefix)
			admin.ServeHTTP(writer, clone)
			return
		}
		admin.ServeHTTP(writer, request)
	})
}

func (s *Server) staticHandler() http.Handler {
	dist, err := fs.Sub(webassets.Dist, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			http.NotFound(writer, request)
			return
		}
		clean := strings.TrimPrefix(request.URL.Path, "/")
		if clean == "" {
			clean = "index.html"
		}
		if clean == "index.html" {
			writer.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		} else if strings.HasPrefix(clean, "assets/") {
			writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		if _, err := fs.Stat(dist, clean); err != nil {
			writer.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			clone := request.Clone(request.Context())
			clone.URL.Path = "/"
			files.ServeHTTP(writer, clone)
			return
		}
		files.ServeHTTP(writer, request)
	})
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/api/") {
			isAdmin := strings.EqualFold(request.Header.Get("X-Trim-Isadmin"), "true")
			if !isAdmin && !s.cfg.DevMode {
				writeError(writer, http.StatusForbidden, "admin_required", "仅飞牛管理员可以使用此管理器")
				return
			}
			if request.Method != http.MethodGet && request.Method != http.MethodHead && request.Method != http.MethodOptions {
				provided := request.Header.Get("X-CSRF-Token")
				if len(provided) != len(s.csrf) || subtle.ConstantTimeCompare([]byte(provided), []byte(s.csrf)) != 1 {
					writeError(writer, http.StatusForbidden, "csrf_rejected", "CSRF 校验失败，请刷新页面")
					return
				}
				if !sameOrigin(request) {
					logOriginRejection(request)
					writeError(writer, http.StatusForbidden, "origin_rejected", "请求来源校验失败")
					return
				}
			}
		}
		next.ServeHTTP(writer, request)
	})
}

func sameOrigin(request *http.Request) bool {
	// 该请求头只能由管理器前端的 fetch 添加。跨域页面添加自定义请求头时
	// 浏览器会先执行 CORS 预检，且无法读取本接口签发的 CSRF Token。
	if subtle.ConstantTimeCompare([]byte(request.Header.Get("X-QVMC-Client")), []byte("qvmconsole-manager")) == 1 {
		return true
	}

	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}

	// 浏览器会为同源 fetch 设置该请求头，前端脚本本身不能伪造。
	// 飞牛统一网关可能将 Host 改写为内部值，因此优先采用此信号。
	fetchSite := strings.ToLower(strings.TrimSpace(request.Header.Get("Sec-Fetch-Site")))
	if fetchSite != "" {
		return fetchSite == "same-origin"
	}

	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return false
	}

	for _, expected := range forwardedHosts(request) {
		if strings.EqualFold(parsed.Host, expected) {
			return true
		}
	}
	return false
}

func logOriginRejection(request *http.Request) {
	fmt.Fprintf(os.Stderr, "请求来源校验失败: origin=%q host=%q forwardedHost=%q originalHost=%q fetchSite=%q client=%q\n",
		request.Header.Get("Origin"),
		request.Host,
		request.Header.Get("X-Forwarded-Host"),
		request.Header.Get("X-Original-Host"),
		request.Header.Get("Sec-Fetch-Site"),
		request.Header.Get("X-QVMC-Client"),
	)
}

func forwardedHosts(request *http.Request) []string {
	values := []string{request.Header.Get("X-Forwarded-Host"), request.Header.Get("X-Original-Host"), request.Host}
	hosts := make([]string, 0, len(values)+1)
	for _, value := range values {
		for _, host := range strings.Split(value, ",") {
			host = strings.Trim(strings.TrimSpace(host), `"`)
			if host != "" {
				hosts = append(hosts, host)
			}
		}
	}
	for _, part := range strings.Split(request.Header.Get("Forwarded"), ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(key, "host") {
			value = strings.Trim(strings.TrimSpace(value), `"`)
			if value != "" {
				hosts = append(hosts, value)
			}
		}
	}
	return hosts
}

func (s *Server) handleSession(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"csrfToken": s.csrf,
		"user": map[string]string{
			"id":       request.Header.Get("X-Trim-Userid"),
			"username": request.Header.Get("X-Trim-Username"),
		},
		"gatewayPrefix": s.cfg.GatewayPrefix,
	})
}

func (s *Server) handleStatus(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
	defer cancel()
	writeJSON(writer, http.StatusOK, collectStatus(ctx, s.cfg, s.jobs))
}

func (s *Server) handleChannels(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, s.cfg.Catalog.Channels())
}

func (s *Server) handleJobs(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, s.jobs.Recent())
}

func (s *Server) handleStartJob(writer http.ResponseWriter, request *http.Request) {
	var payload JobRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	job, err := s.jobs.Start(payload)
	if err != nil {
		writeError(writer, http.StatusConflict, "job_rejected", err.Error())
		return
	}
	writeJSON(writer, http.StatusAccepted, job)
}

func (s *Server) handleJob(writer http.ResponseWriter, request *http.Request) {
	job, ok := s.jobs.Get(request.PathValue("id"))
	if !ok {
		writeError(writer, http.StatusNotFound, "job_not_found", "任务不存在")
		return
	}
	writeJSON(writer, http.StatusOK, job)
}

func (s *Server) handleJobEvents(writer http.ResponseWriter, request *http.Request) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, "stream_unsupported", "当前网关不支持实时日志")
		return
	}
	stream, cancel, err := s.jobs.Subscribe(request.PathValue("id"))
	if err != nil {
		writeError(writer, http.StatusNotFound, "job_not_found", err.Error())
		return
	}
	defer cancel()
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("X-Accel-Buffering", "no")
	for {
		select {
		case <-request.Context().Done():
			return
		case message, open := <-stream:
			if !open {
				return
			}
			data, _ := json.Marshal(message)
			_, _ = fmt.Fprintf(writer, "event: log\ndata: %s\n\n", data)
			flusher.Flush()
		case <-time.After(15 * time.Second):
			_, _ = io.WriteString(writer, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleJobLog(writer http.ResponseWriter, request *http.Request) {
	path, ok := s.jobs.LogPath(request.PathValue("id"))
	if !ok {
		writeError(writer, http.StatusNotFound, "job_not_found", "任务不存在")
		return
	}
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("Content-Disposition", "attachment; filename=job-"+request.PathValue("id")+".log")
	http.ServeFile(writer, request, path)
}

func (s *Server) handleClearCache(writer http.ResponseWriter, _ *http.Request) {
	if err := s.jobs.ClearCache(); err != nil {
		writeError(writer, http.StatusConflict, "cache_busy", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"cleared": true})
}

func (s *Server) handleService(writer http.ResponseWriter, request *http.Request) {
	action := request.PathValue("action")
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	if err := controlService(ctx, s.cfg, action); err != nil {
		writeError(writer, http.StatusBadGateway, "service_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"action": action})
}

func (s *Server) handleServiceLogs(writer http.ResponseWriter, request *http.Request) {
	lines, _ := strconv.Atoi(request.URL.Query().Get("lines"))
	if lines <= 0 || lines > 2000 {
		lines = 300
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	output, err := commandOutput(ctx, "journalctl", "-u", s.cfg.ServiceName, "--no-pager", "-n", strconv.Itoa(lines))
	output = redactLog(output)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "logs_failed", "读取服务日志失败: "+output)
		return
	}
	if request.URL.Query().Get("download") == "1" {
		writer.Header().Set("Content-Disposition", "attachment; filename=qvmconsole-service.log")
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = writer.Write([]byte(output))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"content": output})
}

func (s *Server) handleUsers(writer http.ResponseWriter, request *http.Request) {
	users, err := s.accounts.ListUsers(request.Context())
	if err != nil {
		writeError(writer, http.StatusBadGateway, "users_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, users)
}

func (s *Server) handleResetAdmin(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Confirmation string `json:"confirmation"`
	}
	if err := decodeJSON(request, &payload); err != nil || payload.Confirmation != "RESET ADMIN" {
		writeError(writer, http.StatusBadRequest, "confirmation_required", "请输入 RESET ADMIN 完成二次确认")
		return
	}
	if err := s.accounts.ResetDefaultAdmin(request.Context()); err != nil {
		writeError(writer, http.StatusBadGateway, "reset_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"username": "admin", "password": "admin123"})
}

func (s *Server) handleClearTOTP(writer http.ResponseWriter, request *http.Request) {
	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_user", "用户 ID 无效")
		return
	}
	var payload struct {
		Confirmation string `json:"confirmation"`
	}
	if err := decodeJSON(request, &payload); err != nil || payload.Confirmation != "CLEAR TOTP" {
		writeError(writer, http.StatusBadRequest, "confirmation_required", "请输入 CLEAR TOTP 完成二次确认")
		return
	}
	if err := s.accounts.ClearTOTP(request.Context(), id); err != nil {
		writeError(writer, http.StatusBadGateway, "totp_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"cleared": true})
}

func (s *Server) handleChangePassword(writer http.ResponseWriter, request *http.Request) {
	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_user", "用户 ID 无效")
		return
	}
	var payload struct {
		Password         string `json:"password"`
		ConfirmPassword  string `json:"confirmPassword"`
		AllowUnavailable bool   `json:"allowUnavailable"`
		Confirmation     string `json:"confirmation"`
	}
	if err := decodeJSON(request, &payload); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if payload.Password != payload.ConfirmPassword {
		writeError(writer, http.StatusBadRequest, "password_mismatch", "两次输入的密码不一致")
		return
	}
	if payload.Confirmation != "CHANGE PASSWORD" {
		writeError(writer, http.StatusBadRequest, "confirmation_required", "请输入 CHANGE PASSWORD 完成二次确认")
		return
	}
	security, err := s.accounts.ChangeAdminPassword(request.Context(), id, payload.Password, payload.AllowUnavailable)
	if err != nil {
		var accountErr *AccountError
		if errors.As(err, &accountErr) {
			status := http.StatusUnprocessableEntity
			if accountErr.Code == "password_check_unavailable" {
				status = http.StatusConflict
			}
			writeJSON(writer, status, map[string]any{"error": map[string]string{"code": accountErr.Code, "message": accountErr.Message}, "security": security})
			return
		}
		writeError(writer, http.StatusBadGateway, "password_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"changed": true, "security": security})
}

func (s *Server) handleChangePort(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Port         int    `json:"port"`
		Confirmation string `json:"confirmation"`
	}
	if err := decodeJSON(request, &payload); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if payload.Confirmation != "CHANGE PORT" {
		writeError(writer, http.StatusBadRequest, "confirmation_required", "请输入 CHANGE PORT 完成二次确认")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 45*time.Second)
	defer cancel()
	if err := changePort(ctx, s.cfg, payload.Port); err != nil {
		writeError(writer, http.StatusBadGateway, "port_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]int{"port": payload.Port})
}

func decodeJSON(request *http.Request, destination any) error {
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("请求内容无效: %w", err)
	}
	return nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
