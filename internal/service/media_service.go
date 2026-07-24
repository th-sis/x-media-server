package service

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/th-sis/x-media-server/gen/go/xmedia/v1"
)

// ═══════════════════════════════════════════════════════════════════════════
// MediaService — Playlist / Favorite / Subscription CRUD (P0-2)
// ═══════════════════════════════════════════════════════════════════════════
//
// 13 个 gRPC 方法全部接真 SQLite CRUD。前一版是返回空 stub，
// 现按 protobuf v1 契约实现最小可用闭环：
//   - Playlists:    List/Get/Create/Update/Delete + AddMedia/RemoveMedia
//   - Favorites:    List/Add/Remove
//   - Subscriptions: List/Subscribe/Unsubscribe
//
// 所有 user_id 暂用 "admin" (单用户家庭部署，与 AuthService.Login 对齐)。
// 时间戳统一 RFC3339 / SQLite "YYYY-MM-DD HH:MM:SS"，proto 字段用 google.protobuf.Timestamp。
//
// 注：本文件仅实现数据存取层。ContentService / BrowseLibrary / GetMediaDetail
// 等元数据查询仍待 P1 接入 TMDB + OpenList 后再补。

type MediaService struct {
	db *sql.DB
	pb.UnimplementedMediaServiceServer
}

func NewMediaService(db *sql.DB) *MediaService {
	return &MediaService{db: db}
}

const defaultUserID = "admin"

// ── Playlists ──

func (s *MediaService) ListPlaylists(ctx context.Context, req *pb.ListPlaylistsRequest) (*pb.ListPlaylistsResponse, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.name, p.description, p.cover_url,
		        (SELECT COUNT(*) FROM playlist_items WHERE playlist_id = p.id) AS item_count,
		        p.created_at, p.updated_at
		 FROM playlists p WHERE p.user_id = ? ORDER BY p.updated_at DESC`,
		defaultUserID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list playlists: %v", err)
	}
	defer rows.Close()

	var playlists []*pb.Playlist
	for rows.Next() {
		var (
			id, name, desc, cover string
			itemCount             int
			createdAt, updatedAt  string
		)
		if err := rows.Scan(&id, &name, &desc, &cover, &itemCount, &createdAt, &updatedAt); err != nil {
			return nil, status.Errorf(codes.Internal, "scan playlist: %v", err)
		}
		playlists = append(playlists, &pb.Playlist{
			Id:          id,
			Name:        name,
			Description: desc,
			CoverUrl:    cover,
			ItemCount:   int32(itemCount),
			CreatedAt:   parseTS(createdAt),
			UpdatedAt:   parseTS(updatedAt),
		})
	}
	return &pb.ListPlaylistsResponse{
		Playlists: playlists,
		Pagination: &pb.PaginationResponse{
			Total:    int32(len(playlists)),
			Page:     1,
			PageSize: int32(len(playlists)),
			HasNext:  false,
		},
	}, nil
}

func (s *MediaService) GetPlaylist(ctx context.Context, req *pb.GetPlaylistRequest) (*pb.Playlist, error) {
	if req.PlaylistId == "" {
		return nil, status.Error(codes.InvalidArgument, "playlist_id required")
	}
	var (
		id, name, desc, cover, createdAt, updatedAt string
		itemCount                                   int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, description, cover_url,
		        (SELECT COUNT(*) FROM playlist_items WHERE playlist_id = ?) AS item_count,
		        created_at, updated_at
		 FROM playlists WHERE id = ? AND user_id = ?`,
		req.PlaylistId, req.PlaylistId, defaultUserID).
		Scan(&id, &name, &desc, &cover, &itemCount, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "playlist not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get playlist: %v", err)
	}
	return &pb.Playlist{
		Id:          id,
		Name:        name,
		Description: desc,
		CoverUrl:    cover,
		ItemCount:   int32(itemCount),
		CreatedAt:   parseTS(createdAt),
		UpdatedAt:   parseTS(updatedAt),
	}, nil
}

func (s *MediaService) CreatePlaylist(ctx context.Context, req *pb.CreatePlaylistRequest) (*pb.Playlist, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name required")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO playlists (user_id, name, description) VALUES (?, ?, ?)`,
		defaultUserID, req.Name, req.Description)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create playlist: %v", err)
	}
	id, _ := res.LastInsertId()
	log.Info().Int64("playlist_id", id).Str("name", req.Name).Msg("playlist created")
	return s.GetPlaylist(ctx, &pb.GetPlaylistRequest{PlaylistId: intToStr(id)})
}

func (s *MediaService) UpdatePlaylist(ctx context.Context, req *pb.UpdatePlaylistRequest) (*pb.Playlist, error) {
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE playlists SET name = COALESCE(NULLIF(?, ''), name),
		                       description = COALESCE(NULLIF(?, ''), description),
		                       updated_at = datetime('now')
		 WHERE id = ? AND user_id = ?`,
		req.Name, req.Description, req.Id, defaultUserID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "update playlist: %v", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, status.Error(codes.NotFound, "playlist not found or unchanged")
	}
	return s.GetPlaylist(ctx, &pb.GetPlaylistRequest{PlaylistId: req.Id})
}

func (s *MediaService) DeletePlaylist(ctx context.Context, req *pb.DeletePlaylistRequest) (*emptypb.Empty, error) {
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "begin tx: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM playlist_items WHERE playlist_id = ?`, req.Id); err != nil {
		return nil, status.Errorf(codes.Internal, "delete items: %v", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM playlists WHERE id = ? AND user_id = ?`, req.Id, defaultUserID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete playlist: %v", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, status.Error(codes.NotFound, "playlist not found")
	}
	if err := tx.Commit(); err != nil {
		return nil, status.Errorf(codes.Internal, "commit: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *MediaService) AddMediaToPlaylist(ctx context.Context, req *pb.AddMediaToPlaylistRequest) (*pb.Playlist, error) {
	if req.PlaylistId == "" || req.MediaId == "" {
		return nil, status.Error(codes.InvalidArgument, "playlist_id and media_id required")
	}
	var maxPos sql.NullInt64
	_ = s.db.QueryRowContext(ctx,
		`SELECT MAX(position) FROM playlist_items WHERE playlist_id = ?`,
		req.PlaylistId).Scan(&maxPos)
	nextPos := int64(0)
	if maxPos.Valid {
		nextPos = maxPos.Int64 + 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO playlist_items (playlist_id, media_id, position) VALUES (?, ?, ?)`,
		req.PlaylistId, req.MediaId, nextPos)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "add to playlist: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE playlists SET updated_at = datetime('now') WHERE id = ?`, req.PlaylistId); err != nil {
		log.Warn().Err(err).Msg("touch playlist updated_at")
	}
	return s.GetPlaylist(ctx, &pb.GetPlaylistRequest{PlaylistId: req.PlaylistId})
}

func (s *MediaService) RemoveMediaFromPlaylist(ctx context.Context, req *pb.RemoveMediaFromPlaylistRequest) (*pb.Playlist, error) {
	if req.PlaylistId == "" || req.MediaId == "" {
		return nil, status.Error(codes.InvalidArgument, "playlist_id and media_id required")
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM playlist_items WHERE playlist_id = ? AND media_id = ?`,
		req.PlaylistId, req.MediaId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "remove from playlist: %v", err)
	}
	return s.GetPlaylist(ctx, &pb.GetPlaylistRequest{PlaylistId: req.PlaylistId})
}

// ── Favorites ──

func (s *MediaService) ListFavorites(ctx context.Context, req *pb.ListFavoritesRequest) (*pb.ListFavoritesResponse, error) {
	page, pageSize := normalizePagination(req.Pagination)
	offset := (page - 1) * pageSize

	q := `SELECT media_id, media_type, title, added_at FROM favorites WHERE user_id = ?`
	args := []interface{}{defaultUserID}
	if req.MediaType != pb.MediaType_MEDIA_TYPE_UNSPECIFIED {
		q += ` AND media_type = ?`
		args = append(args, int(req.MediaType))
	}
	q += ` ORDER BY added_at DESC LIMIT ? OFFSET ?`
	args = append(args, pageSize, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list favorites: %v", err)
	}
	defer rows.Close()

	var total int32
	_ = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM favorites WHERE user_id = ?`, defaultUserID).Scan(&total)

	var items []*pb.MediaItem
	for rows.Next() {
		var (
			mediaID, title, addedAt string
			mediaType               int
		)
		if err := rows.Scan(&mediaID, &mediaType, &title, &addedAt); err != nil {
			return nil, status.Errorf(codes.Internal, "scan favorite: %v", err)
		}
		items = append(items, &pb.MediaItem{
			Id:        mediaID,
			Title:     title,
			Source:    pb.MediaSource_MEDIA_SOURCE_LOCAL,
			MediaType: pb.MediaType(mediaType),
			AddedAt:   parseTS(addedAt),
		})
	}
	return &pb.ListFavoritesResponse{
		Items: items,
		Pagination: &pb.PaginationResponse{
			Total:    total,
			Page:     page,
			PageSize: pageSize,
			HasNext:  int32(int(offset)+len(items)) < total,
		},
	}, nil
}

func (s *MediaService) AddFavorite(ctx context.Context, req *pb.AddFavoriteRequest) (*emptypb.Empty, error) {
	if req.MediaId == "" {
		return nil, status.Error(codes.InvalidArgument, "media_id required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO favorites (user_id, media_id, media_type, title) VALUES (?, ?, 0, '')`,
		defaultUserID, req.MediaId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "add favorite: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *MediaService) RemoveFavorite(ctx context.Context, req *pb.RemoveFavoriteRequest) (*emptypb.Empty, error) {
	if req.MediaId == "" {
		return nil, status.Error(codes.InvalidArgument, "media_id required")
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM favorites WHERE user_id = ? AND media_id = ?`,
		defaultUserID, req.MediaId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "remove favorite: %v", err)
	}
	return &emptypb.Empty{}, nil
}

// ── Subscriptions ──

func (s *MediaService) ListSubscriptions(ctx context.Context, req *pb.ListSubscriptionsRequest) (*pb.ListSubscriptionsResponse, error) {
	page, pageSize := normalizePagination(req.Pagination)
	offset := (page - 1) * pageSize

	rows, err := s.db.QueryContext(ctx,
		`SELECT media_id, title, enabled, updated_at FROM subscriptions
		 WHERE user_id = ? ORDER BY updated_at DESC LIMIT ? OFFSET ?`,
		defaultUserID, pageSize, offset)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list subscriptions: %v", err)
	}
	defer rows.Close()

	var total int32
	_ = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM subscriptions WHERE user_id = ?`, defaultUserID).Scan(&total)

	var items []*pb.MediaItem
	for rows.Next() {
		var (
			mediaID, title, updatedAt string
			enabled                   int
		)
		if err := rows.Scan(&mediaID, &title, &enabled, &updatedAt); err != nil {
			return nil, status.Errorf(codes.Internal, "scan subscription: %v", err)
		}
		items = append(items, &pb.MediaItem{
			Id:      mediaID,
			Title:   title,
			Source:  pb.MediaSource_MEDIA_SOURCE_LOCAL,
			AddedAt: parseTS(updatedAt),
		})
	}
	return &pb.ListSubscriptionsResponse{
		Items: items,
		Pagination: &pb.PaginationResponse{
			Total:    total,
			Page:     page,
			PageSize: pageSize,
			HasNext:  int32(int(offset)+len(items)) < total,
		},
	}, nil
}

func (s *MediaService) Subscribe(ctx context.Context, req *pb.SubscribeRequest) (*emptypb.Empty, error) {
	if req.MediaId == "" {
		return nil, status.Error(codes.InvalidArgument, "media_id required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO subscriptions (user_id, media_id, title, enabled)
		 VALUES (?, ?, '', 1)
		 ON CONFLICT(user_id, media_id) DO UPDATE SET enabled = 1, updated_at = datetime('now')`,
		defaultUserID, req.MediaId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "subscribe: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *MediaService) Unsubscribe(ctx context.Context, req *pb.UnsubscribeRequest) (*emptypb.Empty, error) {
	if req.MediaId == "" {
		return nil, status.Error(codes.InvalidArgument, "media_id required")
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM subscriptions WHERE user_id = ? AND media_id = ?`,
		defaultUserID, req.MediaId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unsubscribe: %v", err)
	}
	return &emptypb.Empty{}, nil
}

// ── Helpers ──

// parseTS converts SQLite "YYYY-MM-DD HH:MM:SS" or RFC3339 into proto Timestamp.
// Empty string returns nil.
func parseTS(s string) *timestamppb.Timestamp {
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return timestamppb.New(t)
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return timestamppb.New(t)
	}
	return nil
}

func normalizePagination(p *pb.PaginationRequest) (page, pageSize int32) {
	if p == nil {
		return 1, 50
	}
	page = p.Page
	if page < 1 {
		page = 1
	}
	pageSize = p.PageSize
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}
	return
}

func intToStr(i int64) string {
	return strconv.FormatInt(i, 10)
}