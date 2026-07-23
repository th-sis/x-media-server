// X-Media gRPC Mock Client — 全链路黑盒压测工具
//
// 用法:
//   cd x-media-server
//   go run test/mock/client_sim.go --host=192.168.7.154 --port=50051 --mode=full
//
// 模式:
//   --mode=ping     仅心跳测试
//   --mode=play     播放 → 监听转存
//   --mode=chaos    混沌测试（断网→重连）
//   --mode=full     完整流程

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/th-sis/x-media-server/gen/go/xmedia/v1"
)

var (
	host      = flag.String("host", "192.168.7.154", "gRPC server host")
	port      = flag.Int("port", 50051, "gRPC server port")
	mode      = flag.String("mode", "full", "Test mode: ping|play|chaos|full")
	deviceID = flag.String("device", "mock-win-001", "Device ID for session tracking")
)

func main() {
	flag.Parse()

	addr := fmt.Sprintf("%s:%d", *host, *port)
	log("══════════════════════════════════════════")
	log("  X-Media gRPC Mock Client")
	log("  Target: %s  |  Device: %s  |  Mode: %s", addr, *deviceID, *mode)
	log("══════════════════════════════════════════")

	conn, err := grpc.Dial(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithTimeout(10*time.Second),
	)
	if err != nil {
		fatal("Failed to connect: %v", err)
	}
	defer conn.Close()
	log("✅ gRPC connected")

	// ── Login ──
	authClient := pb.NewAuthServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := authClient.Login(ctx, &pb.LoginRequest{
		Username: "admin",
		Password: "admin",
	})
	if err != nil {
		fatal("Login failed: %v", err)
	}
	token := resp.AccessToken
	log("✅ Login OK — token: %s...", token[:20])

	// ── Health Check ──
	healthClient := pb.NewHealthServiceClient(conn)
	hresp, err := healthClient.Check(context.Background(), &emptypb.Empty{})
	if err != nil {
		log("⚠ Health check failed: %v", err)
	} else {
		log("✅ Health: %s (uptime %ds) v%s", hresp.Status, hresp.UptimeSecs, hresp.Version)
	}

	switch *mode {
	case "ping":
		runPingTest(conn, token)
	case "play":
		runPlayTest(conn, token)
	case "chaos":
		runChaosTest(conn, token)
	case "full":
		runFullTest(conn, token)
	}
}

func runPingTest(conn *grpc.ClientConn, token string) {
	log("\n📡 心跳测试 — 每秒 Ping，Ctrl+C 退出")
	stream := openControlStream(conn, token)
	defer stream.CloseSend()

	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				return
			}
			handleResponse(resp)
		}
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	seq := int64(1)
	for {
		select {
		case <-sigCh:
			log("\n👋 退出")
			return
		case <-ticker.C:
			seq++
			stream.Send(&pb.ControlRequest{
				SequenceId: seq,
				Payload: &pb.ControlRequest_Ping{
					Ping: &pb.PingPayload{
						ClientTimestamp: time.Now().UnixMilli(),
						DeviceId:        *deviceID,
					},
				},
			})
		}
	}
}

func runPlayTest(conn *grpc.ClientConn, token string) {
	log("\n🎬 播放测试 — 发送 PlayPayload → 监听转存")
	stream := openControlStream(conn, token)
	defer stream.CloseSend()

	// Send play command
	stream.Send(&pb.ControlRequest{
		SequenceId: 1,
		Payload: &pb.ControlRequest_Play{
			Play: &pb.PlayPayload{
				MediaId:       "tt-test-4k-movie",
				SeasonNumber:  0,
				EpisodeNumber: 0,
			},
		},
	})

	// Listen for events in background
	go func() {
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				log("📡 Stream closed by server")
				return
			}
			if err != nil {
				log("⚠ Recv error: %v", err)
				return
			}
			handleResponse(resp)
		}
	}()

	// Keep sending ping every second
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	seq := int64(2)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-sigCh:
			log("\n👋 退出")
			return
		case <-ticker.C:
			seq++
			stream.Send(&pb.ControlRequest{
				SequenceId: seq,
				Payload: &pb.ControlRequest_Ping{
					Ping: &pb.PingPayload{
						ClientTimestamp: time.Now().UnixMilli(),
						DeviceId:        *deviceID,
					},
				},
			})
		}
	}
}

func runChaosTest(conn *grpc.ClientConn, token string) {
	log("\n🌪 混沌测试 — 30s 后模拟断线 → 5s 后重连 → 验证 Unregister")
	stream := openControlStream(conn, token)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	seq := int64(0)

	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				return
			}
			handleResponse(resp)
		}
	}()

	start := time.Now()
	for {
		seq++
		stream.Send(&pb.ControlRequest{
			SequenceId: seq,
			Payload: &pb.ControlRequest_Ping{
				Ping: &pb.PingPayload{
					ClientTimestamp: time.Now().UnixMilli(),
					DeviceId:        *deviceID,
				},
			},
		})

		if time.Since(start) > 30*time.Second {
			log("💀 模拟断线 — 强制关闭流")
			stream.CloseSend()
			log("⏳ 等待 5 秒...")
			time.Sleep(5 * time.Second)

			log("🔄 重连中...")
			newStream := openControlStream(conn, token)
			defer newStream.CloseSend()

			// Continue ping on new stream
			for i := 0; i < 10; i++ {
				newStream.Send(&pb.ControlRequest{
					SequenceId: seq + int64(i),
					Payload: &pb.ControlRequest_Ping{
						Ping: &pb.PingPayload{
							ClientTimestamp: time.Now().UnixMilli(),
							DeviceId:        *deviceID,
						},
					},
				})
				time.Sleep(1 * time.Second)
			}
			log("✅ 重连测试完成 — 检查 NAS 日志: docker logs x-media-server")
			return
		}
		time.Sleep(1 * time.Second)
	}
}

func runFullTest(conn *grpc.ClientConn, token string) {
	log("\n🚀 全链路测试")

	// Step 1: PanSearch
	contentClient := pb.NewContentServiceClient(conn)
	sresp, err := contentClient.Search(context.Background(), &pb.SearchRequest{
		Query: "阿凡达3 4K",
		Pagination: &pb.PaginationRequest{Page: 1, PageSize: 5},
	})
	if err != nil {
		log("⚠ Search failed: %v (expected — 盘搜为 HTTP API)", err)
	} else {
		log("🔍 Search results: %d items", len(sresp.Items))
	}

	// Step 2: Playback + ControlStream
	log("\n🎬 建立播放控制流...")
	stream := openControlStream(conn, token)
	defer stream.CloseSend()

	stream.Send(&pb.ControlRequest{
		SequenceId: 1,
		Payload: &pb.ControlRequest_Play{
			Play: &pb.PlayPayload{MediaId: "tt-test-transfer", SeasonNumber: 0, EpisodeNumber: 0, StartPositionSecs: 0},
		},
	})
	log("📤 已发送 PlayPayload → tt-test-transfer")

	// Step 3: Transfer check (gRPC)
	transferClient := pb.NewTransferServiceClient(conn)
	tresp, err := transferClient.GetTransferStatus(context.Background(), &pb.TransferStatusRequest{
		TaskId: "test-task-001",
	})
	if err != nil {
		log("⚠ Transfer status: %v (expected — no real task yet)", err)
	} else {
		log("📦 Transfer: %v — progress %d%%", tresp.Status, tresp.Progress)
	}

	// Step 4: Listen for events
	log("\n📡 监听 ControlStream 推送...")
	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				return
			}
			handleResponse(resp)
		}
	}()

	// Keep alive
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	seq := int64(2)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	log("\n✅ 测试运行中 — Ctrl+C 退出")
	log("   观察 NAS: docker logs -f x-media-server")
	log("══════════════════════════════════════════\n")

	for {
		select {
		case <-sigCh:
			log("\n👋 退出")
			return
		case <-ticker.C:
			seq++
			stream.Send(&pb.ControlRequest{
				SequenceId: seq,
				Payload: &pb.ControlRequest_Ping{
					Ping: &pb.PingPayload{
						ClientTimestamp: time.Now().UnixMilli(),
						DeviceId:        *deviceID,
					},
				},
			})
		}
	}
}

func openControlStream(conn *grpc.ClientConn, token string) pb.PlaybackControlService_ControlStreamClient {
	client := pb.NewPlaybackControlServiceClient(conn)
	stream, err := client.ControlStream(context.Background())
	if err != nil {
		fatal("ControlStream failed: %v", err)
	}

	// Send device registration
	err = stream.Send(&pb.ControlRequest{
		SequenceId: 0,
		Payload: &pb.ControlRequest_Ping{
			Ping: &pb.PingPayload{
				ClientTimestamp: time.Now().UnixMilli(),
				DeviceId:        *deviceID,
			},
		},
	})
	if err != nil {
		fatal("Device registration failed: %v", err)
	}

	// Wait for server ACK (Pong) before returning
	resp, err := stream.Recv()
	if err != nil {
		fatal("Registration ACK failed: %v", err)
	}
	if pong, ok := resp.Payload.(*pb.ControlResponse_Pong); ok {
		rtt := time.Now().UnixMilli() - pong.Pong.ClientTimestamp
		log("✅ ControlStream established — device: %s (RTT: %dms)", *deviceID, rtt)
	} else {
		log("✅ ControlStream established — device: %s (no Pong ACK)", *deviceID)
	}
	return stream
}

func handleResponse(resp *pb.ControlResponse) {
	ts := time.Now().Format("15:04:05")
	switch p := resp.Payload.(type) {
	case *pb.ControlResponse_Pong:
		rtt := time.Now().UnixMilli() - p.Pong.ClientTimestamp
		log("[%s] 💓 Pong — RTT: %dms", ts, rtt)
	case *pb.ControlResponse_StateChanged:
		log("[%s] 🎬 State: %v", ts, p.StateChanged.State)
	case *pb.ControlResponse_PositionUpdated:
		// Suppress frequent updates
	case *pb.ControlResponse_MediaChanged:
		log("[%s] 📺 Media changed", ts)
	case *pb.ControlResponse_MediaEnded:
		log("[%s] ⏹ Media ended: %s", ts, p.MediaEnded.MediaId)
	case *pb.ControlResponse_Error:
		log("[%s] ❌ Error: %s", ts, p.Error.Error.Message)
	case *pb.ControlResponse_Buffering:
		log("[%s] ⏳ Buffering: %d%%", ts, p.Buffering.ProgressPercent)
	default:
		log("[%s] 📩 %T", ts, resp.Payload)
	}
}

func log(format string, args ...interface{}) {
	fmt.Printf(format+"\n", args...)
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "❌ "+format+"\n", args...)
	os.Exit(1)
}
