package auth

import "github.com/jackc/pgx/v5"

// SetLoginHooks installs test hooks around Login's session transaction.
func SetLoginHooks(s *Service, afterVerify func(), inSessionTx func(pgx.Tx)) {
	s.afterVerify, s.inSessionTx = afterVerify, inSessionTx
}
