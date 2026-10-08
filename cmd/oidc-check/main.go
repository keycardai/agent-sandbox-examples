// Command oidc-check verifies a ServiceAccount token against the keys its
// cluster's issuer publishes. It reads the issuer's discovery document and
// the JWKS that document names over https, as an external verifier does.
//
//	kubectl -n agents create token <sa> --audience "$AUDIENCE" |
//	  oidc-check --issuer "$ISSUER" --token-file - --audience "$AUDIENCE" \
//	    --subject system:serviceaccount:agents:<sa>
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/keycardai/agent-sandbox-poc/internal/issuer"
)

var errToken = errors.New("token refused")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newCommand().Run(ctx, os.Args)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "oidc-check: %v\n", err)
		os.Exit(1)
	}
}

func newCommand() *cli.Command {
	return &cli.Command{
		Name:            "oidc-check",
		Usage:           "verify a ServiceAccount token against the keys its cluster's issuer publishes",
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "issuer", Required: true, Usage: "the https issuer URL, exactly as the cluster names it"},
			&cli.StringFlag{Name: "token-file", Required: true, Usage: "the token to verify; - reads standard input"},
			&cli.StringFlag{Name: "audience", Required: true, Usage: "the audience the token must name"},
			&cli.StringFlag{Name: "subject", Required: true, Usage: "the subject the token must name"},
			&cli.StringFlag{Name: "ca-file", Usage: "a PEM file whose certificates join the system roots"},
		},
		Action: check,
	}
}

func check(ctx context.Context, cmd *cli.Command) error {
	token, err := readToken(cmd.Reader, cmd.String("token-file"))
	if err != nil {
		return err
	}
	want := issuer.Expect{
		Issuer:   cmd.String("issuer"),
		Audience: cmd.String("audience"),
		Subject:  cmd.String("subject"),
		CAFile:   cmd.String("ca-file"),
	}
	if err = issuer.Verify(ctx, want, token); err != nil {
		return fmt.Errorf("verifying the token: %w", err)
	}
	_, err = fmt.Fprintf(cmd.Writer, "oidc-check: token for %s verifies against %s\n", want.Subject, want.Issuer)
	if err != nil {
		return fmt.Errorf("writing the result: %w", err)
	}
	return nil
}

// readToken reads the token from a file, or from stdin when path is "-",
// and refuses one larger than issuer.MaxBody.
func readToken(stdin io.Reader, path string) (string, error) {
	src := stdin
	if path != "-" {
		file, err := os.Open(path) //nolint:gosec // G304: the path is the token file the operator named.
		if err != nil {
			return "", fmt.Errorf("%w: reading %s: %w", errToken, path, err)
		}
		defer func() { _ = file.Close() }()
		src = file
	}
	raw, err := io.ReadAll(io.LimitReader(src, issuer.MaxBody+1))
	if err != nil {
		return "", fmt.Errorf("%w: reading %s: %w", errToken, path, err)
	}
	if len(raw) > issuer.MaxBody {
		return "", fmt.Errorf("%w: %s holds more than %d bytes", errToken, path, issuer.MaxBody)
	}
	return strings.TrimSpace(string(raw)), nil
}
