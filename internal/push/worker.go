package push

import (
	"context"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
)

// Dispatch batches due reminders; each send is rechecked under its session
// transaction lock. Invalid credentials cannot be logged through this path.
func (s *Service) Dispatch(ctx context.Context, db *store.Store, now time.Time) error {
	ids, err := db.DuePushIDs(ctx, now, 50)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		current := time.Now().UTC()
		err = db.DeliverPush(ctx, id, current, func(p store.PushSubscription) (int, error) { return s.Send(ctx, p, current) })
		if err != nil {
			return err
		}
	}
	return nil
}
