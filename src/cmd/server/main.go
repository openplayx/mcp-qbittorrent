// Command bittorrent-server runs the OpenPlayX BitTorrent API.
//
//	bittorrent-server -config config.json          serve
//	bittorrent-server new-token <caller>           print a token and its secrets.json entry
//	bittorrent-server engine-password-hash         read a password on stdin, print qBittorrent's PBKDF2 value
package main

import (
	"bufio"
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/openplayx/bittorrent-server/src/internal/api"
	"github.com/openplayx/bittorrent-server/src/internal/config"
	"github.com/openplayx/bittorrent-server/src/internal/files"
	"github.com/openplayx/bittorrent-server/src/internal/qbittorrent"
	"github.com/openplayx/bittorrent-server/src/internal/torrents"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "new-token":
			newToken(os.Args[2:])
			return
		case "engine-password-hash":
			enginePasswordHash()
			return
		case "version":
			fmt.Println(version)
			return
		}
	}
	configPath := flag.String("config", "config.json", "path to config.json")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := serve(*configPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func serve(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	secrets, err := config.LoadSecrets(cfg.SecretsFile)
	if err != nil {
		return err
	}
	engine, err := qbittorrent.New(cfg.Engine.URL, secrets.Engine.Username, secrets.Engine.Password,
		time.Duration(cfg.Engine.TimeoutSeconds)*time.Second)
	if err != nil {
		return err
	}
	svc := torrents.New(&cfg, engine, log, files.FreeBytes)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fatal := make(chan error, 1)
	go func() {
		// Bind to the pinned engine version before reconciling anything.
		for {
			v, w, err := engine.Check(ctx, cfg.Engine.SupportedVersions, cfg.Engine.MinWebAPIVersion)
			if err == nil {
				log.Info("engine bound", "version", v, "webapi", w)
				break
			}
			if qbittorrent.IsCode(err, qbittorrent.CodeVersion) {
				fatal <- fmt.Errorf("engine version %s (webapi %s) is not in supported_versions", v, w)
				return
			}
			if qbittorrent.IsCode(err, qbittorrent.CodeAuth) {
				log.Error("engine login failed; check secrets.json engine credentials")
			} else {
				log.Warn("engine not reachable yet", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
		svc.Run(ctx)
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(svc, &cfg, secrets.APITokens, log, version),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: file content responses may stream for a long time.
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "tls", cfg.TLSCertFile != "", "version", version)
		if cfg.TLSCertFile != "" {
			errc <- srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case err := <-fatal:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func newToken(args []string) {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "usage: bittorrent-server new-token <caller>")
		os.Exit(2)
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := "obt_" + base64.RawURLEncoding.EncodeToString(b)
	entry, _ := json.Marshal(config.APIToken{Caller: args[0], TokenSHA256: config.HashToken(token)})
	fmt.Printf("token: %s\nsecrets.json api_tokens entry: %s\n", token, entry)
}

// enginePasswordHash prints the WebUI\Password_PBKDF2 value qBittorrent
// expects: PBKDF2-HMAC-SHA512, 100000 iterations, 16-byte salt, 64-byte key.
func enginePasswordHash() {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr, "read password from stdin")
		os.Exit(2)
	}
	password := strings.TrimRight(line, "\r\n")
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key, err := pbkdf2.Key(sha512.New, password, salt, 100000, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("@ByteArray(%s:%s)\n", base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(key))
}
