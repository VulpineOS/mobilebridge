package mobilebridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type workerAttachedSession interface {
	BrowserURL() string
	StartRecording(context.Context, string) error
	StopRecording(context.Context) error
	Close() error
	Done() <-chan struct{}
}

type WorkerControlServer struct {
	addr       string
	listenAddr string

	mu            sync.Mutex
	httpSrv       *http.Server
	sessions      map[string]*workerControlSession
	recordings    map[string]*workerControlRecording
	startAttached func(context.Context, string, string) (workerAttachedSession, error)
	listDevices   func(context.Context) ([]Device, error)
	enrichDevice  func(context.Context, *Device) error
	socketInfo    func(context.Context, string) (DevtoolsSocket, error)
	newSessionID  func() string
	maxSessions   int
	pending       int
	requests      int
	failures      int
	lastError     string
	controlToken  string
}

type workerControlSession struct {
	id          string
	deviceID    string
	session     workerAttachedSession
	recordingID string
}

type workerControlRecording struct {
	id        string
	sessionID string
	path      string
}

type WorkerAttachRequest struct {
	DeviceID string `json:"device_id"`
}

type WorkerAttachResponse struct {
	SessionID string `json:"session_id"`
	DeviceID  string `json:"device_id"`
	Endpoint  string `json:"endpoint,omitempty"`
}

type WorkerCreateTargetRequest struct {
	URL string `json:"url"`
}

type WorkerCreateTargetResponse struct {
	ID                   string `json:"id"`
	Title                string `json:"title,omitempty"`
	URL                  string `json:"url,omitempty"`
	Type                 string `json:"type,omitempty"`
	DevtoolsFrontendURL  string `json:"devtoolsFrontendUrl,omitempty"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl,omitempty"`
}

type WorkerRecordingResponse struct {
	RecordingID string `json:"recording_id"`
	FileName    string `json:"file_name,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
}

type WorkerHeartbeatDevice struct {
	Platform     string   `json:"platform"`
	DeviceID     string   `json:"device_id"`
	State        string   `json:"state,omitempty"`
	Name         string   `json:"name,omitempty"`
	Model        string   `json:"model,omitempty"`
	Product      string   `json:"product,omitempty"`
	OSVersion    string   `json:"os_version,omitempty"`
	Inspectable  bool     `json:"inspectable"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type WorkerHeartbeat struct {
	WorkerID       string                  `json:"worker_id"`
	Hostname       string                  `json:"hostname,omitempty"`
	AdvertiseAddr  string                  `json:"advertise_addr,omitempty"`
	Healthy        bool                    `json:"healthy"`
	ActiveSessions int                     `json:"active_sessions,omitempty"`
	QueueDepth     int                     `json:"queue_depth,omitempty"`
	MaxSessions    int                     `json:"max_sessions,omitempty"`
	FailureRate    float64                 `json:"failure_rate,omitempty"`
	LastError      string                  `json:"last_error,omitempty"`
	Devices        []WorkerHeartbeatDevice `json:"devices,omitempty"`
}

func NewWorkerControlServer(addr string) *WorkerControlServer {
	return &WorkerControlServer{
		addr:        addr,
		sessions:    make(map[string]*workerControlSession),
		recordings:  make(map[string]*workerControlRecording),
		listDevices: ListDevices,
		enrichDevice: func(ctx context.Context, device *Device) error {
			return device.Enrich(ctx)
		},
		socketInfo: ChromeDevtoolsSocketInfo,
		startAttached: func(ctx context.Context, serial, addr string) (workerAttachedSession, error) {
			return StartAttachedServer(ctx, serial, addr)
		},
		newSessionID: func() string {
			return "mbw_" + randomSuffix(4)
		},
	}
}

func (s *WorkerControlServer) SetMaxSessions(value int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxSessions = value
}

func (s *WorkerControlServer) SetControlToken(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.controlToken = strings.TrimSpace(value)
}

func (s *WorkerControlServer) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listenAddr
}

func (s *WorkerControlServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/sessions", s.handleSessions)
	mux.HandleFunc("/sessions/", s.handleSession)
	mux.HandleFunc("/recordings/", s.handleRecording)

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.mu.Lock()
	s.httpSrv = srv
	s.listenAddr = ln.Addr().String()
	s.mu.Unlock()
	go func() {
		_ = srv.Serve(ln)
	}()
	return nil
}

func (s *WorkerControlServer) Stop() error {
	s.mu.Lock()
	srv := s.httpSrv
	sessions := make([]*workerControlSession, 0, len(s.sessions))
	recordings := make([]*workerControlRecording, 0, len(s.recordings))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	for _, recording := range s.recordings {
		recordings = append(recordings, recording)
	}
	s.mu.Unlock()

	var err error
	for _, session := range sessions {
		if closeErr := session.session.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	for _, recording := range recordings {
		if removeErr := os.Remove(recording.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
			err = removeErr
		}
	}
	if srv == nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if shutdownErr := srv.Shutdown(ctx); shutdownErr != nil && err == nil {
		err = shutdownErr
	}
	return err
}

func (s *WorkerControlServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		s.recordRequest(errors.New("unauthorized worker control request"))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var req WorkerAttachRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if strings.TrimSpace(req.DeviceID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device_id is required"})
		return
	}
	if !s.reserveAttachSlot() {
		s.recordRequest(errors.New("worker at max sessions"))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "worker at max sessions"})
		return
	}
	defer s.releaseAttachSlot()
	port, err := freeTCPPort()
	if err != nil {
		s.recordRequest(err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	session, err := s.startAttached(r.Context(), req.DeviceID, fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		s.recordRequest(err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	entry := &workerControlSession{
		id:       s.newSessionID(),
		deviceID: req.DeviceID,
		session:  session,
	}
	s.mu.Lock()
	s.sessions[entry.id] = entry
	s.mu.Unlock()
	go s.watchSession(entry)
	s.recordRequest(nil)

	writeJSON(w, http.StatusOK, WorkerAttachResponse{
		SessionID: entry.id,
		DeviceID:  entry.deviceID,
		Endpoint:  session.BrowserURL(),
	})
}

func (s *WorkerControlServer) handleSession(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/sessions/")
	if path == "" {
		http.NotFound(w, r)
		return
	}
	if !s.authorized(r) {
		s.recordRequest(errors.New("unauthorized worker control request"))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if strings.HasSuffix(path, "/targets") {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		sessionID := strings.TrimSuffix(path, "/targets")
		sessionID = strings.TrimSuffix(sessionID, "/")
		s.handleCreateTarget(w, r, sessionID)
		return
	}
	if strings.HasSuffix(path, "/recording/start") {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		sessionID := strings.TrimSuffix(path, "/recording/start")
		sessionID = strings.TrimSuffix(sessionID, "/")
		s.handleStartRecording(w, r, sessionID)
		return
	}
	if strings.HasSuffix(path, "/recording/stop") {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		sessionID := strings.TrimSuffix(path, "/recording/stop")
		sessionID = strings.TrimSuffix(sessionID, "/")
		s.handleStopRecording(w, r, sessionID)
		return
	}
	if strings.Contains(path, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.handleDeleteSession(w, r, path)
}

func (s *WorkerControlServer) handleDeleteSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	entry := s.popSession(sessionID)
	if entry == nil {
		s.recordRequest(errors.New("session not found"))
		http.NotFound(w, r)
		return
	}
	if err := entry.session.Close(); err != nil {
		s.recordRequest(err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_ = s.cleanupSessionRecordings(sessionID)
	s.recordRequest(nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
}

func (s *WorkerControlServer) handleCreateTarget(w http.ResponseWriter, r *http.Request, sessionID string) {
	entry := s.getSession(sessionID)
	if entry == nil {
		s.recordRequest(errors.New("session not found"))
		http.NotFound(w, r)
		return
	}
	var req WorkerCreateTargetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.recordRequest(err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		s.recordRequest(errors.New("url is required"))
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is required"})
		return
	}

	target, err := createTargetViaBrowserURL(r.Context(), entry.session.BrowserURL(), req.URL)
	if err != nil {
		s.recordRequest(err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.recordRequest(nil)
	writeJSON(w, http.StatusOK, target)
}

func (s *WorkerControlServer) handleStartRecording(w http.ResponseWriter, r *http.Request, sessionID string) {
	entry := s.getSession(sessionID)
	if entry == nil {
		s.recordRequest(errors.New("session not found"))
		http.NotFound(w, r)
		return
	}
	if entry.recordingID != "" {
		recording := s.getRecording(entry.recordingID)
		if recording != nil {
			s.recordRequest(nil)
			writeJSON(w, http.StatusOK, WorkerRecordingResponse{
				RecordingID: recording.id,
				FileName:    filepath.Base(recording.path),
				ContentType: "video/mp4",
			})
			return
		}
		s.mu.Lock()
		entry.recordingID = ""
		s.mu.Unlock()
	}
	file, err := os.CreateTemp("", "mobilebridge-worker-recording-*.mp4")
	if err != nil {
		s.recordRequest(err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	outputPath := file.Name()
	_ = file.Close()
	_ = os.Remove(outputPath)
	if err := entry.session.StartRecording(r.Context(), outputPath); err != nil {
		s.recordRequest(err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	recording := &workerControlRecording{
		id:        "mbr_" + randomSuffix(4),
		sessionID: sessionID,
		path:      outputPath,
	}
	s.mu.Lock()
	entry.recordingID = recording.id
	s.recordings[recording.id] = recording
	s.mu.Unlock()
	s.recordRequest(nil)
	writeJSON(w, http.StatusOK, WorkerRecordingResponse{
		RecordingID: recording.id,
		FileName:    filepath.Base(outputPath),
		ContentType: "video/mp4",
	})
}

func (s *WorkerControlServer) handleStopRecording(w http.ResponseWriter, r *http.Request, sessionID string) {
	entry := s.getSession(sessionID)
	if entry == nil {
		s.recordRequest(errors.New("session not found"))
		http.NotFound(w, r)
		return
	}
	recording := s.getRecording(entry.recordingID)
	if recording == nil {
		s.recordRequest(errors.New("recording not found"))
		http.NotFound(w, r)
		return
	}
	if err := entry.session.StopRecording(r.Context()); err != nil {
		s.recordRequest(err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	info, err := os.Stat(recording.path)
	if err != nil {
		s.recordRequest(err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	if current := s.sessions[sessionID]; current == entry {
		current.recordingID = ""
	}
	s.mu.Unlock()
	s.recordRequest(nil)
	writeJSON(w, http.StatusOK, WorkerRecordingResponse{
		RecordingID: recording.id,
		FileName:    filepath.Base(recording.path),
		ContentType: "video/mp4",
		SizeBytes:   info.Size(),
	})
}

func (s *WorkerControlServer) handleRecording(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/recordings/")
	if !s.authorized(r) {
		s.recordRequest(errors.New("unauthorized worker control request"))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method == http.MethodDelete {
		recordingID := strings.TrimSuffix(path, "/")
		if recordingID == "" || strings.Contains(recordingID, "/") {
			http.NotFound(w, r)
			return
		}
		if s.getRecording(recordingID) == nil {
			s.recordRequest(errors.New("recording not found"))
			http.NotFound(w, r)
			return
		}
		if err := s.cleanupRecording(recordingID); err != nil {
			s.recordRequest(err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.recordRequest(nil)
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}
	if !strings.HasSuffix(path, "/content") {
		http.NotFound(w, r)
		return
	}
	recordingID := strings.TrimSuffix(path, "/content")
	recordingID = strings.TrimSuffix(recordingID, "/")
	recording := s.getRecording(recordingID)
	if recording == nil {
		s.recordRequest(errors.New("recording not found"))
		http.NotFound(w, r)
		return
	}
	s.recordRequest(nil)
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeFile(w, r, recording.path)
}

func (s *WorkerControlServer) watchSession(entry *workerControlSession) {
	<-entry.session.Done()
	s.mu.Lock()
	current := s.sessions[entry.id]
	if current == entry {
		delete(s.sessions, entry.id)
	}
	s.mu.Unlock()
}

func (s *WorkerControlServer) getSession(id string) *workerControlSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *WorkerControlServer) popSession(id string) *workerControlSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.sessions[id]
	delete(s.sessions, id)
	return entry
}

func (s *WorkerControlServer) getRecording(id string) *workerControlRecording {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordings[id]
}

func (s *WorkerControlServer) cleanupRecording(id string) error {
	s.mu.Lock()
	recording := s.recordings[id]
	delete(s.recordings, id)
	if recording != nil {
		if session := s.sessions[recording.sessionID]; session != nil && session.recordingID == id {
			session.recordingID = ""
		}
	}
	s.mu.Unlock()
	if recording == nil {
		return nil
	}
	if removeErr := os.Remove(recording.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return nil
}

func (s *WorkerControlServer) cleanupSessionRecordings(sessionID string) error {
	s.mu.Lock()
	ids := make([]string, 0, len(s.recordings))
	for id, recording := range s.recordings {
		if recording.sessionID == sessionID {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()

	var err error
	for _, id := range ids {
		if cleanupErr := s.cleanupRecording(id); cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}
	return err
}

func (s *WorkerControlServer) Snapshot(ctx context.Context, workerID, hostname, advertiseAddr string) WorkerHeartbeat {
	if ctx == nil {
		ctx = context.Background()
	}
	devices, err := s.collectHeartbeatDevices(ctx)

	s.mu.Lock()
	activeSessions := len(s.sessions)
	queueDepth := s.pending
	maxSessions := s.maxSessions
	lastError := s.lastError
	requests := s.requests
	failures := s.failures
	s.mu.Unlock()

	if err != nil && lastError == "" {
		lastError = err.Error()
	}
	failureRate := 0.0
	if requests > 0 {
		failureRate = float64(failures) / float64(requests)
	}
	return WorkerHeartbeat{
		WorkerID:       workerID,
		Hostname:       hostname,
		AdvertiseAddr:  advertiseAddr,
		Healthy:        err == nil,
		ActiveSessions: activeSessions,
		QueueDepth:     queueDepth,
		MaxSessions:    maxSessions,
		FailureRate:    failureRate,
		LastError:      lastError,
		Devices:        devices,
	}
}

func (s *WorkerControlServer) collectHeartbeatDevices(ctx context.Context) ([]WorkerHeartbeatDevice, error) {
	if s == nil || s.listDevices == nil {
		return nil, nil
	}
	devices, err := s.listDevices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]WorkerHeartbeatDevice, 0, len(devices))
	for _, device := range devices {
		if s.enrichDevice != nil {
			_ = s.enrichDevice(ctx, &device)
		}
		inspectable := false
		if s.socketInfo != nil {
			if _, socketErr := s.socketInfo(ctx, device.Serial); socketErr == nil {
				inspectable = true
			}
		}
		out = append(out, WorkerHeartbeatDevice{
			Platform:     "android",
			DeviceID:     device.Serial,
			State:        device.State,
			Name:         firstNonEmpty(device.Model, device.Serial),
			Model:        device.Model,
			Product:      device.Product,
			OSVersion:    device.AndroidVersion,
			Inspectable:  inspectable,
			Capabilities: heartbeatCapabilities(inspectable),
		})
	}
	return out, nil
}

func heartbeatCapabilities(inspectable bool) []string {
	if !inspectable {
		return nil
	}
	return []string{"targets", "create_target", "cdp", "screen_recording"}
}

func (s *WorkerControlServer) reserveAttachSlot() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxSessions > 0 && len(s.sessions)+s.pending >= s.maxSessions {
		return false
	}
	s.pending++
	return true
}

func (s *WorkerControlServer) releaseAttachSlot() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending > 0 {
		s.pending--
	}
}

func (s *WorkerControlServer) recordRequest(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	if err != nil {
		s.failures++
		s.lastError = err.Error()
	}
}

func (s *WorkerControlServer) authorized(r *http.Request) bool {
	s.mu.Lock()
	token := s.controlToken
	s.mu.Unlock()
	if token == "" {
		return true
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func createTargetViaBrowserURL(ctx context.Context, browserURL, targetURL string) (*WorkerCreateTargetResponse, error) {
	if strings.TrimSpace(browserURL) == "" {
		return nil, errors.New("mobilebridge: browser url is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	u := browserURL + "/json/new?url=" + url.QueryEscape(targetURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mobilebridge: target creation failed: status %d", resp.StatusCode)
	}
	var target WorkerCreateTargetResponse
	if err := json.NewDecoder(resp.Body).Decode(&target); err != nil {
		return nil, err
	}
	return &target, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func randomSuffix(n int) string {
	if n <= 0 {
		n = 4
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(buf)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
