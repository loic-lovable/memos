package auth

import "context"

type archivedAccessTokenObserverKey struct{}

// WithArchivedAccessTokenObserver installs a request-local observer for a valid
// access token whose database user is archived. It receives no credentials and
// does not change authentication. This is not a final authorization verdict:
// callers must also observe the protected request's actual response.
// Only the disposable pilot launcher installs this diagnostic hook.
func WithArchivedAccessTokenObserver(ctx context.Context, observer func(int32)) context.Context {
	return context.WithValue(ctx, archivedAccessTokenObserverKey{}, observer)
}

func observeArchivedAccessToken(ctx context.Context, userID int32) {
	if observer, ok := ctx.Value(archivedAccessTokenObserverKey{}).(func(int32)); ok && observer != nil {
		observer(userID)
	}
}
