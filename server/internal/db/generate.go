package db

// sqlc は cgo（pg_query）を使うので go.mod の tool には入れず、バージョンを固定して go run で動かす。
//go:generate go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate
