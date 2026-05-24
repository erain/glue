// Command peggy-telegram is the binary form of the Telegram channel
// adapter. It loads Peggy's settings, constructs the agent, decodes
// the channel config, and runs the Telegram poll loop until SIGINT /
// SIGTERM.
//
// Usage:
//
//	export PEGGY_TELEGRAM_TOKEN=<BotFather token>
//	# Optional: ~/.config/peggy/settings.json with channels.telegram block
//	peggy-telegram
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/erain/glue/agents/peggy"
	"github.com/erain/glue/agents/peggy/channels/telegram"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(parent context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("peggy-telegram", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath  = fs.String("config", "", "path to settings.json (overrides $PEGGY_CONFIG / XDG / ~/.config/peggy)")
		soulPath    = fs.String("soul", "", "path to identity Markdown")
		showVersion = fs.Bool("version", false, "print version and exit")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `peggy-telegram — Peggy reachable on Telegram.

One-time setup:
  1. Talk to @BotFather on Telegram and create a bot. Save the token.
  2. Set PEGGY_TELEGRAM_TOKEN to that token (or whatever env var your
     settings.json names under channels.telegram.bot_token_env).
  3. Add your chat id to channels.telegram.allow_chats in settings.json.
     (Send any message to your bot once, then check the getUpdates JSON
     for your chat id.)

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, peggy.Version)
		return 0
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "peggy-telegram: positional args not supported (the channel takes input from Telegram)")
		return 2
	}

	settings, _, err := peggy.LoadSettings(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy-telegram: %v\n", err)
		return 1
	}
	soul, _, err := peggy.LoadSoul(*soulPath)
	if err != nil {
		fmt.Fprintf(stderr, "peggy-telegram: %v\n", err)
		return 1
	}

	cfg, err := telegram.DecodeConfig(settings.Channels[telegram.ChannelName])
	if err != nil {
		fmt.Fprintf(stderr, "peggy-telegram: %v\n", err)
		return 1
	}
	var permission *telegram.Permission
	if settings.Coding.Enabled {
		permission = telegram.NewPermission(telegram.PermissionOptions{Stderr: stderr})
	}

	p, err := peggy.New(peggy.Options{
		Settings:   settings,
		Soul:       soul,
		Stderr:     stderr,
		Permission: permission,
	})
	if err != nil {
		fmt.Fprintf(stderr, "peggy-telegram: setup: %v\n", err)
		return 1
	}
	defer p.Close()

	ch, err := telegram.New(telegram.Options{Peggy: p, Config: cfg, Stderr: stderr, Permission: permission})
	if err != nil {
		fmt.Fprintf(stderr, "peggy-telegram: %v\n", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintln(stderr, "peggy-telegram: listening for Telegram updates; SIGINT to stop.")
	if err := ch.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "peggy-telegram: %v\n", err)
		return 1
	}
	fmt.Fprintln(stderr, "peggy-telegram: stopped.")
	return 0
}
