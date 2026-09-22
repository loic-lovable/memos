package client

import (
	"context"
	"errors"
)

// SaveIntent must durably and immutably save the complete enrollment-bound intent
// before returning nil. Errors prevent submission. The caller owns its storage.
type SaveIntent func(ctx context.Context, id string, intent []byte) error

func saveOperation(ctx context.Context, save SaveIntent, id string, intent []byte) error {
	if save == nil {
		return errors.New("durable intent saver is required")
	}
	return save(ctx, id, intent)
}
