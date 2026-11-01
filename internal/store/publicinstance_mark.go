package store

import "context"

func addPublicInstanceMark(ctx context.Context, x executor) error {
	return addColumn(ctx, x, "flats", "created_public_instance", "INTEGER NOT NULL DEFAULT 0 CHECK(created_public_instance IN (0,1))")
}
