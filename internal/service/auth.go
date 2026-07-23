package service

import (
	"context"

	jwt "github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/th-sis/x-media-server/internal/config"
)

// AuthInterceptor validates JWT for gRPC unary calls
func AuthInterceptor(cfg *config.Config) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if info.FullMethod == "/xmedia.v1.AuthService/Login" || info.FullMethod == "/xmedia.v1.HealthService/Check" {
			return handler(ctx, req)
		}
		_, err := authenticate(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamAuthInterceptor validates JWT for gRPC streams
func StreamAuthInterceptor(cfg *config.Config) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if info.FullMethod == "/xmedia.v1.PlaybackControlService/ControlStream" {
			return handler(srv, ss) // Allow ControlStream — auth via device registration
		}
		_, err := authenticate(ss.Context(), cfg)
		if err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func authenticate(ctx context.Context, cfg *config.Config) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing metadata")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return "", status.Error(codes.Unauthenticated, "missing authorization")
	}
	tokenStr := vals[0]
	if len(tokenStr) > 7 && tokenStr[:7] == "Bearer " {
		tokenStr = tokenStr[7:]
	}
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		return []byte(cfg.Auth.JWTSecret), nil
	})
	if err != nil || !token.Valid {
		return "", status.Error(codes.Unauthenticated, "invalid token")
	}
	claims, _ := token.Claims.(jwt.MapClaims)
	sub, _ := claims["sub"].(string)
	return sub, nil
}

// AuthError for HTTP layer
type AuthError struct{ Message string }

func (e *AuthError) Error() string { return e.Message }
