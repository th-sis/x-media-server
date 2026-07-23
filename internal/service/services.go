package service

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/th-sis/x-media-server/internal/config"
	"github.com/th-sis/x-media-server/internal/model"
	pb "github.com/th-sis/x-media-server/gen/go/xmedia/v1"
)

// ═══════════════════════════════════════════════════════════
// Session Manager — 全局连接池 + device 追踪
// ═══════════════════════════════════════════════════════════

type SessionManager struct {
	sessions sync.Map // device_id → *ClientSession (sync.Map: 读多写少，Ping高频读取无锁)
}

type ClientSession struct {
	DeviceID string
	UserID   string
	Stream   pb.PlaybackControlService_ControlStreamServer
	ctx      context.Context
	Cancel   context.CancelFunc
	State    *model.PlaybackSession
	// Per-device FIFO signal queue for remote key ordering
	keyQueue chan *pb.ControlRequest
}

var globalSM = &SessionManager{}

// processKeyQueue consumes remote key events in FIFO order per device.
// Single goroutine → guaranteed ordering.
func (cs *ClientSession) processKeyQueue() {
	for req := range cs.keyQueue {
		p := req.Payload.(*pb.ControlRequest_RemoteKey)
		log.Info().Str("device", cs.DeviceID).Str("key", p.RemoteKey.Key.String()).Bool("pressed", p.RemoteKey.Pressed).Msg("Remote key")
		// TODO: map remote key to playback action (volume, seek, back, menu, etc.)
	}
}

func (sm *SessionManager) Register(deviceID string, s *ClientSession) {
	s.keyQueue = make(chan *pb.ControlRequest, 64) // buffered for burst key presses
	go s.processKeyQueue()                          // single goroutine per device = FIFO order guarantee
	sm.sessions.Store(deviceID, s)
	log.Info().Str("device", deviceID).Msg("Session registered")
}

func (sm *SessionManager) Unregister(deviceID string) {
	if v, ok := sm.sessions.LoadAndDelete(deviceID); ok {
		sess := v.(*ClientSession)
		close(sess.keyQueue)
		sess.Cancel()
	}
	log.Info().Str("device", deviceID).Msg("Session unregistered")
}

func (sm *SessionManager) Get(deviceID string) *ClientSession {
	v, ok := sm.sessions.Load(deviceID)
	if !ok {
		return nil
	}
	return v.(*ClientSession)
}

// PushToDevice pushes data to a specific device stream with non-blocking timeout.
// Returns false if the device is offline, context cancelled, or send times out.
func (sm *SessionManager) PushToDevice(deviceID string, resp *pb.ControlResponse) bool {
	sess := sm.Get(deviceID)
	if sess == nil {
		log.Warn().Str("device", deviceID).Msg("Push failed: device offline")
		return false
	}
	select {
	case <-sess.ctx.Done():
		log.Warn().Str("device", deviceID).Msg("Push failed: context cancelled")
		return false
	case <-time.After(500 * time.Millisecond):
		log.Warn().Str("device", deviceID).Msg("Push failed: stream send timeout (dead link)")
		sm.Unregister(deviceID) // force cleanup dead connection
		return false
	case err := <-sess.sendAsync(resp):
		if err != nil {
			log.Error().Err(err).Str("device", deviceID).Msg("Push send error")
			return false
		}
		return true
	}
}

// sendAsync wraps Stream.Send in a goroutine + channel to enable select timeout
func (cs *ClientSession) sendAsync(resp *pb.ControlResponse) <-chan error {
	ch := make(chan error, 1)
	go func() {
		ch <- cs.Stream.Send(resp)
	}()
	return ch
}

// ═══════════════════════════════════════════════════════════
// PlaybackService — ControlStream 双向流（集成 Session Manager）
// ═══════════════════════════════════════════════════════════

type PlaybackService struct {
	pb.UnimplementedPlaybackControlServiceServer
}

func NewPlaybackService() *PlaybackService {
	return &PlaybackService{}
}

func (s *PlaybackService) ControlStream(stream pb.PlaybackControlService_ControlStreamServer) error {
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	// First message must carry device_id
	req, err := stream.Recv()
	if err != nil {
		return status.Error(codes.Internal, "failed to receive initial message")
	}
	ping, ok := req.Payload.(*pb.ControlRequest_Ping)
	if !ok || ping.Ping.DeviceId == "" {
		return status.Error(codes.InvalidArgument, "first message must be Ping with device_id")
	}
	deviceID := ping.Ping.DeviceId

	sess := &ClientSession{
		DeviceID: deviceID,
		Stream:   stream,
		ctx:      ctx,
		Cancel:   cancel,
		State: &model.PlaybackSession{
			DeviceID: deviceID,
			State:    "idle",
		},
	}
	globalSM.Register(deviceID, sess)
	defer globalSM.Unregister(deviceID)

	// Acknowledge registration
	stream.Send(&pb.ControlResponse{
		Payload: &pb.ControlResponse_Pong{
			Pong: &pb.PongPayload{ClientTimestamp: req.SequenceId, ServerTimestamp: time.Now().UnixMilli()},
		},
	})

	// Message loop
	for {
		req, err := stream.Recv()
		if err != nil {
			log.Info().Str("device", deviceID).Msg("Stream closed")
			return nil
		}
		sess.State.LastSeen = time.Now()

		switch p := req.Payload.(type) {
		case *pb.ControlRequest_Play:
			sess.State.MediaID = p.Play.MediaId
			sess.State.State = "playing"
			stream.Send(&pb.ControlResponse{
				Payload: &pb.ControlResponse_StateChanged{
					StateChanged: &pb.StateChangedPayload{State: pb.PlayerState_PLAYER_STATE_PLAYING},
				},
			})

		case *pb.ControlRequest_Pause:
			sess.State.State = "paused"
			stream.Send(&pb.ControlResponse{
				Payload: &pb.ControlResponse_StateChanged{
					StateChanged: &pb.StateChangedPayload{State: pb.PlayerState_PLAYER_STATE_PAUSED},
				},
			})

		case *pb.ControlRequest_Seek:
			sess.State.Position = p.Seek.PositionSecs

		case *pb.ControlRequest_SetVolume:
			sess.State.Volume = p.SetVolume.Volume

		case *pb.ControlRequest_RemoteKey:
			// Route through per-device FIFO queue to guarantee ordering
			select {
			case sess.keyQueue <- req:
			default:
				log.Warn().Str("device", deviceID).Msg("Key queue full, dropping event")
			}

		case *pb.ControlRequest_Ping:
			stream.Send(&pb.ControlResponse{
				Payload: &pb.ControlResponse_Pong{
					Pong: &pb.PongPayload{
						ClientTimestamp: ping.Ping.ClientTimestamp,
						ServerTimestamp: time.Now().UnixMilli(),
					},
				},
			})

		case *pb.ControlRequest_Stop:
			sess.State.State = "idle"
			stream.Send(&pb.ControlResponse{
				Payload: &pb.ControlResponse_StateChanged{
					StateChanged: &pb.StateChangedPayload{State: pb.PlayerState_PLAYER_STATE_IDLE},
				},
			})
		}
	}
}

func (s *PlaybackService) GetPlaybackState(ctx context.Context, req *emptypb.Empty) (*pb.PlaybackStateSnapshot, error) {
	return &pb.PlaybackStateSnapshot{
		State: pb.PlayerState_PLAYER_STATE_IDLE,
		Volume: 1.0,
		Speed:  1.0,
	}, nil
}

// ═══════════════════════════════════════════════════════════
// TransferService — gRPC 转存状态查询（Pull 模式）
// ═══════════════════════════════════════════════════════════

type TransferService struct {
	pb.UnimplementedTransferServiceServer
	store *TransferTaskStore
}

type TransferTaskStore struct {
	mu    sync.RWMutex
	tasks map[string]*model.TransferTask // task_id → task
}

func NewTransferTaskStore() *TransferTaskStore {
	return &TransferTaskStore{tasks: make(map[string]*model.TransferTask)}
}

func NewTransferService(store *TransferTaskStore) *TransferService {
	return &TransferService{store: store}
}

func (s *TransferService) GetTransferStatus(ctx context.Context, req *pb.TransferStatusRequest) (*pb.TransferStatusResponse, error) {
	s.store.mu.RLock()
	task, ok := s.store.tasks[req.TaskId]
	s.store.mu.RUnlock()
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	transStatus := pb.TransferStatus_TRANSFER_STATUS_UNSPECIFIED
	switch task.Status {
	case model.TransferPending:
		transStatus = pb.TransferStatus_TRANSFER_STATUS_PENDING
	case model.TransferDownloading:
		transStatus = pb.TransferStatus_TRANSFER_STATUS_DOWNLOADING
	case model.TransferCompleted:
		transStatus = pb.TransferStatus_TRANSFER_STATUS_COMPLETED
	case model.TransferFailed:
		transStatus = pb.TransferStatus_TRANSFER_STATUS_FAILED
	}
	return &pb.TransferStatusResponse{
		Status:       transStatus,
		Progress:     task.Progress,
		PlayableUrl:  task.ResultURL,
		ErrorMessage: task.Error,
	}, nil
}

func (s *TransferService) CancelTransfer(ctx context.Context, req *pb.TransferStatusRequest) (*pb.TransferStatusResponse, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	task, ok := s.store.tasks[req.TaskId]
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	task.Status = model.TransferFailed
	task.Error = "cancelled by user"
	return &pb.TransferStatusResponse{
		Status:       pb.TransferStatus_TRANSFER_STATUS_FAILED,
		ErrorMessage: "cancelled",
	}, nil
}

// StartTransfer runs async transfer and handles PUSH notification via SessionManager
func (s *TransferService) StartTransfer(mediaID, sourceURL, sourcePan, deviceID string, imgCache *model.ImageCache) *model.TransferTask {
	task := model.NewTransferTask(mediaID, sourceURL, sourcePan)
	task.Status = model.TransferDownloading

	s.store.mu.Lock()
	s.store.tasks[task.ID] = task
	s.store.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Interface("panic", r).Str("media", mediaID).Msg("Transfer panic")
				task.Status = model.TransferFailed
				task.Error = "internal error"
			}
		}()

		log.Info().Str("media", mediaID).Str("source", sourcePan).Str("device", deviceID).Msg("Transfer started")

		// Phase 1: Submit to main pan (115)
		// TODO: actual 115 API call
		time.Sleep(500 * time.Millisecond)

		// Phase 2: Poll progress
		for i := 0; i < 30; i++ {
			time.Sleep(500 * time.Millisecond)
			task.Progress = int32(i * 3)
			if task.Progress > 100 {
				task.Progress = 100
			}
		}

		task.Status = model.TransferCompleted
		task.Progress = 100
		task.ResultURL = "https://115-direct/" + mediaID

		log.Info().Str("media", mediaID).Str("url", task.ResultURL).Msg("Transfer completed")

		// Push to device via SessionManager (safe — checks ctx + online status)
		_ = imgCache // unused in transfer, but keeps signature consistent
		ok := globalSM.PushToDevice(deviceID, &pb.ControlResponse{
			Payload: &pb.ControlResponse_MediaChanged{
				MediaChanged: &pb.MediaChangedPayload{},
			},
		})
		if !ok {
			// Device offline — status persisted in store, client will Pull via GetTransferStatus
			log.Info().Str("media", mediaID).Msg("Device offline, transfer result persisted for Pull sync")
		}
	}()

	return task
}

// ═══════════════════════════════════════════════════════════
// AuthService
// ═══════════════════════════════════════════════════════════

type AuthService struct {
	cfg *config.Config
	db  *sql.DB
	pb.UnimplementedAuthServiceServer
}

func NewAuthService(cfg *config.Config, db *sql.DB) *AuthService {
	return &AuthService{cfg: cfg, db: db}
}

func (s *AuthService) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	if req.Username != s.cfg.Auth.AdminUsername || req.Password != s.cfg.Auth.AdminPassword {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	access, refresh, _ := generateTokens(req.Username, s.cfg)
	db := s.db
	if db != nil {
		db.Exec(`INSERT INTO tokens (user_id, access_token, refresh_token, expires_at) VALUES (?, ?, ?, datetime('now','+'||?||' seconds'))`,
			req.Username, access, refresh, s.cfg.Auth.TokenTTL)
	}
	return &pb.LoginResponse{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int64(s.cfg.Auth.TokenTTL),
		User:         &pb.User{Id: "1", Username: req.Username, DisplayName: req.Username},
	}, nil
}

func (s *AuthService) RefreshToken(ctx context.Context, req *pb.RefreshTokenRequest) (*pb.RefreshTokenResponse, error) {
	access, refresh, _ := generateTokens("admin", s.cfg)
	return &pb.RefreshTokenResponse{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int64(s.cfg.Auth.TokenTTL),
	}, nil
}

func (s *AuthService) Logout(ctx context.Context, req *pb.LogoutRequest) (*pb.LogoutResponse, error) {
	return &pb.LogoutResponse{}, nil
}

func generateTokens(username string, cfg *config.Config) (string, string, error) {
	access, _ := generateJWT(username, cfg.Auth.JWTSecret, time.Duration(cfg.Auth.TokenTTL)*time.Second)
	refresh, _ := generateJWT(username, cfg.Auth.JWTSecret, time.Duration(cfg.Auth.RefreshTokenTTL)*time.Second)
	return access, refresh, nil
}

// ═══════════════════════════════════════════════════════════
// ContentService
// ═══════════════════════════════════════════════════════════

type ContentService struct {
	cfg *config.Config
	pb.UnimplementedContentServiceServer
}

func (s *ContentService) BrowseLibrary(ctx context.Context, req *pb.BrowseLibraryRequest) (*pb.BrowseLibraryResponse, error) {
	return &pb.BrowseLibraryResponse{}, nil
}
func (s *ContentService) GetMediaDetail(ctx context.Context, req *pb.GetMediaDetailRequest) (*pb.MediaDetail, error) {
	return &pb.MediaDetail{}, nil
}
func (s *ContentService) Search(ctx context.Context, req *pb.SearchRequest) (*pb.SearchResponse, error) {
	return &pb.SearchResponse{}, nil
}
func (s *ContentService) GetPlayUrl(ctx context.Context, req *pb.GetPlayUrlRequest) (*pb.GetPlayUrlResponse, error) {
	return &pb.GetPlayUrlResponse{Url: "", StreamType: "direct"}, nil
}
func (s *ContentService) GetExploreFeed(ctx context.Context, req *pb.GetExploreFeedRequest) (*pb.GetExploreFeedResponse, error) {
	return &pb.GetExploreFeedResponse{}, nil
}

// ═══════════════════════════════════════════════════════════
// MediaService + HealthService (stubs)
// ═══════════════════════════════════════════════════════════

type MediaService struct {
	db *sql.DB
	pb.UnimplementedMediaServiceServer
}

func (s *MediaService) ListPlaylists(ctx context.Context, req *pb.ListPlaylistsRequest) (*pb.ListPlaylistsResponse, error) {
	return &pb.ListPlaylistsResponse{}, nil
}
func (s *MediaService) GetPlaylist(ctx context.Context, req *pb.GetPlaylistRequest) (*pb.Playlist, error) {
	return &pb.Playlist{}, nil
}
func (s *MediaService) CreatePlaylist(ctx context.Context, req *pb.CreatePlaylistRequest) (*pb.Playlist, error) {
	return &pb.Playlist{}, nil
}
func (s *MediaService) UpdatePlaylist(ctx context.Context, req *pb.UpdatePlaylistRequest) (*pb.Playlist, error) {
	return &pb.Playlist{}, nil
}
func (s *MediaService) DeletePlaylist(ctx context.Context, req *pb.DeletePlaylistRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
func (s *MediaService) AddMediaToPlaylist(ctx context.Context, req *pb.AddMediaToPlaylistRequest) (*pb.Playlist, error) {
	return &pb.Playlist{}, nil
}
func (s *MediaService) RemoveMediaFromPlaylist(ctx context.Context, req *pb.RemoveMediaFromPlaylistRequest) (*pb.Playlist, error) {
	return &pb.Playlist{}, nil
}
func (s *MediaService) ListFavorites(ctx context.Context, req *pb.ListFavoritesRequest) (*pb.ListFavoritesResponse, error) {
	return &pb.ListFavoritesResponse{}, nil
}
func (s *MediaService) AddFavorite(ctx context.Context, req *pb.AddFavoriteRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
func (s *MediaService) RemoveFavorite(ctx context.Context, req *pb.RemoveFavoriteRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
func (s *MediaService) ListSubscriptions(ctx context.Context, req *pb.ListSubscriptionsRequest) (*pb.ListSubscriptionsResponse, error) {
	return &pb.ListSubscriptionsResponse{}, nil
}
func (s *MediaService) Subscribe(ctx context.Context, req *pb.SubscribeRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
func (s *MediaService) Unsubscribe(ctx context.Context, req *pb.UnsubscribeRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

type HealthService struct {
	startTime time.Time
	pb.UnimplementedHealthServiceServer
}

func NewHealthService() *HealthService {
	return &HealthService{startTime: time.Now()}
}

func (s *HealthService) Check(ctx context.Context, req *emptypb.Empty) (*pb.HealthCheckResponse, error) {
	return &pb.HealthCheckResponse{
		Status:     "SERVING",
		Version:    "0.1.0",
		UptimeSecs: int64(time.Since(s.startTime).Seconds()),
	}, nil
}

// ═══════════════════════════════════════════════════════════
// Image Proxy
// ═══════════════════════════════════════════════════════════

type ImageProxy struct {
	cfg    *config.Config
	cache  *model.ImageCache
	client *http.Client
}

func NewImageProxy(cfg *config.Config, cache *model.ImageCache) *ImageProxy {
	return &ImageProxy{cfg: cfg, cache: cache, client: &http.Client{Timeout: 10 * time.Second}}
}

func (p *ImageProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "missing path", 400)
		return
	}
	if data, ok := p.cache.Get(path); ok {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public,max-age=86400")
		w.Write(data)
		return
	}
	url := p.cfg.TMDB.ImageBaseURL + "/w500" + path
	resp, err := p.client.Get(url)
	if err != nil {
		http.Error(w, "fetch failed", 502)
		return
	}
	defer resp.Body.Close()
	if resp.ContentLength > 0 {
		buf := make([]byte, resp.ContentLength)
		resp.Body.Read(buf)
		p.cache.Set(path, buf)
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public,max-age=86400")
		w.Write(buf)
	}
}

// ═══════════════════════════════════════════════════════════
// JWT helpers
// ═══════════════════════════════════════════════════════════

func generateJWT(username, secret string, ttl time.Duration) (string, error) {
	claims := jwt.MapClaims{"sub": username, "iat": time.Now().Unix(), "exp": time.Now().Add(ttl).Unix()}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}
