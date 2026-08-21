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

	user := resp.GetUser()
	principal := authctx.Principal{
		UserID:    user.GetId(),
		Role:      user.GetRole(),
		CompanyID: user.GetCompanyId(),
		ParentID:  user.GetParentId(),
	}

	if perm := user.GetPermission(); len(perm) > 0 {
		principal.Permission = make(map[string]map[string]bool, len(perm))
		for module, actions := range perm {
			principal.Permission[module] = actions.GetActions()
		}
	}

	return principal, nil
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
