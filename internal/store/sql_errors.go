package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

func (s *Store) isUniqueViolation(err error) bool {
	if s.postgresSchema != "" {
		var pg *pgconn.PgError
		return errors.As(err, &pg) && pg.Code == "23505"
	}
	return s.driver.IsUniqueViolation(err)
}
