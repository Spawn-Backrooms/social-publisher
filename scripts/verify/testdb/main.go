// testdb manages only uniquely named schemas owned by the executable smoke.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 3 || (os.Args[1] != "create-schema" && os.Args[1] != "drop-schema") || !regexp.MustCompile(`^sp_verify_smoke_[a-f0-9]{20}$`).MatchString(os.Args[2]) {
		return errors.New("usage: testdb create-schema|drop-schema sp_verify_smoke_<20 lowercase hex digits>")
	}
	if os.Getenv("TEST_DATABASE_URL") == "" {
		return errors.New("TEST_DATABASE_URL is required")
	}
	if _, set := os.LookupEnv("TEST_PGLITE"); set {
		return errors.New("TEST_PGLITE must be unset for native executable verification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		return errors.New("cannot connect to the native test database")
	}
	defer conn.Close(ctx)
	statement := "CREATE SCHEMA " + pgx.Identifier{os.Args[2]}.Sanitize()
	if os.Args[1] == "drop-schema" {
		statement = "DROP SCHEMA " + pgx.Identifier{os.Args[2]}.Sanitize() + " CASCADE"
	}
	if _, err = conn.Exec(ctx, statement); err != nil {
		return errors.New("isolated smoke schema operation failed")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"operation": os.Args[1], "schema": os.Args[2], "passed": true})
}
