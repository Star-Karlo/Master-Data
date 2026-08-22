// Package clients holds this service's outbound gRPC connections.
package clients

import (
	"context"
	"fmt"

	"github.com/karlo/masterdata-service/internal/platform/authctx"
	authv1 "github.com/karlo/masterdata-service/internal/platform/genproto/karlo/auth/v1"
	"github.com/karlo/masterdata-service/internal/platform/grpcutil"
	"google.golang.org/grpc"
)

// Auth is the authentication service client.
//
// It exists so this service can honour credentials it cannot verify itself,
// namely API keys. Ordinary JWTs are verified locally with the public key and
// never reach here.
type Auth struct {
	conn   *grpc.ClientConn
	client authv1.AuthServiceClient
}

// NewAuth dials the authentication service.
func NewAuth(target, serviceName, serviceToken string) (*Auth, error) {
	conn, err := grpcutil.Dial(grpcutil.DialConfig{
		Service:      serviceName,
		Target:       target,
		ServiceToken: serviceToken,
	})
	if err != nil {
		return nil, fmt.Errorf("clients: auth: %w", err)
	}
	return &Auth{conn: conn, client: authv1.NewAuthServiceClient(conn)}, nil
}

// Close releases the connection.
func (a *Auth) Close() error { return a.conn.Close() }

// ValidateToken satisfies authctx.RemoteValidator.
func (a *Auth) ValidateToken(ctx context.Context, token string) (authctx.Principal, error) {
	resp, err := a.client.ValidateToken(ctx, &authv1.ValidateTokenRequest{Token: token})
	if err != nil {
		return authctx.Principal{}, fmt.Errorf("clients: validate token: %w", err)
	}
	if !resp.GetValid() {
		return authctx.Principal{}, fmt.Errorf("clients: %s", resp.GetReason())
	}

	return principalFrom(resp.GetUser()), nil
}

// principalFrom maps the authentication service's user into a principal.
//
// The per-product access map is copied wholesale rather than flattened: this
// service resolves its own product through authctx, and flattening here would
// discard the other product's access from a token that legitimately carries
// both.
func principalFrom(user *authv1.User) authctx.Principal {
	p := authctx.Principal{
		UserID:          user.GetId(),
		CompanyID:       user.GetCompanyId(),
		ParentID:        user.GetParentId(),
		IsPlatformStaff: user.GetIsPlatformStaff(),
		FMSTenantID:     user.GetFmsTenantId(),
	}

	if access := user.GetAccess(); len(access) > 0 {
		p.Access = make(map[authctx.Product]authctx.ProductAccess, len(access))
		for product, a := range access {
			p.Access[authctx.Product(product)] = authctx.ProductAccess{
				Role:        a.GetRole(),
				Permissions: a.GetPermissions(),
				Features:    a.GetFeatures(),
			}
		}
	}

	return p
}

// GetUser resolves one user, for handlers that need to display a driver's name
// alongside a truck.
func (a *Auth) GetUser(ctx context.Context, id string) (*authv1.User, error) {
	resp, err := a.client.GetUser(ctx, &authv1.GetUserRequest{Id: id})
	if err != nil {
		return nil, fmt.Errorf("clients: get user: %w", err)
	}
	return resp.GetUser(), nil
}
