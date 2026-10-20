// Command maiakctl is the Maiak admin CLI.
//
//	maiakctl hashpw [-m KiB] [-t iterations] [-p parallelism]   print an argon2id hash of a password read from stdin
//	maiakctl db bootstrap                                       create/repair the database and roles (see internal/dbbootstrap)
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/jackc/pgx/v5"
	"golang.org/x/term"

	"github.com/theechofive/maiak/backend/internal/dbbootstrap"
	"github.com/theechofive/maiak/backend/internal/platform/config"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/logging"
)

var (
	version = "dev"
	commit  = "unknown"
)

const usage = `usage:
  maiakctl hashpw [-m KiB] [-t iterations] [-p parallelism]
  maiakctl db bootstrap
  maiakctl version
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "maiakctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("missing command")
	}
	switch {
	case args[0] == "hashpw":
		return hashpw(args[1:], os.Stdin, os.Stdout, os.Stderr)
	case args[0] == "db" && len(args) > 1 && args[1] == "bootstrap":
		return dbBootstrap()
	case args[0] == "version":
		fmt.Printf("maiakctl %s (%s)\n", version, commit)
		return nil
	}
	fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

func hashpw(args []string, stdin *os.File, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("hashpw", flag.ContinueOnError)
	p := crypto.DefaultArgon2Params
	m := fs.Uint("m", uint(p.Memory), "memory in KiB")
	t := fs.Uint("t", uint(p.Iterations), "iterations")
	par := fs.Uint("p", uint(p.Parallelism), "parallelism (1-255)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *m > 1<<32-1 || *t > 1<<32-1 || *par < 1 || *par > 255 {
		return errors.New("parameter out of range")
	}
	if *m > crypto.MaxArgon2Memory {
		return fmt.Errorf("-m must be at most %d KiB (256 MiB): the api refuses larger hashes", crypto.MaxArgon2Memory)
	}
	p.Memory, p.Iterations, p.Parallelism = uint32(*m), uint32(*t), uint8(*par) //nolint:gosec // range-checked above

	password, err := readPassword(stdin, stderr)
	if err != nil {
		return err
	}
	h, err := crypto.HashPassword(password, p)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, h)
	return err
}

// readPassword prompts without echo on a terminal, or reads one line from a pipe.
func readPassword(stdin *os.File, stderr io.Writer) (string, error) {
	fd := int(stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(stderr, "Password: ")
		a, err := term.ReadPassword(fd)
		fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		fmt.Fprint(stderr, "Repeat password: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(a, b) {
			return "", errors.New("passwords do not match")
		}
		return checkPassword(string(a))
	}
	line, err := bufio.NewReader(io.LimitReader(stdin, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = trimNewline(line)
	return checkPassword(line)
}

func trimNewline(s string) string {
	if n := len(s); n > 0 && s[n-1] == '\n' {
		s = s[:n-1]
		if n := len(s); n > 0 && s[n-1] == '\r' {
			s = s[:n-1]
		}
	}
	return s
}

func checkPassword(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty password")
	}
	return p, nil
}

func dbBootstrap() error {
	cfg, err := config.LoadBootstrap(os.LookupEnv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stderr, cfg.LogLevel).With("service", "maiakctl", "command", "db bootstrap", "version", version)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	ns := cfg.Namespace
	if ns == "" {
		if ns, err = dbbootstrap.InClusterNamespace(); err != nil {
			return err
		}
	}
	secrets, err := dbbootstrap.NewInClusterSecrets()
	if err != nil {
		return err
	}
	admin, err := adminConnConfig(cfg)
	if err != nil {
		return err
	}
	_, err = dbbootstrap.Run(ctx, dbbootstrap.Options{
		Admin:      admin,
		Host:       cfg.Host,
		Port:       cfg.Port,
		DBName:     cfg.DBName,
		SSLMode:    cfg.SSLMode,
		Namespace:  ns,
		SecretName: cfg.SecretName,
		Secrets:    secrets,
		Out:        os.Stdout,
		Logger:     log,
	})
	if err != nil {
		return err
	}
	log.Info("bootstrap complete")
	return nil
}

// adminConnConfig builds the admin connection to database "postgres". The URL is assembled
// with net/url so the password is escaped, and is never logged.
func adminConnConfig(cfg config.Bootstrap) (*pgx.ConnConfig, error) {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.AdminUser, cfg.AdminPassword.Value()),
		Host:     net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Path:     "/postgres",
		RawQuery: url.Values{"sslmode": {cfg.SSLMode}, "application_name": {"maiakctl-bootstrap"}}.Encode(),
	}
	c, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, errors.New("invalid admin connection settings")
	}
	return c, nil
}
