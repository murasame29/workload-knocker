package kedaexternal

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"

	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const componentName = "external-scaler"

// Server serves a Hub over gRPC as a server.Lifecycle.
type Server struct {
	hub  *Hub
	addr string
	grpc *grpc.Server
}

// NewServer returns a Server listening on port. certFile and keyFile enable TLS and are set together
// or not at all; the certificate is re-read on every handshake so rotation needs no restart.
func NewServer(hub *Hub, port int, certFile, keyFile string) (*Server, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("EXTERNAL_SCALER_PORT must be between 1 and 65535, got %d", port)
	}
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("EXTERNAL_SCALER_TLS_CERT and EXTERNAL_SCALER_TLS_KEY must be set together")
	}
	var opts []grpc.ServerOption
	if certFile != "" {
		opts = append(opts, grpc.Creds(credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				c, err := tls.LoadX509KeyPair(certFile, keyFile)
				if err != nil {
					return nil, err
				}
				return &c, nil
			},
		})))
	}
	s := grpc.NewServer(opts...)
	pb.RegisterExternalScalerServer(s, hub)
	return &Server{hub: hub, addr: fmt.Sprintf(":%d", port), grpc: s}, nil
}

// Name implements server.Lifecycle.
func (s *Server) Name() string { return componentName }

// Start serves until Shutdown.
func (s *Server) Start(_ context.Context) error {
	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}
	slog.Info("external scaler listening", slog.String("server.address", lis.Addr().String()))
	if err := s.grpc.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

// Shutdown ends the open streams, then stops gracefully or forcibly once ctx expires.
func (s *Server) Shutdown(ctx context.Context) error {
	s.hub.Close()
	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.grpc.Stop()
		<-done
	}
	return nil
}
