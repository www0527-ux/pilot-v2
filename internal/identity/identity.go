// Package identity carries a server-authenticated account through agent/tool calls.
package identity

import "context"

type ownerKey struct{}

func WithOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerKey{}, owner)
}

func Owner(ctx context.Context) string { owner, _ := ctx.Value(ownerKey{}).(string); return owner }
