package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/firmo-tecnologia/zappy-mcp/internal/zappymcp"
)

func main() {
	log.SetOutput(os.Stderr) // stdout belongs exclusively to MCP in serve mode.
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		help()
		return nil
	}
	command := os.Args[1]
	if command == "version" || command == "--version" {
		fmt.Println(zappymcp.Version)
		return nil
	}
	if command == "help" || command == "--help" {
		help()
		return nil
	}
	if command != "login" && command != "logout" && command != "listen" && command != "serve" && command != "status" {
		return fmt.Errorf("unknown command %q", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	accountID := flags.String("account", "", "Account UUID (default: last login)")
	var apiURL *string
	var port *int
	var noBrowser *bool
	if command == "login" {
		apiURL = flags.String("api-url", zappymcp.DefaultAPIURL, "Zappy API URL (HTTP only for loopback development)")
		port = flags.Int("callback-port", 18743, "OAuth loopback callback port (must be allowed by the OAuth client)")
		noBrowser = flags.Bool("no-browser", false, "Print login URL without launching a browser")
	}
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	root, err := zappymcp.HomeDir()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if command == "login" {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if err := zappymcp.Login(ctx, root, *apiURL, *port, !*noBrowser); err != nil {
			return err
		}
		log.Print("Zappy login complete. Start zappy-mcp listen or add zappy-mcp serve to your MCP client.")
		return nil
	}
	if *accountID == "" {
		*accountID, err = zappymcp.CurrentAccount(root)
		if err != nil {
			return err
		}
	}
	if command == "logout" {
		return zappymcp.Logout(ctx, root, *accountID)
	}
	store, err := zappymcp.NewStore(root, *accountID)
	if err != nil {
		return err
	}
	if command == "status" {
		var listener any
		if bytes, err := os.ReadFile(filepath.Join(store.Dir, "listener.json")); err == nil {
			_ = json.Unmarshal(bytes, &listener)
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"account_id": *accountID, "history_path": store.HistoryPath(), "listener": listener})
	}
	tokens, err := zappymcp.NewTokenManager(root, *accountID)
	if err != nil {
		return err
	}
	api := &zappymcp.API{Tokens: tokens}
	if command == "serve" {
		return zappymcp.Serve(ctx, api, store)
	}
	log.Printf("Listening for account %s; history: %s", *accountID, store.HistoryPath())
	(&zappymcp.Collector{API: api, Store: store}).Run(ctx)
	return nil
}
func help() {
	fmt.Fprintln(os.Stderr, "Zappy MCP "+zappymcp.Version+"\n\nCommands:\n  login    Browser OAuth login with PKCE\n  logout   Revoke the OAuth refresh token; preserve history\n  listen   Collect messages continuously into ~/.zappy/mcp\n  serve    MCP server over stdio (also starts a collector)\n  status   Show history path and listener status\n  version  Print version\n\nUse <command> --help for flags. All diagnostics go to stderr.")
}
