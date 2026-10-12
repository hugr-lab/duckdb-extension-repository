package store

import "context"

// MigrateTo applies the migrations up to level; for migration tests.
func MigrateTo(ctx context.Context, s *Store, level int) error {
	ms, err := loadMigrations(s.d.Name)
	if err != nil {
		return err
	}
	for {
		applied, err := s.migrateOne(ctx, ms[:level])
		if err != nil || !applied {
			return err
		}
	}
}
