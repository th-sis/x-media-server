package service

import (
	"context"
	"database/sql"
	"net/http"
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

// ── AllServices ──

type AllServices struct {
	Auth      *AuthService
	Media     *MediaService
	Playback  *PlaybackService
	Content   *ContentService
	Health    *HealthService
	Transfer  *TransferService
	Scrape    *ScrapeHandler
	Pan       *PanHandler
	PanSearch *PanSearchHandler
	Strm      *StrmHandler
	Task      *TaskHandler
}

func NewAll(cfg *config.Config, db *sql.DB, state *model.StateStore, img *model.ImageCache) *AllServices {
	return &AllServices{
		Auth:      NewAuthService(cfg, db),
		Media:     &MediaService{db: db},
		Playback:  NewPlaybackService(state),
		Content:   &ContentService{cfg: cfg},
		Health:    NewHealthService(),
		Transfer:  NewTransferService(cfg, state),
		Scrape:    &ScrapeHandler{img: img},
		Pan:       NewPanHandler(cfg, db),
		PanSearch: NewPanSearchHandler(),
		Strm:      &StrmHandler{db: db},
		Task:      &TaskHandler{},
	}
}

// ── AuthService (gRPC implementation) ──

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
	access, refresh, err := s.generateTokens(req.Username)
	if err != nil {
		return nil, status.Error(codes.Internal, "token generation failed")
	}
	tokenTTL := int64(s.cfg.Auth.TokenTTL)
	s.db.Exec(`INSERT INTO tokens (user_id, access_token, refresh_token, expires_at) VALUES (?, ?, ?, datetime('now','+'||?||' seconds'))`,
		req.Username, access, refresh, tokenTTL)

	return &pb.LoginResponse{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    tokenTTL,
		User: &pb.User{Id: "1", Username: req.Username, DisplayName: req.Username},
	}, nil
}

func (s *AuthService) RefreshToken(ctx context.Context, req *pb.RefreshTokenRequest) (*pb.RefreshTokenResponse, error) {
	var userID string
	err := s.db.QueryRow(`SELECT user_id FROM tokens WHERE refresh_token = ?`, req.RefreshToken).Scan(&userID)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid refresh token")
	}
	access, refresh, _ := s.generateTokens(userID)
	return &pb.RefreshTokenResponse{AccessToken: access, RefreshToken: refresh, ExpiresIn: int64(s.cfg.Auth.TokenTTL)}, nil
}

func (s *AuthService) Logout(ctx context.Context, req *pb.LogoutRequest) (*pb.LogoutResponse, error) {
	return &pb.LogoutResponse{}, nil
}

func (s *AuthService) generateTokens(username string) (string, string, error) {
	access, _ := generateJWT(username, s.cfg.Auth.JWTSecret, time.Duration(s.cfg.Auth.TokenTTL)*time.Second)
	refresh, _ := generateJWT(username, s.cfg.Auth.JWTSecret, time.Duration(s.cfg.Auth.RefreshTokenTTL)*time.Second)
	return access, refresh, nil
}

func (s *AuthService) HTTPLogin(username, password string) (string, string, int64, error) {
	resp, err := s.Login(context.Background(), &pb.LoginRequest{Username: username, Password: password})
	if err != nil {
		return "", "", 0, err
	}
	return resp.AccessToken, resp.RefreshToken, resp.ExpiresIn, nil
}

// ── MediaService (gRPC stub implementation) ──

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

// ── PlaybackService (gRPC bidirectional stream) ──

type PlaybackService struct {
	state *model.StateStore
	pb.UnimplementedPlaybackControlServiceServer
}

func NewPlaybackService(state *model.StateStore) *PlaybackService {
	return &PlaybackService{state: state}
}

func (s *PlaybackService) ControlStream(stream pb.PlaybackControlService_ControlStreamServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			return err
		}
		resp := &pb.ControlResponse{RequestSequenceId: req.SequenceId}
		switch p := req.Payload.(type) {
		case *pb.ControlRequest_Play:
			log.Info().Str("media", p.Play.MediaId).Msg("Play command")
			s.state.Set(&model.PlaybackSession{MediaID: p.Play.MediaId, State: "playing"})
			resp.Payload = &pb.ControlResponse_StateChanged{StateChanged: &pb.StateChangedPayload{State: pb.PlayerState_PLAYER_STATE_PLAYING}}
		case *pb.ControlRequest_Pause:
			resp.Payload = &pb.ControlResponse_StateChanged{StateChanged: &pb.StateChangedPayload{State: pb.PlayerState_PLAYER_STATE_PAUSED}}
		case *pb.ControlRequest_Ping:
			resp.Payload = &pb.ControlResponse_Pong{Pong: &pb.PongPayload{TimestampMs: p.Ping.TimestampMs}}
		default:
			continue
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (s *PlaybackService) GetPlaybackState(ctx context.Context, req *emptypb.Empty) (*pb.PlaybackStateSnapshot, error) {
	return &pb.PlaybackStateSnapshot{State: pb.PlayerState_PLAYER_STATE_IDLE}, nil
}

// ── ContentService ──

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

// ── HealthService ──

type HealthService struct {
	startTime time.Time
	pb.UnimplementedHealthServiceServer
}

func NewHealthService() *HealthService {
	return &HealthService{startTime: time.Now()}
}

func (s *HealthService) Uptime() time.Duration {
	return time.Since(s.startTime)
}

func (s *HealthService) Check(ctx context.Context, req *emptypb.Empty) (*pb.HealthCheckResponse, error) {
	return &pb.HealthCheckResponse{
		Status:     "SERVING",
		Version:    "0.1.0",
		UptimeSecs: int64(s.Uptime().Seconds()),
	}, nil
}

// ── TransferService ──

type TransferService struct {
	cfg   *config.Config
	state *model.StateStore
}

func NewTransferService(cfg *config.Config, state *model.StateStore) *TransferService {
	return &TransferService{cfg: cfg, state: state}
}

func (t *TransferService) StartTransfer(mediaID, sourceURL, sourcePan string) *model.TransferTask {
	task := model.NewTransferTask(mediaID, sourceURL, sourcePan)
	task.Status = model.TransferDownloading

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Interface("panic", r).Str("media", mediaID).Msg("Transfer panic")
				task.Status = model.TransferFailed
			}
		}()
		log.Info().Str("media", mediaID).Str("source", sourcePan).Msg("Transfer started")
		time.Sleep(500 * time.Millisecond) // TODO: actual 115 transfer
		task.Status = model.TransferCompleted
		task.Progress = 100
		task.ResultURL = "https://115/" + mediaID
		log.Info().Str("media", mediaID).Msg("Transfer completed")
	}()
	return task
}

// ── Image Proxy ──

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
	buf := make([]byte, resp.ContentLength)
	resp.Body.Read(buf)
	p.cache.Set(path, buf)
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public,max-age=86400")
	w.Write(buf)
}

// ── JWT helpers ──

func generateJWT(username, secret string, ttl time.Duration) (string, error) {
	claims := jwt.MapClaims{"sub": username, "iat": time.Now().Unix(), "exp": time.Now().Add(ttl).Unix()}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}
