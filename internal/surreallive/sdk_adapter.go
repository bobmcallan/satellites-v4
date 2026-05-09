// SDK adapter — wraps *surrealdb.DB so the Subscriber can speak the
// production SDK without leaking surrealdb-go types into the package's
// public surface. Tests bypass this adapter via the DB interface; only
// cmd/satellites uses it at boot.

package surreallive

import (
	"context"
	"fmt"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"
)

// NewSDKAdapter wraps a *surrealdb.DB to satisfy the package's DB
// interface. The adapter is a thin shim: Live + LiveNotifications +
// CloseLiveNotifications all map 1:1 to the SDK's symbols. The
// Notification translation re-shapes connection.Notification.Result
// (which is `interface{}` in the SDK) into a typed `map[string]any`
// the Subscriber can decode without importing the SDK.
func NewSDKAdapter(db *surrealdb.DB) DB {
	return &sdkAdapter{db: db}
}

type sdkAdapter struct {
	db *surrealdb.DB
}

func (a *sdkAdapter) Live(ctx context.Context, table string, diff bool) (string, error) {
	uuid, err := surrealdb.Live(ctx, a.db, surrealmodels.Table(table), diff)
	if err != nil {
		return "", err
	}
	if uuid == nil {
		return "", fmt.Errorf("surreallive: nil live id for table %s", table)
	}
	return uuid.String(), nil
}

func (a *sdkAdapter) LiveNotifications(liveID string) (<-chan Notification, error) {
	src, err := a.db.LiveNotifications(liveID)
	if err != nil {
		return nil, err
	}
	out := make(chan Notification, 16)
	go func() {
		defer close(out)
		for n := range src {
			notif := Notification{
				Action: Action(n.Action),
			}
			if n.ID != nil {
				notif.ID = n.ID.String()
			}
			if row, ok := n.Result.(map[string]any); ok {
				notif.Result = row
			}
			out <- notif
		}
	}()
	return out, nil
}

func (a *sdkAdapter) CloseLiveNotifications(liveID string) error {
	return a.db.CloseLiveNotifications(liveID)
}
