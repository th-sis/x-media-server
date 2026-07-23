package main

import (
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/th-sis/x-media-server/internal/config"
	"github.com/th-sis/x-media-server/internal/database"
	"github.com/th-sis/x-media-server/internal/model"
	"github.com/th-sis/x-media-server/internal/service"
	pb "github.com/th-sis/x-media-server/gen/go/xmedia/v1"
)

//go:embed admin.html
var adminHTML []byte

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: "15:04:05"})

	cfg := config.Load()
	level, _ := zerolog.ParseLevel(cfg.Log.Level)
	zerolog.SetGlobalLevel(level)

	log.Info().Str("version", "0.1.0").Msg("X-Media Server booting...")

	// ── Database ──
	db, err := database.Get(&cfg.Database)
	if err != nil {
		log.Fatal().Err(err).Msg("Database init failed")
	}
	defer db.Close()

	// ── Isolated model stores ──
	stateStore := model.NewStateStore()
	imgCache := model.NewImageCache(800)

	// ── Services ──
	authSvc := service.NewAuthService(cfg, db)
	mediaSvc := &service.MediaService{}
	playbackSvc := service.NewPlaybackService(stateStore)
	contentSvc := &service.ContentService{}
	healthSvc := service.NewHealthService()
	transferSvc := service.NewTransferService(cfg, stateStore)

	// ── gRPC Server ──
	authInterceptor := service.AuthInterceptor(cfg)
	streamAuthInterceptor := service.StreamAuthInterceptor(cfg)

	grpcSrv := grpc.NewServer(
		grpc.UnaryInterceptor(authInterceptor),
		grpc.StreamInterceptor(streamAuthInterceptor),
	)

	// Register gRPC service implementations
	pb.RegisterAuthServiceServer(grpcSrv, authSvc)
	pb.RegisterMediaServiceServer(grpcSrv, mediaSvc)
	pb.RegisterPlaybackControlServiceServer(grpcSrv, playbackSvc)
	pb.RegisterContentServiceServer(grpcSrv, contentSvc)
	pb.RegisterHealthServiceServer(grpcSrv, healthSvc)

	_ = transferSvc // used via HTTP API

	reflection.Register(grpcSrv)

	grpcLis, err := net.Listen("tcp", cfg.Server.GRPCPort)
	if err != nil {
		log.Fatal().Err(err).Msg("gRPC listen failed")
	}
	go func() {
		log.Info().Str("addr", cfg.Server.GRPCPort).Msg("gRPC listening")
		if err := grpcSrv.Serve(grpcLis); err != nil {
			log.Fatal().Err(err).Msg("gRPC serve error")
		}
	}()

	// ── HTTP Server ──
	router := mux.NewRouter()
	api := router.PathPrefix("/api").Subrouter()
	service.RegisterAll(api, cfg, db, stateStore, imgCache)

	router.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(); err != nil {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}).Methods("GET")

	router.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(adminHTML)
	})
	router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/config", 302)
	})

	imgSvc := service.NewImageProxy(cfg, imgCache)
	router.HandleFunc("/img", imgSvc.ServeHTTP).Methods("GET")

	httpSrv := &http.Server{Addr: cfg.Server.HTTPPort, Handler: router}
	go func() {
		log.Info().Str("addr", cfg.Server.HTTPPort).Msg("HTTP Admin listening")
		fmt.Printf("\n  ✅ X-Media Server Ready\n")
		fmt.Printf("  Admin: %s/config\n", cfg.Server.ExternalURL)
		fmt.Printf("  Health:%s/healthz\n\n", cfg.Server.ExternalURL)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("HTTP serve error")
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info().Msg("Shutting down...")
	grpcSrv.GracefulStop()
	httpSrv.Close()
}
