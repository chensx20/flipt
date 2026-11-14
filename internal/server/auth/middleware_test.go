package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	fliptErrors "go.flipt.io/flipt/errors"
	"go.flipt.io/flipt/internal/containers"
	"go.flipt.io/flipt/internal/storage/auth"
	"go.flipt.io/flipt/internal/storage/auth/memory"
	authrpc "go.flipt.io/flipt/rpc/flipt/auth"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type errorAuthenticator struct {
	err error
}

func (a errorAuthenticator) GetAuthenticationByClientToken(context.Context, string) (*authrpc.Authentication, error) {
	return nil, a.err
}

func TestUnaryInterceptor_Cancellation(t *testing.T) {
	md := metadata.New(map[string]string{"authorization": "Bearer token"})
	for _, test := range []struct {
		name string
		ctx  context.Context
		err  error
		want codes.Code
	}{
		{name: "canceled context error", ctx: canceledAuthContext(), err: context.Canceled, want: codes.Canceled},
		{name: "wrapped deadline error", ctx: expiredAuthContext(), err: fmt.Errorf("lookup: %w", context.DeadlineExceeded), want: codes.DeadlineExceeded},
		{name: "postgres cancellation", ctx: canceledAuthContext(), err: fmt.Errorf("lookup: %w", &pq.Error{Code: pq.ErrorCode("57014")}), want: codes.Canceled},
		{name: "wrapped sql cancellation", ctx: expiredAuthContext(), err: fmt.Errorf("lookup: %w", fliptErrors.ErrCanceled("query canceled")), want: codes.DeadlineExceeded},
		{name: "live sql cancellation", ctx: context.Background(), err: storageSQLCancellationErrorForAuth(), want: codes.Unauthenticated},
		{name: "invalid credentials", ctx: context.Background(), err: fmt.Errorf("invalid credentials"), want: codes.Unauthenticated},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := metadata.NewIncomingContext(test.ctx, md)
			_, err := UnaryInterceptor(zaptest.NewLogger(t), errorAuthenticator{err: test.err})(
				ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, interface{}) (interface{}, error) {
					return nil, nil
				})
			require.Equal(t, test.want, status.Code(err))
		})
	}
}

func canceledAuthContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func expiredAuthContext() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	return ctx
}

func storageSQLCancellationErrorForAuth() error {
	return fliptErrors.ErrCanceled("query canceled")
}

// fakeserver is used to test skipping auth
var fakeserver struct{}

func TestUnaryInterceptor(t *testing.T) {
	authenticator := memory.NewStore()
	clientToken, storedAuth, err := authenticator.CreateAuthentication(
		context.TODO(),
		&auth.CreateAuthenticationRequest{Method: authrpc.Method_METHOD_TOKEN},
	)
	require.NoError(t, err)

	// expired auth
	expiredToken, _, err := authenticator.CreateAuthentication(
		context.TODO(),
		&auth.CreateAuthenticationRequest{
			Method:    authrpc.Method_METHOD_TOKEN,
			ExpiresAt: timestamppb.New(time.Now().UTC().Add(-time.Hour)),
		},
	)
	require.NoError(t, err)

	for _, test := range []struct {
		name         string
		metadata     metadata.MD
		server       any
		options      []containers.Option[InterceptorOptions]
		expectedErr  error
		expectedAuth *authrpc.Authentication
	}{
		{
			name: "successful authentication (authorization header)",
			metadata: metadata.MD{
				"Authorization": []string{"Bearer " + clientToken},
			},
			expectedAuth: storedAuth,
		},
		{
			name: "successful authentication (cookie header)",
			metadata: metadata.MD{
				"grpcgateway-cookie": []string{"flipt_client_token=" + clientToken},
			},
			expectedAuth: storedAuth,
		},
		{
			name:     "successful authentication (skipped)",
			metadata: metadata.MD{},
			server:   &fakeserver,
			options: []containers.Option[InterceptorOptions]{
				WithServerSkipsAuthentication(&fakeserver),
			},
		},
		{
			name: "token has expired",
			metadata: metadata.MD{
				"Authorization": []string{"Bearer " + expiredToken},
			},
			expectedErr: errUnauthenticated,
		},
		{
			name: "client token not found in store",
			metadata: metadata.MD{
				"Authorization": []string{"Bearer unknowntoken"},
			},
			expectedErr: errUnauthenticated,
		},
		{
			name: "client token missing Bearer prefix",
			metadata: metadata.MD{
				"Authorization": []string{clientToken},
			},
			expectedErr: errUnauthenticated,
		},
		{
			name: "authorization header empty",
			metadata: metadata.MD{
				"Authorization": []string{},
			},
			expectedErr: errUnauthenticated,
		},
		{
			name: "cookie header with no flipt_client_token",
			metadata: metadata.MD{
				"grcpgateway-cookie": []string{"blah"},
			},
			expectedErr: errUnauthenticated,
		},
		{
			name:        "authorization header not set",
			metadata:    metadata.MD{},
			expectedErr: errUnauthenticated,
		},
		{
			name:        "no metadata on context",
			metadata:    nil,
			expectedErr: errUnauthenticated,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			var (
				logger = zaptest.NewLogger(t)

				ctx          = context.Background()
				retrievedCtx = ctx
				handler      = func(ctx context.Context, req interface{}) (interface{}, error) {
					// update retrievedCtx to the one delegated to the handler
					retrievedCtx = ctx
					return nil, nil
				}
			)

			if test.metadata != nil {
				ctx = metadata.NewIncomingContext(ctx, test.metadata)
			}

			_, err := UnaryInterceptor(logger, authenticator, test.options...)(
				ctx,
				nil,
				&grpc.UnaryServerInfo{Server: test.server},
				handler,
			)
			require.Equal(t, test.expectedErr, err)
			assert.Equal(t, test.expectedAuth, GetAuthenticationFrom(retrievedCtx))
		})
	}
}
