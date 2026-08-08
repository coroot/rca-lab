package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"

	pb "github.com/coroot/rca-lab/services/recommendation-service/proto"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

type server struct {
	pb.UnimplementedRecommendationServiceServer
}

// leakedProfiles is a per-request "personalization cache". Each call stores a
// freshly computed personalization profile under a unique key and it is never
// evicted, so the map grows without bound under load.
var leakedProfiles = struct {
	sync.Mutex
	m map[string][]byte
}{m: map[string][]byte{}}

func (s *server) GetRecommendations(ctx context.Context, req *pb.GetRecommendationsRequest) (*pb.GetRecommendationsResponse, error) {
	limit := int(req.Limit)
	if limit <= 0 || limit > 50 {
		limit = 10
	}

	// Compute and cache a personalization profile for this request. The profile
	// is ~256KB and is retained in an unbounded package-level cache keyed by a
	// unique id, never freed.
	profile := make([]byte, 256*1024)
	rand.Read(profile)
	key := fmt.Sprintf("%s-%d", req.UserId, time.Now().UnixNano())
	leakedProfiles.Lock()
	leakedProfiles.m[key] = profile
	leakedProfiles.Unlock()

	ids := make([]string, limit)
	for i := range ids {
		ids[i] = fmt.Sprintf("product-%d", rand.Intn(100000))
	}
	return &pb.GetRecommendationsResponse{ProductIds: ids}, nil
}

func main() {
	ctx := context.Background()

	shutdownTelemetry, err := initTelemetry(ctx, "recommendation-service")
	if err != nil {
		slog.Error("failed to initialize telemetry", "error", err)
		os.Exit(1)
	}
	defer shutdownTelemetry(context.Background())

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":9000"
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("listen failed", "error", err)
		os.Exit(1)
	}
	s := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	pb.RegisterRecommendationServiceServer(s, &server{})
	healthpb.RegisterHealthServer(s, health.NewServer())
	reflection.Register(s)
	slog.Info("recommendation-service listening", "addr", addr)
	if err := s.Serve(lis); err != nil {
		slog.Error("serve failed", "error", err)
		os.Exit(1)
	}
}
