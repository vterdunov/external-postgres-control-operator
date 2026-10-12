//go:build deliveryprobe

// The delivery suite compiles this private-input client into its owned temporary
// directory. It reports connection outcome and typed SQLSTATE only, never errors
// that could include a credential or connection URI.
package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type connectionInput struct {
	Host     string `json:"host"`
	Port     uint16 `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
}

type connectionResult struct {
	Connected bool   `json:"connected"`
	SQLState  string `json:"sqlState"`
}

func probe() int {
	var input connectionInput
	if err := json.UnmarshalRead(io.LimitReader(os.Stdin, 1<<20), &input); err != nil {
		return 2
	}
	// Only the runner-owned loopback forwarding endpoint is admissible.
	if input.Host != "127.0.0.1" || input.Port == 0 || input.User == "" || input.Database == "" {
		return 2
	}
	config, err := pgx.ParseConfig("sslmode=disable")
	if err != nil {
		return 2
	}
	config.Host, config.Port = input.Host, input.Port
	config.User, config.Password, config.Database = input.User, input.Password, input.Database
	config.Fallbacks = nil
	config.ConnectTimeout = 5 * time.Second
	config.RuntimeParams["application_name"] = "epco-delivery-auth-control"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := pgx.ConnectConfig(ctx, config)
	result := connectionResult{Connected: err == nil}
	if postgresError, ok := errors.AsType[*pgconn.PgError](err); ok {
		result.SQLState = postgresError.Code
	}
	if connection != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer closeCancel()
		if connection.Close(closeCtx) != nil {
			return 2
		}
	}
	if json.MarshalWrite(os.Stdout, result) != nil {
		return 2
	}
	return 0
}

func main() {
	os.Exit(probe())
}
