package grpcserver

import (
	"context"

	"metrics-system/internal/auth"
	metricspb "metrics-system/internal/proto/metricspb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// grpcActionForMethod maps a full gRPC method name to its RBAC action. Methods
// that need no auth (reflection, health) return required=false.
func grpcActionForMethod(fullMethod string) (auth.Action, bool) {
	switch fullMethod {
	case metricspb.MetricsService_Ingest_FullMethodName,
		metricspb.MetricsService_IngestStream_FullMethodName:
		return auth.ActionIngest, true
	case metricspb.MetricsService_Query_FullMethodName:
		return auth.ActionQuery, true
	default:
		return "", false // reflection etc. are public
	}
}

// authorize authenticates the call and checks its action, returning a context
// carrying the principal on success.
func authorize(ctx context.Context, authenticator auth.Authenticator, fullMethod string) (context.Context, error) {
	action, required := grpcActionForMethod(fullMethod)
	if !required {
		return ctx, nil
	}
	principal, err := authenticator.Authenticate(ctx, credentialsFromMetadata(ctx))
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	if !principal.Can(action) {
		return nil, status.Error(codes.PermissionDenied, "forbidden")
	}
	return auth.WithPrincipal(ctx, principal), nil
}

func credentialsFromMetadata(ctx context.Context) auth.Credentials {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return auth.Credentials{}
	}
	var c auth.Credentials
	if v := md.Get("x-api-key"); len(v) > 0 {
		c.APIKey = v[0]
	}
	if v := md.Get("authorization"); len(v) > 0 {
		if token, ok := auth.BearerToken(v[0]); ok {
			c.Bearer = token
		}
	}
	return c
}

func authUnary(authenticator auth.Authenticator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		newCtx, err := authorize(ctx, authenticator, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(newCtx, req)
	}
}

func authStream(authenticator auth.Authenticator) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		newCtx, err := authorize(ss.Context(), authenticator, info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &authStreamWrapper{ServerStream: ss, ctx: newCtx})
	}
}

// authStreamWrapper overrides Context() so downstream handlers see the
// principal-carrying context.
type authStreamWrapper struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *authStreamWrapper) Context() context.Context { return w.ctx }
